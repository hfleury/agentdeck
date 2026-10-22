package daemon

import (
	"encoding/json"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"agentdeck/internal/claude"
	"agentdeck/internal/gitx"
	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
	"agentdeck/internal/notify"
	"agentdeck/internal/paths"
	"agentdeck/internal/tmux"
)

// deckEcho is how long after the deck sends a prompt the matching
// UserPromptSubmit hook is treated as that same prompt.
const deckEcho = 30 * time.Second

// sent is a prompt the deck sent, to recognize its UserPromptSubmit echo.
type sent struct {
	text string
	at   time.Time
}

// matches reports whether a submitted prompt is this one coming back.
// Claude may wrap pasted text in tags, so both sides are compared with
// tags and whitespace removed.
func (s sent) matches(prompt string) bool {
	if s.text == "" || time.Since(s.at) > deckEcho {
		return false
	}
	norm := func(x string) string { return strings.Join(strings.Fields(stripTags(x)), " ") }
	return norm(s.text) == norm(prompt)
}

// stripTags removes <pasted_content …> wrappers but keeps their content.
func stripTags(s string) string {
	return pastedTagRE.ReplaceAllString(s, "")
}

var pastedTagRE = regexp.MustCompile(`</?pasted_content[^>]*>`)

// Hook applies one Claude Code hook event to its session. For
// UserPromptSubmit it returns the context to add to the prompt.
func (d *Daemon) Hook(p ipc.HookParams) ipc.HookResult {
	var hp claude.HookPayload
	_ = json.Unmarshal(p.Payload, &hp)
	d.mu.Lock()
	defer d.mu.Unlock()
	var res ipc.HookResult
	s := d.find(p.Session)
	if s == nil {
		return res
	}
	if hp.SessionID != "" && hp.SessionID != s.ClaudeID {
		s.ClaudeID = hp.SessionID // e.g. after /clear; --resume needs the newest id
	}
	old := s.State
	refreshGit := false
	switch p.Event {
	case "SessionStart":
		d.hooked[s.ID] = true
		if s.State != model.StateWorking && s.State != model.StateTool {
			s.State = model.StateIdle
			if s.Title != "" {
				s.State = model.StateDone
			}
		}
	case "UserPromptSubmit":
		s.State, s.Tool = model.StateWorking, ""
		fromDeck := d.deckSent[s.ID].matches(hp.Prompt)
		if fromDeck {
			delete(d.deckSent, s.ID)
		}
		if !fromDeck && strings.TrimSpace(hp.Prompt) != "" && !claude.IsSystemPrompt(hp.Prompt) {
			// Typed straight into Claude's box: it is history and title too.
			s.Title = shortTitle(hp.Prompt)
			d.addHistory(s.ID, strings.TrimSpace(hp.Prompt), "claude")
		}
		if !claude.IsSystemPrompt(hp.Prompt) {
			res.Context = d.takeContext(s.ID)
		}
	case "PreToolUse":
		s.State, s.Tool = model.StateTool, hp.ToolName
		if _, err := d.st.StartTool(s.ID, hp.ToolUseID, hp.ToolName, hp.ToolSummary(), hp.ToolInputText()); err != nil {
			log.Printf("tool start: %v", err)
		}
		d.publish("tools", s.ID)
	case "PostToolUse", "PostToolUseFailure":
		s.State, s.Tool = model.StateWorking, ""
		status := model.ToolOK
		if p.Event == "PostToolUseFailure" {
			status = model.ToolFailed
		}
		if _, err := d.st.FinishTool(s.ID, hp.ToolUseID, hp.ToolName, hp.ToolSummary(), hp.ToolInputText(), hp.ToolOutputText(), status); err != nil {
			log.Printf("tool finish: %v", err)
		}
		d.publish("tools", s.ID)
	case "Notification":
		switch notificationState(hp) {
		case model.StateInput:
			s.State = model.StateInput
		case model.StateDone:
			if s.State != model.StateInput {
				s.State = model.StateDone
			}
		}
		_ = d.st.AddEvent(s.ID, "notification", "", hp.Message)
	case "Stop":
		s.State, s.Tool = model.StateDone, ""
		refreshGit = true
		if err := d.st.StopRunningTools(s.ID); err == nil {
			d.publish("tools", s.ID)
		}
	}
	if s.State != old {
		d.attention(s, old)
	}
	d.save(s)
	d.changed()
	if refreshGit {
		go d.refreshGit(s.ID)
	}
	return res
}

