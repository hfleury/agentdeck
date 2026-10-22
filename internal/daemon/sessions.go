package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"agentdeck/internal/claude"
	"agentdeck/internal/gitx"
	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
	"agentdeck/internal/paths"
	"agentdeck/internal/route"
	"agentdeck/internal/tmux"
)

// sideWidths returns the sessions and context pane widths for a window
// width: a share of the window, within limits, so the center stays wide.
func sideWidths(w int) (sidebar, context int) {
	clamp := func(v, lo, hi int) int { return min(max(v, lo), hi) }
	return clamp(w*21/100, 24, 34), clamp(w*19/100, 22, 32)
}

// EnsureLayout creates the deck session with its three panes (sessions,
// Claude, context), or repairs it when a pane is missing. w and h are the terminal size, used for the
// initial window size.
func (d *Daemon) EnsureLayout(w, h int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	panes, _ := d.tm.Panes()
	// Decks from before 2026-10-09 had a prompt pane at the bottom; you type
	// in Claude's own box now.
	if d.layout.Prompt != "" {
		if _, live := panes[d.layout.Prompt]; live {
			_, _ = d.tm.Run("kill-pane", "-t", d.layout.Prompt)
			delete(panes, d.layout.Prompt)
		}
		d.layout.Prompt = ""
		d.saveLayout()
		d.bindKeys()
	}
	ok := d.tm.HasSession(deckSession)
	for _, id := range []string{d.layout.Sidebar, d.layout.Context, d.layout.Placeholder, d.layout.Center} {
		if _, live := panes[id]; !live {
			ok = false
		}
	}
	if ok {
		d.relayoutLocked()
		return nil
	}
	if d.tm.HasSession(deckSession) {
		// Move agent panes out of the broken deck before rebuilding it,
		// so no Claude process is killed.
		for _, s := range d.sessions {
			if p, live := panes[s.Pane]; live && p.Session == deckSession {
				if _, err := d.tm.Run("break-pane", "-d", "-s", s.Pane, "-t", agentsSession+":"); err != nil {
					log.Printf("rescue pane %s: %v", s.Pane, err)
				}
			}
		}
		_, _ = d.tm.Run("kill-session", "-t", "="+deckSession)
	}
	if w < 80 {
		w = 200
	}
	if h < 20 {
		h = 50
	}
	ui := func(role string) string { return tmux.Quote(d.self) + " ui " + role }
	var err error
	var l Layout
	if l.Placeholder, err = d.tm.Run("new-session", "-d", "-s", deckSession, "-n", "deck",
		"-x", fmt.Sprint(w), "-y", fmt.Sprint(h), "-P", "-F", "#{pane_id}", ui("empty")); err != nil {
		return err
	}
	sw, cw := sideWidths(w)
	if l.Sidebar, err = d.tm.Run("split-window", "-h", "-b", "-l", fmt.Sprint(sw), "-t", l.Placeholder, "-P", "-F", "#{pane_id}", ui("sidebar")); err != nil {
		return err
	}
	if l.Context, err = d.tm.Run("split-window", "-h", "-l", fmt.Sprint(cw), "-t", l.Placeholder, "-P", "-F", "#{pane_id}", ui("context")); err != nil {
		return err
	}
	l.Center = l.Placeholder
	d.layout = l
	d.saveLayout()
	// Show the active session again, if it is still running.
	if s := d.find(d.active); s != nil && s.Pane != "" {
		if _, live := panes[s.Pane]; live {
			_ = d.swapIn(s.Pane)
		}
	}
	d.bindKeys()
	// Keep the side panes proportional when the terminal is resized.
	hook := "run-shell -b " + tmux.Quote(tmux.Quote(d.self)+" relayout")
	if _, err := d.tm.Run("set-hook", "-t", deckSession, "window-resized", hook); err != nil {
		log.Printf("set-hook: %v", err)
	}
	_, _ = d.tm.Run("select-pane", "-t", d.layout.Center)
	return nil
}

// Relayout resizes the side panes for the current window width.
func (d *Daemon) Relayout() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.relayoutLocked()
}

func (d *Daemon) relayoutLocked() {
	out, err := d.tm.Run("display-message", "-p", "-t", d.layout.Sidebar, "#{window_width}")
	if err != nil {
		return
	}
	sw, cw := sideWidths(atoi(out))
	_, _ = d.tm.Run("resize-pane", "-t", d.layout.Sidebar, "-x", fmt.Sprint(sw))
	_, _ = d.tm.Run("resize-pane", "-t", d.layout.Context, "-x", fmt.Sprint(cw))
}

