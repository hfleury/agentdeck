// Package daemon owns agentdeck's state: sessions, the tmux layout, hook
// events and history. The UI panes and hooks talk to it over the socket.
package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentdeck/internal/claude"
	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
	"agentdeck/internal/paths"
	"agentdeck/internal/store"
	"agentdeck/internal/tmux"
)

// Layout records the deck's pane ids.
type Layout struct {
	Sidebar     string `json:"sidebar"`
	Prompt      string `json:"prompt,omitempty"` // removed 2026-10-09; kept to migrate old decks
	Context     string `json:"context"`
	Placeholder string `json:"placeholder"` // the "no session" pane
	Center      string `json:"center"`      // pane currently in the center slot
}

// Daemon is the running state.
type Daemon struct {
	mu       sync.Mutex
	st       *store.Store
	tm       *tmux.Client
	self     string // path of the agentdeck binary
	settings string // hooks settings file for claude --settings

	sessions []*model.Session
	active   string
	layout   Layout
	focused  bool
	ttys     []string            // attached client terminals
	deckSent map[string]sent     // last prompt sent by the deck, per session
	hooked   map[string]bool     // sessions whose claude has sent SessionStart
	grants   map[string][]string // folders to /add-dir per session, when Claude's box is free

	subMu sync.Mutex
	subs  map[chan ipc.Event]struct{}
}

const (
	deckSession   = "deck"
	agentsSession = "agents"
)

// Run starts the daemon and serves until the listener fails.
func Run(self string) error {
	dir := paths.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		log.SetOutput(logf)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		return err
	}
	d := &Daemon{
		st:       st,
		tm:       &tmux.Client{Server: paths.TmuxServer()},
		self:     self,
		settings: filepath.Join(dir, "claude-hooks.json"),
		deckSent: map[string]sent{},
		hooked:   map[string]bool{},
		grants:   map[string][]string{},
		subs:     map[chan ipc.Event]struct{}{},
	}
	if err := claude.WriteHookSettings(d.settings, self); err != nil {
		return err
	}
	if err := d.load(); err != nil {
		return err
	}
	ln, err := listen()
	if err != nil {
		return err
	}
	log.Printf("daemon started, %d sessions", len(d.sessions))
	go d.paneLoop()
	go d.gitLoop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go d.serve(conn)
	}
}

func listen() (net.Listener, error) {
	sock := paths.Socket()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return nil, err
	}
	if c, err := net.DialTimeout("unix", sock, time.Second); err == nil {
		c.Close()
		return nil, errors.New("another agentdeck daemon is already running")
	}
	_ = os.Remove(sock)
	return net.Listen("unix", sock)
}

func (d *Daemon) load() error {
	list, err := d.st.Sessions()
	if err != nil {
		return err
	}
	for i := range list {
		s := list[i]
		d.sessions = append(d.sessions, &s)
	}
	d.sortSessions()
	d.active = d.st.Get("active")
	if v := d.st.Get("layout"); v != "" {
		_ = json.Unmarshal([]byte(v), &d.layout)
	}
	d.reconcilePanes(false)
	return nil
}

func (d *Daemon) find(id string) *model.Session {
	for _, s := range d.sessions {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func (d *Daemon) save(s *model.Session) {
	if err := d.st.SaveSession(*s); err != nil {
		log.Printf("save session %s: %v", s.ID, err)
	}
}

func (d *Daemon) saveLayout() {
	b, _ := json.Marshal(d.layout)
	_ = d.st.Set("layout", string(b))
}

// snapshot must be called with d.mu held.
func (d *Daemon) snapshot() model.State {
	st := model.State{Active: d.active, Focused: d.focused, Collapsed: d.collapsed()}
	for _, s := range d.sessions {
		st.Sessions = append(st.Sessions, *s)
	}
	return st
}

// changed pushes the current state to every subscriber. Call with d.mu held.
func (d *Daemon) changed() {
	d.publish("state", d.snapshot())
}

func (d *Daemon) publish(typ string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	ev := ipc.Event{Type: typ, Data: b}
	d.subMu.Lock()
	defer d.subMu.Unlock()
	for ch := range d.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop this event, the next state catches up
		}
	}
}

// serve handles one connection.
func (d *Daemon) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req ipc.Request
	if err := json.Unmarshal(line, &req); err != nil {
		writeResp(conn, nil, err)
		return
	}
	if req.Method == "subscribe" {
		d.subscribe(conn)
		return
	}
	res, err := d.handle(req)
	writeResp(conn, res, err)
}

func writeResp(conn net.Conn, res any, err error) {
	var resp ipc.Response
	if err != nil {
		resp.Error = err.Error()
	} else if res != nil {
		resp.Result, _ = json.Marshal(res)
	}
	b, _ := json.Marshal(resp)
	_, _ = conn.Write(append(b, '\n'))
}

func (d *Daemon) subscribe(conn net.Conn) {
	ch := make(chan ipc.Event, 256)
	d.mu.Lock()
	b, _ := json.Marshal(d.snapshot())
	d.mu.Unlock()
	ch <- ipc.Event{Type: "state", Data: b}
	d.subMu.Lock()
	d.subs[ch] = struct{}{}
	d.subMu.Unlock()
	defer func() {
		d.subMu.Lock()
		delete(d.subs, ch)
		d.subMu.Unlock()
	}()
	// Detect the client going away even when no events flow.
	gone := make(chan struct{})
	go func() {
		_, _ = conn.Read(make([]byte, 1))
		close(gone)
	}()
	for {
		select {
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Write(append(b, '\n')); err != nil {
				return
			}
		case <-gone:
			return
		}
	}
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	err := json.Unmarshal(raw, &v)
	return v, err
}