// notificationState maps a Notification hook to a state ("" = no change).
func notificationState(hp claude.HookPayload) string {
	switch hp.NotificationType {
	case "permission_prompt", "elicitation_dialog":
		return model.StateInput
	case "idle_prompt":
		return model.StateDone
	}
	msg := strings.ToLower(hp.Message)
	switch {
	case strings.Contains(msg, "permission"), strings.Contains(msg, "needs your"):
		return model.StateInput
	case strings.Contains(msg, "waiting for your input"):
		return model.StateDone
	}
	return ""
}

// attention marks a session unread and notifies when it needs you or has
// finished while you weren't looking at it. Call with d.mu held.
func (d *Daemon) attention(s *model.Session, old string) {
	var title string
	urgent := false
	switch s.State {
	case model.StateInput:
		title, urgent = "Claude needs your input", true
	case model.StateDone:
		if old != model.StateWorking && old != model.StateTool {
			return // e.g. idle → done on resume: nothing happened
		}
		title = "Claude finished"
	case model.StateExited:
		title = "Claude exited"
	default:
		return
	}
	viewing := s.ID == d.active
	if !viewing {
		s.Unread = true
	}
	if viewing && d.focused {
		return
	}
	body := s.DisplayTitle()
	body += "\n" + paths.Tilde(s.OriginDir)
	notify.Send(title, body, urgent)
	notify.Bell(d.ttys)
}

// reconcilePanes marks sessions whose pane is gone or dead as exited,
// notifying only when announce is set (not at startup). Call with d.mu
// held. It reports whether anything changed.
func (d *Daemon) reconcilePanes(announce bool) bool {
	panes, err := d.tm.Panes()
	if err != nil {
		panes = nil // no server: every pane is gone
	}
	changed := false
	for _, s := range d.sessions {
		if s.Pane == "" {
			if s.State != model.StateExited {
				s.State = model.StateExited
				changed = true
				d.save(s)
			}
			continue
		}
		p, live := panes[s.Pane]
		switch {
		case !live:
			s.Pane = ""
			if s.State != model.StateExited {
				old := s.State
				s.State = model.StateExited
				if announce {
					d.attention(s, old)
				}
			}
			changed = true
			d.save(s)
		case p.Dead && s.State != model.StateExited:
			old := s.State
			s.State, s.ExitCode, s.Tool = model.StateExited, p.DeadStatus, ""
			if announce {
				d.attention(s, old)
			}
			changed = true
			d.save(s)
		}
	}
	if d.layout.Center != "" {
		if _, live := panes[d.layout.Center]; !live && panes != nil {
			d.layout.Center = d.layout.Placeholder
			d.saveLayout()
		}
	}
	return changed
}

// startupQuestions are screens Claude shows before it starts (and before
// any hook fires) that need an answer from you.
var startupQuestions = []string{"trust this folder", "do you trust the files"}

// watchStartup marks a session as needing input while Claude shows a
// startup question such as the folder trust prompt. Call with d.mu held.
func (d *Daemon) watchStartup() bool {
	changed := false
	for _, s := range d.sessions {
		if s.Pane == "" || d.hooked[s.ID] || (s.State != model.StateIdle && s.State != model.StateInput) {
			continue
		}
		screen, err := d.tm.Run("capture-pane", "-p", "-t", s.Pane)
		if err != nil {
			continue
		}
		asking := false
		low := strings.ToLower(screen)
		for _, q := range startupQuestions {
			asking = asking || strings.Contains(low, q)
		}
		switch {
		case asking && s.State != model.StateInput:
			old := s.State
			s.State = model.StateInput
			d.attention(s, old)
		case !asking && s.State == model.StateInput:
			s.State = model.StateIdle
		default:
			continue
		}
		d.save(s)
		changed = true
	}
	return changed
}