// bindKeys sets the root-table keys that need pane ids: Ctrl-A and
// Ctrl-G go to the sessions list and context and back to Claude,
// Ctrl-Space goes to Claude. Call with d.mu held.
func (d *Daemon) bindKeys() {
	l := d.layout
	toggle := func(key, pane string) {
		_, err := d.tm.Run("bind-key", "-n", key, "if-shell", "-F", "#{==:#{pane_id},"+pane+"}",
			"select-pane -t "+l.Center, "select-pane -t "+pane)
		if err != nil {
			log.Printf("bind %s: %v", key, err)
		}
	}
	toggle("C-a", l.Sidebar)
	toggle("C-g", l.Context)
	if _, err := d.tm.Run("bind-key", "-n", "C-Space", "select-pane", "-t", l.Center); err != nil {
		log.Printf("bind C-Space: %v", err)
	}
	if _, err := d.tm.Run("bind-key", "-n", "M-k", "display-popup", "-E", "-w", "80", "-h", "22",
		"-T", " agentdeck ", tmux.Quote(d.self)+" palette"); err != nil {
		log.Printf("bind M-k: %v", err)
	}
}

// Insert puts text into the active session's input box without sending
// it, and moves the keyboard there.
func (d *Daemon) Insert(text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := d.resolve("")
	if err != nil {
		return err
	}
	if s.Pane == "" || s.State == model.StateExited {
		return errors.New("this session has exited — restart it with R")
	}
	if err := d.tm.Paste(s.Pane, text); err != nil {
		return err
	}
	_, err = d.tm.Run("select-pane", "-t", d.layout.Center)
	return err
}

// Palette opens the command window (also bound to Alt-K in tmux.conf).
func (d *Daemon) Palette() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.popup(" agentdeck ", "", 80, 22, tmux.Quote(d.self)+" palette")
}

// swapIn shows pane in the center slot. Call with d.mu held.
func (d *Daemon) swapIn(pane string) error {
	if pane == d.layout.Center {
		return nil
	}
	if _, err := d.tm.Run("swap-pane", "-d", "-s", pane, "-t", d.layout.Center); err != nil {
		return err
	}
	d.layout.Center = pane
	d.saveLayout()
	d.bindKeys()
	return nil
}

// New starts a session in dir; with branch, in a new worktree of dir's repo.
func (d *Daemon) New(dir, branch, mdl, group string) (*model.Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.newLocked(dir, branch, mdl, group)
}

func (d *Daemon) newLocked(dir, branch, mdl, group string) (*model.Session, error) {
	if dir == "" {
		dir = paths.Home()
	}
	dir = paths.Expand(dir)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", paths.Tilde(dir))
	}
	s := &model.Session{
		ID:        uuid.NewString(),
		Cwd:       dir,
		OriginDir: dir,
		Model:     mdl,
		Group:     strings.TrimSpace(group),
		Order:     d.nextOrder(),
		State:     model.StateIdle,
		CreatedAt: time.Now().Unix(),
	}
	s.ClaudeID = s.ID
	if branch != "" {
		repo := gitx.Toplevel(dir)
		if repo == "" {
			return nil, fmt.Errorf("%s is not in a git repository", paths.Tilde(dir))
		}
		wt, err := gitx.AddWorktree(repo, branch)
		if err != nil {
			return nil, err
		}
		// The same subfolder in the worktree, if git has it there (an
		// untracked folder doesn't exist in a new worktree).
		rel, _ := filepath.Rel(repo, dir)
		s.Cwd = filepath.Join(wt, rel)
		if fi, err := os.Stat(s.Cwd); err != nil || !fi.IsDir() {
			s.Cwd = wt
		}
		s.Worktree, s.Branch = wt, branch
	}
	if err := d.startPane(s, false); err != nil {
		return nil, err
	}
	s.Git = gitx.Status(s.Cwd)
	d.rememberDir(s.OriginDir)
	d.sessions = append(d.sessions, s)
	d.save(s)
	if err := d.selectLocked(s.ID); err != nil {
		log.Printf("select new session: %v", err)
	}
	return s, nil
}