func (d *Daemon) handle(req ipc.Request) (any, error) {
	switch req.Method {
	case "ping":
		return "pong", nil
	case "state":
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.snapshot(), nil
	case "layout":
		p, _ := decode[struct{ W, H int }](req.Params)
		return nil, d.EnsureLayout(p.W, p.H)
	case "new":
		p, err := decode[ipc.NewParams](req.Params)
		if err != nil {
			return nil, err
		}
		return d.New(p.Dir, p.Branch, p.Model, p.Group)
	case "select":
		p, _ := decode[ipc.IDParams](req.Params)
		return nil, d.Select(p.ID)
	case "close":
		p, _ := decode[ipc.CloseParams](req.Params)
		return d.Close(p.ID, p.RemoveWorktree)
	case "restart":
		p, _ := decode[ipc.IDParams](req.Params)
		return nil, d.Restart(p.ID)
	case "send":
		p, err := decode[ipc.SendParams](req.Params)
		if err != nil {
			return nil, err
		}
		return d.Send(p.ID, p.Text, !p.NoHistory)
	case "interrupt":
		p, _ := decode[ipc.IDParams](req.Params)
		return nil, d.Interrupt(p.ID)
	case "review":
		p, _ := decode[ipc.IDParams](req.Params)
		return nil, d.Review(p.ID)
	case "rename":
		p, _ := decode[struct{ ID, Title string }](req.Params)
		return nil, d.Rename(p.ID, p.Title)
	case "picker":
		p, _ := decode[ipc.NewParams](req.Params)
		return nil, d.Picker(p.Dir, p.Group)
	case "set_group":
		p, _ := decode[struct{ ID, Group string }](req.Params)
		return nil, d.SetGroup(p.ID, p.Group)
	case "rename_group":
		p, _ := decode[struct{ Old, New string }](req.Params)
		return nil, d.RenameGroup(p.Old, p.New)
	case "move":
		p, _ := decode[struct {
			ID    string
			Delta int
		}](req.Params)
		return nil, d.Move(p.ID, p.Delta)
	case "collapse":
		p, _ := decode[struct {
			Group     string
			Collapsed bool
		}](req.Params)
		return nil, d.Collapse(p.Group, p.Collapsed)
	case "context":
		p, _ := decode[ipc.IDParams](req.Params)
		return d.ContextView(p.ID)
	case "context_add":
		p, err := decode[ipc.AddParams](req.Params)
		if err != nil {
			return nil, err
		}
		return d.AddItem(p)
	case "context_remove":
		p, _ := decode[ipc.ItemParams](req.Params)
		return nil, d.itemOp(p.ID, func(it model.ContextItem) error { return d.st.DeleteItem(it.ID) })
	case "context_pin":
		p, _ := decode[ipc.ItemParams](req.Params)
		return nil, d.itemOp(p.ID, func(it model.ContextItem) error { return d.st.SetPinned(it.ID, p.Pinned) })
	case "context_resend":
		p, _ := decode[ipc.ItemParams](req.Params)
		return nil, d.itemOp(p.ID, func(it model.ContextItem) error { return d.st.MarkSent(it.ID, "") })
	case "context_share":
		p, _ := decode[ipc.ShareParams](req.Params)
		return d.Share(p)
	case "show":
		p, _ := decode[ipc.ItemParams](req.Params)
		return nil, d.ShowItem(p.ID)
	case "show_tools":
		p, _ := decode[ipc.ShareParams](req.Params) // To = session, ID = call
		return nil, d.ShowTools(p.To, p.ID)
	case "add_picker":
		p, _ := decode[ipc.IDParams](req.Params)
		return nil, d.AddPicker(p.ID)
	case "context_preview":
		p, _ := decode[ipc.ItemParams](req.Params)
		return d.Preview(p.ID)
	case "budget":
		p, _ := decode[struct {
			Session string `json:"session"`
			Budget  int    `json:"budget"`
		}](req.Params)
		return nil, d.SetBudget(p.Session, p.Budget)
	case "tools":
		p, _ := decode[ipc.HistoryParams](req.Params)
		if p.Limit <= 0 {
			p.Limit = 200
		}
		return d.st.ToolCalls(p.Session, p.Limit)
	case "tool":
		p, _ := decode[ipc.ItemParams](req.Params)
		return d.st.ToolCall(p.ID)
	case "insert":
		p, _ := decode[ipc.SendParams](req.Params)
		return nil, d.Insert(p.Text)
	case "palette":
		return nil, d.Palette()
	case "recent":
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.recentDirs(), nil
	case "relayout":
		d.Relayout()
		return nil, nil
	case "focus":
		p, _ := decode[ipc.FocusParams](req.Params)
		return nil, d.Focus(p.Pane)
	case "history":
		p, _ := decode[ipc.HistoryParams](req.Params)
		return d.st.History(p.Session, p.Limit)
	case "hook":
		p, err := decode[ipc.HookParams](req.Params)
		if err != nil {
			return nil, err
		}
		return d.Hook(p), nil
	}
	return nil, fmt.Errorf("unknown method %q", req.Method)
}

// resolve returns the session for id, or the active one when id is "".
// Call with d.mu held.
func (d *Daemon) resolve(id string) (*model.Session, error) {
	if id == "" {
		id = d.active
	}
	if id == "" {
		return nil, errors.New("no session selected — start one with /new or n in the sessions list")
	}
	s := d.find(id)
	if s == nil {
		return nil, fmt.Errorf("no session %s", id)
	}
	return s, nil
}

func shortTitle(s string) string {
	s = claude.CleanPrompt(s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return strings.TrimSpace(s)
}
