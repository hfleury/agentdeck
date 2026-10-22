package daemon

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentdeck/internal/contextkit"
	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
	"agentdeck/internal/paths"
	"agentdeck/internal/tmux"
)

// lockedEnv lets contextkit look up sessions while d.mu is held.
type lockedEnv struct{ d *Daemon }

func (e lockedEnv) Session(id string) (model.Session, bool) {
	if s := e.d.find(id); s != nil {
		return *s, true
	}
	return model.Session{}, false
}

// budget is a session's token budget. Call with d.mu held.
func (d *Daemon) budget(sessionID string) int {
	if n, err := strconv.Atoi(d.st.Get("budget:" + sessionID)); err == nil && n > 0 {
		return n
	}
	return contextkit.DefaultBudget
}

// SetBudget changes a session's token budget.
func (d *Daemon) SetBudget(sessionID string, n int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := d.resolve(sessionID)
	if err != nil {
		return err
	}
	if n < 500 {
		return errors.New("the budget must be at least 500 tokens")
	}
	if err := d.st.Set("budget:"+s.ID, strconv.Itoa(n)); err != nil {
		return err
	}
	d.publish("context", s.ID)
	return nil
}

// ContextView is the context panel of a session ("" = active).
func (d *Daemon) ContextView(sessionID string) (model.ContextView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := d.resolve(sessionID)
	if err != nil {
		return model.ContextView{}, err
	}
	items, err := d.st.Items(s.ID)
	if err != nil {
		return model.ContextView{}, err
	}
	env := lockedEnv{d}
	v := model.ContextView{SessionID: s.ID, Budget: d.budget(s.ID)}
	plan := contextkit.Select(items, env, v.Budget)
	going := map[int64]contextkit.Rendered{}
	for _, r := range plan.Send {
		going[r.Item.ID] = r
		v.Next += r.Tokens
	}
	for _, it := range items {
		r, ok := going[it.ID]
		if !ok {
			r = contextkit.Render(it, env, 0)
		}
		iv := model.ItemView{ContextItem: it, Label: r.Label, Tokens: r.Tokens, Pending: ok, Error: r.Err}
		if !ok && r.Pending() {
			iv.Error = "over the budget"
		}
		v.Items = append(v.Items, iv)
	}
	return v, nil
}

// AddItem attaches something to a session.
func (d *Daemon) AddItem(p ipc.AddParams) (model.ContextItem, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := d.resolve(p.Session)
	if err != nil {
		return model.ContextItem{}, err
	}
	it := model.ContextItem{SessionID: s.ID, Kind: p.Kind, Ref: p.Ref, Payload: p.Payload, Title: p.Title, Pinned: p.Pinned}
	switch p.Kind {
	case model.KindFile, model.KindFolder, model.KindLines:
		if it.Ref == "" {
			return it, errors.New("no path given")
		}
		if !filepath.IsAbs(it.Ref) && !strings.HasPrefix(it.Ref, "~") {
			it.Ref = filepath.Join(s.Cwd, it.Ref)
		}
		it.Ref = paths.Expand(it.Ref)
		fi, err := os.Stat(it.Ref)
		if err != nil {
			return it, fmt.Errorf("%s not found", paths.Tilde(it.Ref))
		}
		if fi.IsDir() {
			if p.Kind == model.KindLines {
				return it, errors.New("line ranges need a file")
			}
			it.Kind = model.KindFolder
		} else if p.Kind == model.KindFolder {
			it.Kind = model.KindFile
		}
		if it.Kind == model.KindLines {
			if _, _, err := contextkit.ParseRange(it.Payload); err != nil {
				return it, err
			}
		}
	case model.KindNote:
		if strings.TrimSpace(it.Payload) == "" {
			return it, errors.New("the note is empty")
		}
	case model.KindConversation, model.KindMemory, model.KindResult:
		other := d.find(it.Ref)
		if other == nil {
			return it, errors.New("no such session")
		}
		if other.ID == s.ID {
			return it, errors.New("choose another session")
		}
		it.Title = other.DisplayTitle()
	case model.KindToolOutput:
		if it.Payload == "" {
			return it, errors.New("the tool call has no output")
		}
	default:
		return it, fmt.Errorf("unknown kind %q", p.Kind)
	}
	it, err = d.st.AddItem(it)
	if err != nil {
		return it, err
	}
	if it.Kind == model.KindFolder && !within(it.Ref, s.Cwd) {
		d.grants[s.ID] = append(d.grants[s.ID], it.Ref)
	}
	d.publish("context", s.ID)
	return it, nil
}

// itemOp runs op on an item and tells the panel.
func (d *Daemon) itemOp(id int64, op func(model.ContextItem) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	it, err := d.st.Item(id)
	if err != nil {
		return errors.New("no such item")
	}
	if err := op(it); err != nil {
		return err
	}
	d.publish("context", it.SessionID)
	return nil
}

// Share copies one item, or all items of a session, to another session.
// The copies go with that session's next prompt.
func (d *Daemon) Share(p ipc.ShareParams) (ipc.SendResult, error) {
	n, err := d.share(p)
	return ipc.SendResult{Notice: n}, err
}

