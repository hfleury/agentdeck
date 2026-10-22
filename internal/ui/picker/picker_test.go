package picker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func labels(m Model) []string {
	var out []string
	for _, it := range m.items {
		out = append(out, it.label)
	}
	return out
}

func press(m Model, keys ...string) Model {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "backspace":
			msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
		default:
			r := []rune(k)[0]
			msg = tea.KeyPressMsg{Code: r, Text: k}
		}
		nm, _ := m.Update(msg)
		m = nm.(Model)
	}
	return m
}

func TestBrowseFilterAndDescend(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "Financeiro", "Logo", "Planos", "claude-plugin/sub", ".hidden")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(root)
	m, _ = func() (Model, tea.Cmd) {
		nm, c := m.Update(tea.WindowSizeMsg{Width: 70, Height: 20})
		return nm.(Model), c
	}()
	if got := strings.Join(labels(m), " "); got != "claude-plugin/ Financeiro/ Logo/ Planos/" {
		t.Fatalf("listing: %q", got)
	}
	m = press(m, "p", "l")
	if got := labels(m); len(got) == 0 || got[0] != "Planos/" {
		t.Fatalf("filter pl: %v", got)
	}
	dir, ok := m.target()
	if !ok || dir != filepath.Join(root, "Planos") {
		t.Fatalf("one match is the target: %q %v", dir, ok)
	}
	m = press(m, "backspace", "backspace", "c", "tab")
	if !strings.HasSuffix(m.in.Value(), "/claude-plugin/") || strings.Join(labels(m), " ") != "sub/" {
		t.Fatalf("tab descends: %q %v", m.in.Value(), labels(m))
	}
	m = press(m, "down")
	if dir, _ := m.target(); dir != filepath.Join(root, "claude-plugin", "sub") {
		t.Fatalf("arrow selection is the target: %q", dir)
	}
}

func TestSearchModeUsesRecent(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "Oni/WebOfficer", "other")
	m := New(root)
	nm, _ := m.Update(recentMsg{filepath.Join(root, "other"), filepath.Join(root, "Oni", "WebOfficer")})
	m = nm.(Model)
	m.in.SetValue("webof")
	m.refresh()
	if len(m.items) == 0 || m.items[0].path != filepath.Join(root, "Oni", "WebOfficer") {
		t.Fatalf("recent match first: %+v", m.items)
	}
	if !searchMode("webof") || searchMode("~/x") || searchMode("a/b") || searchMode("") {
		t.Fatal("searchMode")
	}
}
