// Package theme holds the colors and status icons shared by the panes.
package theme

import (
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"strings"

	"charm.land/lipgloss/v2"

	"agentdeck/internal/model"
)

// Colors.
var (
	Gray   = lipgloss.Color("245")
	Dim    = lipgloss.Color("240")
	Yellow = lipgloss.Color("214")
	Red    = lipgloss.Color("203")
	Green  = lipgloss.Color("78")
	Blue   = lipgloss.Color("75")
	Accent = lipgloss.Color("214")
	SelBg  = lipgloss.Color("237")
)

// Styles used by more than one pane.
var (
	Faint  = lipgloss.NewStyle().Foreground(Dim)
	Muted  = lipgloss.NewStyle().Foreground(Gray)
	Bold   = lipgloss.NewStyle().Bold(true)
	Title  = lipgloss.NewStyle().Bold(true).Foreground(Accent)
	ErrMsg = lipgloss.NewStyle().Foreground(Red)
	OkMsg  = lipgloss.NewStyle().Foreground(Green)
)

// IconSet is one family of status icons.
type IconSet struct {
	Name    string
	Idle    string
	Spinner []string // working
	Tool    string
	Input   string
	Done    string
	Exited  string
}

// The three icon sets. Nerd Font code points are Font Awesome ones, which
// every Nerd Font (including the symbols-only font) carries.
var (
	Nerd = IconSet{
		Name:    "nerd",
		Idle:    "", // nf-fa-circle_o
		Spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		Tool:    "", // nf-fa-cog
		Input:   "", // nf-fa-bell
		Done:    "", // nf-fa-check_circle
		Exited:  "", // nf-fa-times_circle
	}
	Unicode = IconSet{
		Name:    "unicode",
		Idle:    "○",
		Spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		Tool:    "⚙",
		Input:   "!",
		Done:    "✓",
		Exited:  "✗",
	}
	ASCII = IconSet{
		Name:    "ascii",
		Idle:    "o",
		Spinner: []string{"-", "\\", "|", "/"},
		Tool:    "*",
		Input:   "!",
		Done:    "v",
		Exited:  "x",
	}
)

// DetectIcons picks the icon set: $AGENTDECK_ICONS if set, ASCII on the
// Linux console, Nerd Font icons when fontconfig knows a Nerd Font,
// otherwise plain Unicode.
func DetectIcons() IconSet {
	switch os.Getenv("AGENTDECK_ICONS") {
	case "nerd":
		return Nerd
	case "unicode":
		return Unicode
	case "ascii":
		return ASCII
	}
	if os.Getenv("TERM") == "linux" {
		return ASCII
	}
	out, err := exec.Command("fc-list", ":", "family").Output()
	if err == nil && strings.Contains(strings.ToLower(string(out)), "nerd font") {
		return Nerd
	}
	return Unicode
}

// StatusColor is the color of a state's icon.
func StatusColor(state string) color.Color {
	switch state {
	case model.StateWorking, model.StateTool:
		return Yellow
	case model.StateInput, model.StateExited:
		return Red
	case model.StateDone:
		return Green
	}
	return Gray
}

// Icon returns the colored icon for a session; frame animates the spinner.
func (s IconSet) Icon(state string, frame int) string {
	var g string
	switch state {
	case model.StateWorking:
		g = s.Spinner[frame%len(s.Spinner)]
	case model.StateTool:
		g = s.Tool
	case model.StateInput:
		g = s.Input
	case model.StateDone:
		g = s.Done
	case model.StateExited:
		g = s.Exited
	default:
		g = s.Idle
	}
	if os.Getenv("NO_COLOR") != "" {
		return g
	}
	return lipgloss.NewStyle().Foreground(StatusColor(state)).Render(g)
}

// StateLabel is a short human description of a state.
func StateLabel(s model.Session) string {
	switch s.State {
	case model.StateWorking:
		return "working"
	case model.StateTool:
		if s.Tool != "" {
			return "running " + s.Tool
		}
		return "running a tool"
	case model.StateInput:
		return "needs your input"
	case model.StateDone:
		return "waiting for you"
	case model.StateExited:
		return "exited"
	}
	return "ready"
}

// TruncateLeft keeps the end of s (the useful part of a path), adding …
// at the start when cut.
func TruncateLeft(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > width {
		r = r[1:]
	}
	return "…" + string(r)
}

// Truncate cuts s to width cells, adding … when cut.
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > width {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// KindIcon is the icon of a context item kind.
func (s IconSet) KindIcon(kind string) string {
	if s.Name == "nerd" {
		switch kind {
		case model.KindFile:
			return "" // nf-fa-file
		case model.KindFolder:
			return "" // nf-fa-folder
		case model.KindLines:
			return "" // nf-fa-bars
		case model.KindNote:
			return "" // nf-fa-sticky_note
		case model.KindConversation:
			return "" // nf-fa-comments
		case model.KindMemory:
			return "" // nf-fa-book
		case model.KindResult:
			return "" // nf-fa-tasks
		case model.KindToolOutput:
			return "" // nf-fa-terminal
		}
	}
	switch kind {
	case model.KindFile:
		return "f"
	case model.KindFolder:
		return "d"
	case model.KindLines:
		return "l"
	case model.KindNote:
		return "n"
	case model.KindConversation:
		return "c"
	case model.KindMemory:
		return "m"
	case model.KindResult:
		return "r"
	case model.KindToolOutput:
		return "t"
	}
	return "?"
}

// ToolIcon is the colored icon of a tool call status.
func (s IconSet) ToolIcon(status string, frame int) string {
	switch status {
	case model.ToolRunning:
		return lipgloss.NewStyle().Foreground(Yellow).Render(s.Spinner[frame%len(s.Spinner)])
	case model.ToolFailed:
		return lipgloss.NewStyle().Foreground(Red).Render(s.Exited)
	}
	return lipgloss.NewStyle().Foreground(Green).Render(s.Done)
}

// Kilo formats a token count: 950, 1.2k, 15k.
func Kilo(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%dk", (n+500)/1000)
}
