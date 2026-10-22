// Package viewer shows a context item's text, as Claude receives it, in a
// popup.
package viewer

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"agentdeck/internal/contextkit"
	"agentdeck/internal/ipc"
	"agentdeck/internal/ui/theme"
)

type loadedMsg struct {
	text string
	err  error
}

// Model is the viewer.
type Model struct {
	id   int64
	vp   viewport.Model
	text string
	err  error
	w, h int
}

func New(id int64) Model { return Model{id: id, vp: viewport.New()} }

func (m Model) Init() tea.Cmd {
	return func() tea.Msg {
		var s string
		err := ipc.Call("context_preview", ipc.ItemParams{ID: m.id}, &s)
		return loadedMsg{s, err}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.SetWidth(m.w)
		m.vp.SetHeight(max(m.h-1, 1))
	case loadedMsg:
		m.text, m.err = msg.text, msg.err
		m.vp.SetContent(msg.text)
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c", "enter":
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m Model) View() tea.View {
	var body, status string
	if m.err != nil {
		body = theme.ErrMsg.Render(m.err.Error())
	} else {
		body = m.vp.View()
	}
	status = fmt.Sprintf("what Claude receives · ~%s tokens · ↑↓ scroll · q close",
		theme.Kilo(contextkit.Tokens(m.text)))
	v := tea.NewView(body + strings.Repeat("\n", max(m.h-1-strings.Count(body, "\n")-1, 0)) + "\n" +
		theme.Faint.Render(theme.Truncate(status, max(m.w, 10))))
	v.AltScreen = true
	return v
}