// startPane runs claude for s, in its existing pane (respawn) or a new
// window of the hidden agents session. Call with d.mu held.
func (d *Daemon) startPane(s *model.Session, resume bool) error {
	start := claude.Start{ID: s.ID, Model: s.Model, Settings: d.settings, AddDirs: d.outsideFolders(s)}
	delete(d.grants, s.ID) // --add-dir covers them
	if resume {
		start.ResumeID = s.ClaudeID
	}
	cmd := claude.StartCommand(start)
	delete(d.hooked, s.ID)
	env := map[string]string{"AGENTDECK_SESSION": s.ID, "AGENTDECK_SOCK": paths.Socket()}
	if s.Pane != "" {
		args := []string{"respawn-pane", "-k", "-t", s.Pane, "-c", s.Cwd}
		for k, v := range env {
			args = append(args, "-e", k+"="+v)
		}
		if _, err := d.tm.Run(append(args, cmd)...); err == nil {
			return nil
		}
		s.Pane = "" // pane is gone; fall through to a new window
	}
	pane, err := d.tm.NewWindow(agentsSession, s.ID[:8], s.Cwd, env, cmd)
	if err != nil {
		return err
	}
	s.Pane = pane
	// Keep the pane after claude exits, so its last screen stays visible
	// and restart can reuse it.
	if _, err := d.tm.Run("set-option", "-p", "-t", pane, "remain-on-exit", "on"); err != nil {
		log.Printf("remain-on-exit: %v", err)
	}
	return nil
}

// Select shows a session in the center pane.
func (d *Daemon) Select(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.selectLocked(id)
}

func (d *Daemon) selectLocked(id string) error {
	s := d.find(id)
	if s == nil {
		return fmt.Errorf("no session %s", id)
	}
	if s.Pane != "" && d.layout.Center != "" {
		if err := d.swapIn(s.Pane); err != nil {
			log.Printf("swap in %s: %v", s.Pane, err)
		}
	}
	d.active = id
	_ = d.st.Set("active", id)
	if s.Unread {
		s.Unread = false
		d.save(s)
	}
	d.changed()
	return nil
}

// Close ends a session. A worktree is removed only when asked and clean.
func (d *Daemon) Close(id string, removeWorktree bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closeLocked(id, removeWorktree)
}

func (d *Daemon) closeLocked(id string, removeWorktree bool) (string, error) {
	s, err := d.resolve(id)
	if err != nil {
		return "", err
	}
	idx := -1
	for i, x := range d.sessions {
		if x == s {
			idx = i
		}
	}
	if s.Pane != "" {
		if s.Pane == d.layout.Center {
			if err := d.swapIn(d.layout.Placeholder); err != nil {
				log.Printf("swap placeholder back: %v", err)
			}
		}
		_, _ = d.tm.Run("kill-pane", "-t", s.Pane)
	}
	notice := ""
	if s.Worktree != "" {
		switch {
		case !removeWorktree:
			notice = "worktree kept: " + paths.Tilde(s.Worktree)
		case gitx.Dirty(s.Worktree):
			notice = "worktree kept, it has uncommitted changes: " + paths.Tilde(s.Worktree)
		default:
			if err := gitx.RemoveWorktree(gitx.Toplevel(s.OriginDir), s.Worktree); err != nil {
				notice = "worktree not removed: " + err.Error()
			} else {
				notice = "worktree removed (branch " + s.Branch + " kept)"
			}
		}
	}
	d.sessions = append(d.sessions[:idx], d.sessions[idx+1:]...)
	_ = d.st.DeleteSession(s.ID)
	_ = d.st.DeleteItems(s.ID)
	delete(d.grants, s.ID)
	delete(d.deckSent, s.ID)
	if d.active == s.ID {
		d.active = ""
		_ = d.st.Set("active", "")
		if len(d.sessions) > 0 {
			next := d.sessions[min(idx, len(d.sessions)-1)]
			_ = d.selectLocked(next.ID)
		}
	}
	d.changed()
	if notice != "" {
		d.publish("notice", notice)
	}
	return notice, nil
}

// Restart starts claude again for a session, resuming its conversation
// when Claude saved one.
func (d *Daemon) Restart(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.restartLocked(id)
}

