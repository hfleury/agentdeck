// Package empty is the center pane shown when no session is selected.
package empty

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"agentdeck/internal/ui/theme"
)

type Model struct{ w, h int }

func New() Model { return Model{} }

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if s, ok := msg.(tea.WindowSizeMsg); ok {
		m.w, m.h = s.Width, s.Height
	}
	return m, nil
}

func (m Model) View() tea.View {
	text := theme.Title.Render("agentdeck") + "\n\n" +
		theme.Muted.Render("No session selected.") + "\n\n" +
		"Press n in the sessions list,\n" +
		"or Alt-K for all commands.\n\n" +
		theme.Faint.Render("Ctrl-A sessions\nCtrl-G context\nCtrl-Space Claude")
	box := lipgloss.NewStyle().Align(lipgloss.Center).Render(text)
	v := tea.NewView(lipgloss.Place(max(m.w, 1), max(m.h, 1), lipgloss.Center, lipgloss.Center, box))
	v.AltScreen = true
	return v
}
