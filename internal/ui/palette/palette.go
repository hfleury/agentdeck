// Package palette is the command window (Alt-K): run a deck command, or
// pick a prompt you sent before, in any session, and put it in Claude's
// input box. It runs in a tmux popup.
package palette

import (
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/sahilm/fuzzy"

	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
	"agentdeck/internal/route"
	"agentdeck/internal/ui/picker"
	"agentdeck/internal/ui/review"
	"agentdeck/internal/ui/theme"
)

const listTop = 3 // title, input, rule

type item struct {
	cmd  *route.AppCommand // a deck command, or
	text string            // a prompt from history
}

func (it item) key() string {
	if it.cmd != nil {
		return "/" + it.cmd.Name
	}
	return it.text
}

type (
	loadedMsg struct {
		hist []model.HistoryEntry
		st   model.State
	}
	doneMsg struct {
		err  error
		note string
	}
)

// Model is the command window.
type Model struct {
	in      textinput.Model
	hist    []string // newest first, unique
	st      model.State
	items   []item
	sel     int
	offset  int
	w, h    int
	err     string
	busy    bool
	lastAt  time.Time
	lastIdx int
}

// New returns the command window.
func New() Model {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = "command, or search what you sent"
	in.Focus()
	m := Model{in: in, lastIdx: -1}
	m.refresh()
	return m
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, func() tea.Msg {
		var h []model.HistoryEntry
		_ = ipc.Call("history", ipc.HistoryParams{Limit: 2000}, &h)
		var st model.State
		_ = ipc.Call("state", nil, &st)
		return loadedMsg{h, st}
	})
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.in.SetWidth(max(m.w-4, 10))
		m.clamp()
		return m, nil
	case loadedMsg:
		m.st = msg.st
		seen := map[string]bool{}
		for i := len(msg.hist) - 1; i >= 0; i-- {
			t := msg.hist[i].Text
			if !seen[t] {
				seen[t] = true
				m.hist = append(m.hist, t)
			}
		}
		m.refresh()
		return m, nil
	case doneMsg:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		return m, tea.Quit
	case tea.PasteMsg:
		m.in.SetValue(m.in.Value() + route.Normalize(msg.Content))
		m.in.CursorEnd()
		m.changed()
		return m, nil
	case tea.MouseClickMsg:
		ms := msg.Mouse()
		if ms.Button != tea.MouseLeft {
			return m, nil
		}
		idx := m.offset + ms.Y - listTop
		if ms.Y < listTop || idx >= len(m.items) {
			return m, nil
		}
		double := idx == m.lastIdx && time.Since(m.lastAt) < 400*time.Millisecond
		m.lastAt, m.lastIdx, m.sel = time.Now(), idx, idx
		if double {
			return m.run()
		}
		return m, nil
	case tea.MouseWheelMsg:
		if msg.Mouse().Button == tea.MouseWheelUp {
			m.move(-1)
		} else if msg.Mouse().Button == tea.MouseWheelDown {
			m.move(1)
		}
		return m, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc", "ctrl+c", "alt+k":
			return m, tea.Quit
		case "enter":
			return m.run()
		case "tab":
			if len(m.items) > 0 && m.items[m.sel].cmd != nil {
				m.in.SetValue("/" + m.items[m.sel].cmd.Name + " ")
				m.in.CursorEnd()
				m.changed()
			}
			return m, nil
		case "up", "ctrl+p":
			m.move(-1)
			return m, nil
		case "down", "ctrl+n":
			m.move(1)
			return m, nil
		}
		before := m.in.Value()
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(msg)
		if m.in.Value() != before {
			m.changed()
		}
		return m, cmd
	}
	return m, nil
}

func (m *Model) changed() {
	m.err, m.sel, m.offset = "", 0, 0
	m.refresh()
}

// refresh filters commands and history by the typed text. Text starting
// with "/" matches command names only.
func (m *Model) refresh() {
	q := strings.TrimSpace(m.in.Value())
	var cmds, hist []item
	for i := range route.AppCommands {
		cmds = append(cmds, item{cmd: &route.AppCommands[i]})
	}
	if !strings.HasPrefix(q, "/") {
		for _, t := range m.hist {
			hist = append(hist, item{text: t})
		}
	}
	if q == "" {
		m.items = append(cmds, hist...)
		m.clamp()
		return
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(q, "/"), " ")
	if strings.HasPrefix(q, "/") {
		m.items = rank(name, cmds, func(it item) string { return it.cmd.Name })
	} else {
		// Commands match by name only, so a prompt's words don't pick one.
		m.items = append(rank(q, cmds, func(it item) string { return it.cmd.Name }),
			rank(q, hist, func(it item) string { return it.text })...)
	}
	m.clamp()
}

func rank(q string, items []item, key func(item) string) []item {
	keys := make([]string, len(items))
	for i, it := range items {
		keys[i] = key(it)
	}
	ms := fuzzy.Find(q, keys)
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Score > ms[j].Score })
	out := make([]item, 0, len(ms))
	for _, x := range ms {
		out = append(out, items[x.Index])
	}
	return out
}