func (d *Daemon) restartLocked(id string) error {
	s, err := d.resolve(id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(s.Cwd); err != nil {
		return fmt.Errorf("folder %s is gone", paths.Tilde(s.Cwd))
	}
	resume := claude.TranscriptExists(s.ClaudeID)
	if !resume {
		s.ClaudeID = s.ID
	}
	hadPane := s.Pane != ""
	if err := d.startPane(s, resume); err != nil {
		return err
	}
	s.State, s.Tool, s.ExitCode = model.StateIdle, "", 0
	if resume && s.Title != "" {
		s.State = model.StateDone
	}
	d.save(s)
	if !hadPane && d.active == s.ID {
		_ = d.swapIn(s.Pane)
	}
	d.changed()
	return nil
}

// Rename sets a session's title.
// Rename gives a session your own name; "" goes back to the automatic
// title (the last prompt).
func (d *Daemon) Rename(id, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := d.resolve(id)
	if err != nil {
		return err
	}
	s.Name = shortTitle(name)
	d.save(s)
	d.changed()
	return nil
}

// Interrupt stops Claude's current turn (Esc in Claude).
func (d *Daemon) Interrupt(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, err := d.resolve(id)
	if err != nil {
		return err
	}
	return d.tm.SendKeys(s.Pane, "Escape")
}

// Focus moves the keyboard to one of the deck's panes.
func (d *Daemon) Focus(which string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	pane := map[string]string{
		"center":  d.layout.Center,
		"sidebar": d.layout.Sidebar, "context": d.layout.Context,
	}[which]
	if pane == "" {
		return fmt.Errorf("unknown pane %q", which)
	}
	_, err := d.tm.Run("select-pane", "-t", pane)
	return err
}

// Review opens the diff of a session's uncommitted changes in a popup.
func (d *Daemon) Review(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reviewLocked(id)
}

func (d *Daemon) reviewLocked(id string) error {
	s, err := d.resolve(id)
	if err != nil {
		return err
	}
	return d.popup(" changes · "+paths.Tilde(s.OriginDir)+" ", s.Cwd, 0, 0,
		tmux.Quote(d.self)+" review "+tmux.Quote(s.Cwd)+" "+tmux.Quote(s.ID))
}

// popup opens a tmux popup on the terminal showing the deck. It returns
// once the popup is open; the popup runs on its own (it calls back over
// the socket), so the daemon never waits on it. w and h are cells; 0
// means 90% of the terminal. Call with d.mu held.
func (d *Daemon) popup(title, dir string, w, h int, cmd string) error {
	out, err := d.tm.Run("list-clients", "-t", deckSession, "-F", "#{client_name}\t#{client_width}\t#{client_height}")
	if err != nil || out == "" {
		return errors.New("no terminal attached to the deck")
	}
	f := strings.Split(strings.SplitN(out, "\n", 2)[0], "\t")
	cw, ch := atoi(f[1]), atoi(f[2])
	size := func(want, have int) string {
		if want <= 0 || want > have-2 {
			return fmt.Sprint(max(have*9/10, 10))
		}
		return fmt.Sprint(want)
	}
	args := []string{"display-popup", "-c", f[0], "-E", "-w", size(w, cw), "-h", size(h, ch), "-T", title}
	if dir != "" {
		args = append(args, "-d", dir)
	}
	args = append(args, cmd)
	go func() {
		if _, err := d.tm.Run(args...); err != nil {
			log.Printf("popup: %v", err)
		}
	}()
	return nil
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// Picker opens the new-session window, starting in dir.
func (d *Daemon) Picker(dir, group string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pickerLocked(dir, group)
}

// pickerLocked opens the new-session window; the session it starts joins
// group. Call with d.mu held.
func (d *Daemon) pickerLocked(dir, group string) error {
	if dir == "" {
		if s := d.find(d.active); s != nil {
			dir = s.OriginDir
		} else if r := d.recentDirs(); len(r) > 0 {
			dir = r[0]
		} else {
			dir = paths.Home()
		}
	}
	cmd := tmux.Quote(d.self) + " pick "
	if group != "" {
		cmd += "--group " + tmux.Quote(group) + " "
	}
	return d.popup(" New session ", "", 76, 22, cmd+tmux.Quote(dir))
}

// recentDirs lists folders sessions were started in, newest first.
// Call with d.mu held.
func (d *Daemon) recentDirs() []string {
	var list []string
	_ = json.Unmarshal([]byte(d.st.Get("recent_dirs")), &list)
	return list
}

// rememberDir puts dir at the front of the recent folders. Call with d.mu held.
func (d *Daemon) rememberDir(dir string) {
	list := []string{dir}
	for _, x := range d.recentDirs() {
		if x != dir && len(list) < 20 {
			list = append(list, x)
		}
	}
	b, _ := json.Marshal(list)
	_ = d.st.Set("recent_dirs", string(b))
}

// Send routes prompt text: a deck command runs here, a Claude slash command
// is typed into Claude, anything else is pasted as one message.
func (d *Daemon) Send(id, text string, saveHistory bool) (ipc.SendResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := route.Classify(text, claude.IsBuiltin)
	if r.Kind == route.Empty {
		return ipc.SendResult{Kind: string(r.Kind)}, nil
	}
	if r.Kind == route.App {
		notice, err := d.runApp(id, r)
		if err != nil {
			return ipc.SendResult{}, err
		}
		if saveHistory {
			d.addHistory(d.active, route.Normalize(text), "deck")
		}
		return ipc.SendResult{Kind: string(r.Kind), Notice: notice}, nil
	}
	s, err := d.resolve(id)
	if err != nil {
		return ipc.SendResult{}, err
	}
	if s.State == model.StateExited || s.Pane == "" {
		return ipc.SendResult{}, errors.New("this session has exited — restart it with R in the sessions list or /restart")
	}
	if saveHistory {
		d.addHistory(s.ID, r.Text, "deck")
	}
	d.deckSent[s.ID] = sent{text: r.Text, at: time.Now()}
	switch r.Kind {
	case route.Claude:
		err = d.typeCommand(s.Pane, r.Text)
	default:
		// The title is the last message you sent; commands don't change it.
		if err = d.pasteText(s.Pane, r.Text); err == nil {
			s.Title = shortTitle(r.Text)
			d.save(s)
		}
	}
	if err != nil {
		return ipc.SendResult{}, err
	}
	d.changed()
	return ipc.SendResult{Kind: string(r.Kind)}, nil
}

// typeCommand types a slash command so Claude parses it as one (a paste
// can be taken as plain text). Extra lines are joined with "\"+Enter, which
// is a new line in Claude's input box.
func (d *Daemon) typeCommand(pane, text string) error {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			if err := d.tm.SendText(pane, line); err != nil {
				return err
			}
		}
		if i < len(lines)-1 {
			if err := d.tm.SendText(pane, `\`); err != nil {
				return err
			}
			if err := d.tm.SendKeys(pane, "Enter"); err != nil {
				return err
			}
		}
	}
	time.Sleep(80 * time.Millisecond)
	return d.tm.SendKeys(pane, "Enter")
}

// pasteText sends text as one bracketed paste, then Enter.
func (d *Daemon) pasteText(pane, text string) error {
	if err := d.tm.Paste(pane, text); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond)
	return d.tm.SendKeys(pane, "Enter")
}

// runApp executes a deck command. Call with d.mu held.
func (d *Daemon) runApp(id string, r route.Route) (string, error) {
	cur := d.find(id)
	if cur == nil {
		cur = d.find(d.active)
	}
	switch r.Name {
	case "new":
		group := ""
		if cur != nil {
			group = cur.Group // a new session joins the group you are in
		}
		if r.Args == "" {
			return "", d.pickerLocked("", group) // ask for the folder in a window
		}
		s, err := d.newLocked(r.Args, "", "", group)
		if err != nil {
			return "", err
		}
		return "started in " + paths.Tilde(s.Cwd), nil
	case "worktree":
		if r.Args == "" {
			return "", errors.New("usage: /worktree <branch>")
		}
		if cur == nil {
			return "", errors.New("select a session in the repository first, or use n then w in the sessions list")
		}
		s, err := d.newLocked(cur.OriginDir, r.Args, cur.Model, cur.Group)
		if err != nil {
			return "", err
		}
		return "worktree " + paths.Tilde(s.Worktree), nil
	case "close":
		if cur == nil {
			return "", errors.New("no session selected")
		}
		return d.closeLocked(cur.ID, true)
	case "restart":
		if cur == nil {
			return "", errors.New("no session selected")
		}
		return "", d.restartLocked(cur.ID)
	case "changes":
		if cur == nil {
			return "", errors.New("no session selected")
		}
		return "", d.reviewLocked(cur.ID)
	case "title":
		if cur == nil {
			return "", errors.New("no session selected")
		}
		cur.Name = shortTitle(r.Args) // no text: back to the automatic title
		d.save(cur)
		d.changed()
		return "", nil
	case "group":
		if cur == nil {
			return "", errors.New("no session selected")
		}
		d.setGroupLocked(cur, r.Args)
		if r.Args == "" {
			return "removed from its group", nil
		}
		return "in group " + strings.TrimSpace(r.Args), nil
	}
	return "", fmt.Errorf("unknown command /%s", r.Name)
}

// addHistory stores a prompt and tells the prompt panes. Call with d.mu held.
func (d *Daemon) addHistory(sessionID, text, source string) {
	if text == "" {
		return
	}
	e, added, err := d.st.AddHistory(sessionID, text, source)
	if err != nil {
		log.Printf("history: %v", err)
		return
	}
	if added {
		d.publish("history", e)
	}
}
