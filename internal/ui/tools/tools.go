// Package tools is the tool-call browser: a session's calls with their
// input and output; a call's output can be added to the context. It runs
// in a tmux popup.
package tools

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
	"agentdeck/internal/ui/theme"
)

type (
	listMsg struct {
		calls []model.ToolCall
		err   error
	}
	callMsg struct {
		c   model.ToolCall
		err error
	}
	addedMsg struct{ err error }
	tickMsg  struct{}
)

// Model is the browser.
type Model struct {
	session string
	want    int64 // call to select once the list loads
	calls   []model.ToolCall
	sel     int
	offset  int
	detail  model.ToolCall
	vp      viewport.Model
	icons   theme.IconSet
	frame   int
	w, h    int
	notice  string
	isErr   bool
}

// New opens the calls of a session at call id (0 = newest).
func New(session string, id int64) Model {
	return Model{session: session, want: id, vp: viewport.New(), icons: theme.DetectIcons()}
}

func (m Model) load() tea.Cmd {
	return func() tea.Msg {
		var c []model.ToolCall
		err := ipc.Call("tools", ipc.HistoryParams{Session: m.session, Limit: 200}, &c)
		return listMsg{c, err}
	}
}

func (m Model) loadCall() tea.Cmd {
	if m.sel >= len(m.calls) {
		return nil
	}
	id := m.calls[m.sel].ID
	return func() tea.Msg {
		var c model.ToolCall
		err := ipc.Call("tool", ipc.ItemParams{ID: id}, &c)
		return callMsg{c, err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.load(), tick()) }

func (m Model) listRows() int { return max(min(len(m.calls), m.h*2/5), 3) }

func (m *Model) layout() {
	m.vp.SetWidth(m.w)
	m.vp.SetHeight(max(m.h-m.listRows()-3, 1))
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.layout()
	case tickMsg:
		m.frame++
		for _, c := range m.calls {
			if c.Status == model.ToolRunning {
				return m, tea.Batch(m.load(), tick())
			}
		}
		return m, tick()
	case listMsg:
		if msg.err != nil {
			m.notice, m.isErr = msg.err.Error(), true
			return m, nil
		}
		prev := int64(0)
		if m.sel < len(m.calls) {
			prev = m.calls[m.sel].ID
		}
		m.calls = msg.calls
		m.sel = len(m.calls) - 1
		target := prev
		if m.want != 0 {
			target, m.want = m.want, 0
		}
		for i, c := range m.calls {
			if c.ID == target {
				m.sel = i
			}
		}
		m.clamp()
		m.layout()
		return m, m.loadCall()
	case callMsg:
		if msg.err == nil {
			m.detail = msg.c
			m.vp.SetContent(render(msg.c))
			m.vp.GotoTop()
		}
	case addedMsg:
		if msg.err != nil {
			m.notice, m.isErr = msg.err.Error(), true
		} else {
			m.notice, m.isErr = "output added to the context (goes with the next prompt)", false
		}
	case tea.MouseClickMsg:
		ms := msg.Mouse()
		if ms.Button == tea.MouseLeft && ms.Y >= 1 && ms.Y <= m.listRows() {
			if idx := m.offset + ms.Y - 1; idx < len(m.calls) {
				m.sel = idx
				return m, m.loadCall()
			}
		}
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "j", "down":
			if m.sel < len(m.calls)-1 {
				m.sel++
				m.clamp()
				return m, m.loadCall()
			}
			return m, nil
		case "k", "up":
			if m.sel > 0 {
				m.sel--
				m.clamp()
				return m, m.loadCall()
			}
			return m, nil
		case "a":
			c := m.detail
			if c.ID == 0 {
				return m, nil
			}
			if strings.TrimSpace(c.Output) == "" {
				m.notice, m.isErr = "this call has no output", true
				return m, nil
			}
			title := strings.TrimSpace(c.Tool + " " + c.Summary)
			return m, func() tea.Msg {
				return addedMsg{ipc.Call("context_add", ipc.AddParams{Session: c.SessionID, Kind: model.KindToolOutput,
					Payload: c.Output, Title: title}, nil)}
			}
		case "r":
			return m, m.load()
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m *Model) clamp() {
	rows := m.listRows()
	if m.sel < m.offset {
		m.offset = m.sel
	}
	if m.sel >= m.offset+rows {
		m.offset = m.sel - rows + 1
	}
	m.offset = max(m.offset, 0)
}

func render(c model.ToolCall) string {
	var b strings.Builder
	b.WriteString(theme.Bold.Render(c.Tool) + "  " + c.Summary + "\n")
	b.WriteString(theme.Faint.Render(fmt.Sprintf("%s · %s", c.Status, time.Unix(c.At, 0).Format("15:04:05"))) + "\n\n")
	if c.Input != "" {
		b.WriteString(theme.Muted.Render("Input") + "\n" + c.Input + "\n\n")
	}
	out := c.Output
	if out == "" {
		out = theme.Faint.Render("(none yet)")
	}
	label := theme.Muted.Render("Output")
	if c.Status == model.ToolFailed {
		label = theme.ErrMsg.Render("Output (failed)")
	}
	b.WriteString(label + "\n" + out)
	return b.String()
}

func (m Model) View() tea.View {
	w := max(m.w, 20)
	var b strings.Builder
	b.WriteString(theme.Title.Render("Tool calls") + theme.Faint.Render(fmt.Sprintf("  %d", len(m.calls))) + "\n")
	rows := m.listRows()
	for i := 0; i < rows; i++ {
		idx := m.offset + i
		if idx >= len(m.calls) {
			if len(m.calls) == 0 && i == 0 {
				b.WriteString(theme.Faint.Render("  no tool calls yet"))
			}
			b.WriteString("\n")
			continue
		}
		c := m.calls[idx]
		line := " " + m.icons.ToolIcon(c.Status, m.frame) + " " + lipgloss.NewStyle().Width(10).Render(theme.Truncate(c.Tool, 10)) +
			" " + theme.Muted.Render(theme.Truncate(c.Summary, w-16))
		if idx == m.sel {
			line = lipgloss.NewStyle().Background(theme.SelBg).Width(w).Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(theme.Faint.Render(strings.Repeat("─", w)) + "\n")
	b.WriteString(m.vp.View() + "\n")
	status := theme.Faint.Render(theme.Truncate("↑↓ choose · a add output to context · scroll with wheel/PgDn · q close", w))
	if m.notice != "" {
		style := theme.OkMsg
		if m.isErr {
			style = theme.ErrMsg
		}
		status = style.Render(theme.Truncate(m.notice, w))
	}
	b.WriteString(status)
	v := tea.NewView(b.String())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