// slots returns which pane is in each slot of the deck window, from left
// to right (sessions, center, context). ok is false unless the window has
// exactly that shape.
func slots(panes map[string]tmux.Pane) (left, center, right string, ok bool) {
	var deck []tmux.Pane
	for _, p := range panes {
		if p.Session == deckSession {
			deck = append(deck, p)
		}
	}
	if len(deck) != 3 {
		return "", "", "", false
	}
	sort.Slice(deck, func(i, j int) bool { return deck[i].Left < deck[j].Left })
	if deck[0].Top != deck[1].Top || deck[1].Top != deck[2].Top {
		return "", "", "", false
	}
	return deck[0].ID, deck[1].ID, deck[2].ID, true
}

// repairLayout puts the deck's panes back in their slots when something
// moved them (a tmux key or mouse binding that swaps panes). Call with
// d.mu held.
func (d *Daemon) repairLayout() {
	l := d.layout
	for i := 0; i < 4; i++ {
		panes, err := d.tm.Panes()
		if err != nil {
			return
		}
		left, center, right, ok := slots(panes)
		if !ok {
			return
		}
		want := []struct{ pane, occupant string }{
			{l.Sidebar, left}, {l.Center, center}, {l.Context, right},
		}
		fixed := false
		for _, w := range want {
			p, live := panes[w.pane]
			if w.pane == w.occupant || !live || p.Session != deckSession {
				continue
			}
			log.Printf("layout: %s was moved, swapping it back with %s", w.pane, w.occupant)
			if _, err := d.tm.Run("swap-pane", "-d", "-s", w.pane, "-t", w.occupant); err != nil {
				log.Printf("layout repair: %v", err)
				return
			}
			fixed = true
			break // positions changed: look again
		}
		if !fixed {
			return
		}
	}
}

// paneLoop watches for exited sessions and the terminal's focus.
func (d *Daemon) paneLoop() {
	for range time.Tick(time.Second) {
		d.mu.Lock()
		changed := d.reconcilePanes(true)
		changed = d.watchStartup() || changed
		d.grantDirs()
		d.repairLayout()
		focused, ttys := d.clients()
		if focused != d.focused {
			d.focused = focused
			changed = true
		}
		d.ttys = ttys
		if changed {
			d.changed()
		}
		d.mu.Unlock()
	}
}

// clients returns whether a terminal showing the deck has focus, and the
// terminals attached to it.
func (d *Daemon) clients() (bool, []string) {
	out, err := d.tm.Run("list-clients", "-F", "#{client_tty}\t#{client_flags}\t#{session_name}")
	if err != nil || out == "" {
		return false, nil
	}
	focused := false
	var ttys []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 || f[2] != deckSession {
			continue
		}
		ttys = append(ttys, f[0])
		if strings.Contains(","+f[1]+",", ",focused,") {
			focused = true
		}
	}
	return focused, ttys
}

// gitLoop refreshes branch and change counts every 5 seconds.
func (d *Daemon) gitLoop() {
	for range time.Tick(5 * time.Second) {
		d.mu.Lock()
		ids := make([]string, 0, len(d.sessions))
		for _, s := range d.sessions {
			ids = append(ids, s.ID)
		}
		d.mu.Unlock()
		for _, id := range ids {
			d.refreshGit(id)
		}
	}
}

func (d *Daemon) refreshGit(id string) {
	d.mu.Lock()
	s := d.find(id)
	if s == nil {
		d.mu.Unlock()
		return
	}
	cwd := s.Cwd
	d.mu.Unlock()

	g := gitx.Status(cwd) // outside the lock: git can be slow

	d.mu.Lock()
	defer d.mu.Unlock()
	if s = d.find(id); s != nil && s.Git != g {
		s.Git = g
		d.save(s)
		d.changed()
	}
}

func init() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}