// run acts on Enter: a deck command runs (or asks for its argument), a
// history entry goes into Claude's input box.
func (m Model) run() (tea.Model, tea.Cmd) {
	if m.busy {
		return m, nil
	}
	q := strings.TrimSpace(m.in.Value())
	// "/worktree feat-x" typed out in full: run it as typed.
	if strings.HasPrefix(q, "/") && strings.Contains(q, " ") {
		return m.send(q)
	}
	if len(m.items) == 0 {
		if q != "" {
			m.err = "no command or earlier prompt matches"
		}
		return m, nil
	}
	it := m.items[m.sel]
	if it.cmd == nil {
		m.busy = true
		text := it.text
		return m, func() tea.Msg { return doneMsg{err: ipc.Call("insert", ipc.SendParams{Text: text}, nil)} }
	}
	switch it.cmd.Name {
	case "new":
		// The folder window, in this same popup.
		dir, group := "", ""
		if s, ok := m.st.Find(m.st.Active); ok {
			dir, group = s.OriginDir, s.Group
		}
		p := picker.New(dir)
		p.Group = group
		return switchTo(p, m.w, m.h)
	case "changes":
		s, ok := m.st.Find(m.st.Active)
		if !ok {
			m.err = "no session selected"
			return m, nil
		}
		return switchTo(review.New(s.Cwd, s.ID), m.w, m.h)
	}
	// An optional argument typed out as nothing ("/title" alone) clears.
	if strings.HasPrefix(it.cmd.ArgHint, "[") && q == "/"+it.cmd.Name && it.cmd.Name != "new" {
		return m.send(q)
	}
	if it.cmd.ArgHint != "" && it.cmd.Name != "new" {
		// Needs an argument: complete the name and wait for it.
		m.in.SetValue("/" + it.cmd.Name + " ")
		m.in.CursorEnd()
		m.changed()
		return m, nil
	}
	return m.send("/" + it.cmd.Name)
}

// switchTo replaces this window with another model in the same popup.
func switchTo(next tea.Model, w, h int) (tea.Model, tea.Cmd) {
	return next, tea.Batch(next.Init(), func() tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} })
}

func (m Model) send(text string) (tea.Model, tea.Cmd) {
	if r := route.Classify(text, nil); r.Kind != route.App {
		m.err = text + " is not an agentdeck command — type it in Claude's box"
		return m, nil
	}
	m.busy = true
	return m, func() tea.Msg {
		var res ipc.SendResult
		err := ipc.Call("send", ipc.SendParams{Text: "//" + strings.TrimLeft(text, "/")}, &res)
		return doneMsg{err: err, note: res.Notice}
	}
}

func (m *Model) move(d int) {
	if len(m.items) == 0 {
		return
	}
	m.sel = min(max(m.sel+d, 0), len(m.items)-1)
	m.clamp()
}

func (m Model) rows() int { return max(m.h-listTop-1, 1) }

func (m *Model) clamp() {
	if m.sel >= len(m.items) {
		m.sel = max(len(m.items)-1, 0)
	}
	if m.sel < m.offset {
		m.offset = m.sel
	}
	if r := m.rows(); m.sel >= m.offset+r {
		m.offset = m.sel - r + 1
	}
}

func (m Model) View() tea.View {
	w := max(m.w, 20)
	accent := lipgloss.NewStyle().Foreground(theme.Accent)
	var b strings.Builder
	head := "Commands and earlier prompts"
	if s, ok := m.st.Find(m.st.Active); ok {
		t := s.DisplayTitle()
		head += theme.Faint.Render(" · " + theme.Truncate(t, w-35))
	}
	b.WriteString(theme.Title.Render(head) + "\n")
	b.WriteString(accent.Render("❯ ") + m.in.View() + "\n")
	b.WriteString(theme.Faint.Render(strings.Repeat("─", w)) + "\n")
	for i := 0; i < m.rows(); i++ {
		idx := m.offset + i
		if idx >= len(m.items) {
			if i == 0 {
				b.WriteString(theme.Faint.Render("  nothing matches"))
			}
			b.WriteString("\n")
			continue
		}
		b.WriteString(m.line(idx, w) + "\n")
	}
	status := "⏎ run / insert into Claude · tab complete · ↑↓ choose · esc close"
	if m.err != "" {
		b.WriteString(theme.ErrMsg.Render(theme.Truncate(m.err, w)))
	} else if m.busy {
		b.WriteString(theme.OkMsg.Render("…"))
	} else {
		b.WriteString(theme.Faint.Render(theme.Truncate(status, w)))
	}
	v := tea.NewView(b.String())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m Model) line(idx, w int) string {
	it := m.items[idx]
	marker := "  "
	if idx == m.sel {
		marker = lipgloss.NewStyle().Foreground(theme.Accent).Render("› ")
	}
	var line string
	if it.cmd != nil {
		name := "/" + it.cmd.Name
		if it.cmd.ArgHint != "" {
			name += " " + it.cmd.ArgHint
		}
		line = marker + lipgloss.NewStyle().Width(22).Render(name) + theme.Muted.Render(theme.Truncate(it.cmd.Desc, w-26))
	} else {
		text := strings.ReplaceAll(it.text, "\n", " ⏎ ")
		tag := "sent"
		line = marker + theme.Truncate(text, w-4-len(tag)-1)
		line += strings.Repeat(" ", max(w-lipgloss.Width(line)-len(tag), 1)) + theme.Faint.Render(tag)
	}
	if idx == m.sel {
		return lipgloss.NewStyle().Background(theme.SelBg).Width(w).Render(line)
	}
	return line
}