func (d *Daemon) share(p ipc.ShareParams) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	to := d.find(p.To)
	if to == nil {
		return "", errors.New("no such session")
	}
	var items []model.ContextItem
	if p.ID != 0 {
		it, err := d.st.Item(p.ID)
		if err != nil {
			return "", errors.New("no such item")
		}
		items = []model.ContextItem{it}
	} else {
		var err error
		if items, err = d.st.Items(p.From); err != nil {
			return "", err
		}
	}
	n := 0
	for _, it := range items {
		if (it.Kind == model.KindConversation || it.Kind == model.KindMemory || it.Kind == model.KindResult) && it.Ref == to.ID {
			continue // a session doesn't need its own conversation
		}
		it.SessionID = to.ID
		if _, err := d.st.AddItem(it); err != nil {
			return "", err
		}
		if it.Kind == model.KindFolder && !within(it.Ref, to.Cwd) {
			d.grants[to.ID] = append(d.grants[to.ID], it.Ref)
		}
		n++
	}
	d.publish("context", to.ID)
	title := to.DisplayTitle()
	return fmt.Sprintf("%d item(s) shared with %s", n, title), nil
}

// ShowItem opens an item's text in a popup.
func (d *Daemon) ShowItem(id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.popup(" context ", "", 0, 0, tmux.Quote(d.self)+" show "+strconv.FormatInt(id, 10))
}

// ShowTools opens the tool-call browser of a session at a call (0 = newest).
func (d *Daemon) ShowTools(sessionID string, id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := d.resolve(sessionID)
	if err != nil {
		return err
	}
	return d.popup(" tool calls ", "", 0, 0, tmux.Quote(d.self)+" tools "+tmux.Quote(s.ID)+" "+strconv.FormatInt(id, 10))
}

// AddPicker opens the file and folder window to add to a session.
func (d *Daemon) AddPicker(sessionID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := d.resolve(sessionID)
	if err != nil {
		return err
	}
	return d.popup(" Add to context ", "", 76, 22, tmux.Quote(d.self)+" pick --add "+tmux.Quote(s.ID)+" "+tmux.Quote(s.Cwd))
}

// Preview is the text an item gives Claude.
func (d *Daemon) Preview(id int64) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	it, err := d.st.Item(id)
	if err != nil {
		return "", errors.New("no such item")
	}
	r := contextkit.Render(it, lockedEnv{d}, 0)
	if r.Err != "" {
		return "", errors.New(r.Err)
	}
	return r.Text, nil
}

// takeContext picks the session's pending items for the prompt being
// submitted, marks them sent, and returns the text. Call with d.mu held.
func (d *Daemon) takeContext(sessionID string) string {
	items, err := d.st.Items(sessionID)
	if err != nil || len(items) == 0 {
		return ""
	}
	budget := d.budget(sessionID)
	plan := contextkit.Select(items, lockedEnv{d}, budget)
	for _, r := range plan.Send {
		if err := d.st.MarkSent(r.Item.ID, r.Hash); err != nil {
			log.Printf("mark sent: %v", err)
		}
	}
	if len(plan.Send) > 0 || len(plan.Omitted) > 0 {
		d.publish("context", sessionID)
	}
	return contextkit.Wrap(plan, budget)
}

// outsideFolders are the folder items outside a session's own folder,
// passed as --add-dir when it starts. Call with d.mu held.
func (d *Daemon) outsideFolders(s *model.Session) []string {
	items, _ := d.st.Items(s.ID)
	var out []string
	seen := map[string]bool{}
	for _, it := range items {
		if it.Kind == model.KindFolder && !within(it.Ref, s.Cwd) && !seen[it.Ref] {
			seen[it.Ref] = true
			out = append(out, it.Ref)
		}
	}
	return out
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// grantDirs types /add-dir into sessions that have folders waiting, but
// only while Claude is waiting and its input box is empty, so it never
// mixes with what you are typing. Call with d.mu held.
func (d *Daemon) grantDirs() {
	for id, dirs := range d.grants {
		s := d.find(id)
		if s == nil || len(dirs) == 0 {
			delete(d.grants, id)
			continue
		}
		// Claude's trust question and other dialogs never look like an empty
		// input box, so checking the box is enough.
		if s.Pane == "" || (s.State != model.StateIdle && s.State != model.StateDone) {
			continue
		}
		screen, err := d.tm.Run("capture-pane", "-p", "-t", s.Pane)
		if err != nil || !inputEmpty(screen) {
			continue
		}
		dir := dirs[0]
		d.grants[id] = dirs[1:]
		log.Printf("granting %s to %s", dir, s.ID)
		go d.addDir(s.Pane, dir) // waits for Claude's question: not under the lock
		return                   // one at a time: let Claude handle it first
	}
}

// addDir types /add-dir and answers Claude's "Add directory to
// workspace" question with its first option, "Yes, for this session" (the
// same as --add-dir on restart).
func (d *Daemon) addDir(pane, dir string) {
	if err := d.typeCommand(pane, "/add-dir "+dir); err != nil {
		log.Printf("add-dir: %v", err)
		return
	}
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		screen, err := d.tm.Run("capture-pane", "-p", "-t", pane)
		if err != nil {
			return
		}
		if strings.Contains(screen, "Add directory to workspace") {
			if !strings.Contains(screen, "❯ 1. Yes, for this session") {
				log.Printf("add-dir: unexpected question, left for you:\n%s", screen)
				return
			}
			_ = d.tm.SendKeys(pane, "Enter")
			return
		}
		if strings.Contains(screen, "as a working directory") {
			return // added without asking
		}
	}
}

// inputEmpty reports whether Claude's input box (the last line starting
// with ❯ or >) has nothing typed in it.
func inputEmpty(screen string) bool {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		for _, p := range []string{"❯", ">"} {
			if strings.HasPrefix(l, p) {
				return strings.TrimSpace(strings.TrimPrefix(l, p)) == ""
			}
		}
	}
	return false
}
