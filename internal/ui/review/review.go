// Package review shows a session's uncommitted changes as a diff and
// jumps to a hunk in $EDITOR. It runs in a tmux popup.
package review

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"agentdeck/internal/gitx"
	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
	"agentdeck/internal/ui/theme"
)

// Model is the diff viewer.
type Model struct {
	dir     string
	session string // gets the lines added with a
	vp      viewport.Model
	plain   []string // uncolored lines, for finding files and hunks
	files   []int    // line index of each "diff --git"
	err     error
	w, h    int
	msg     string
}

type editedMsg struct{ err error }

type addedMsg struct {
	label string
	err   error
}

// New loads the diff of dir; session gets lines added with a.
func New(dir, session string) Model {
	m := Model{dir: dir, session: session, vp: viewport.New()}
	m.reload()
	return m
}

func (m *Model) reload() {
	colored, err := gitx.Diff(m.dir, true)
	if err != nil {
		m.err = err
		return
	}
	plain, _ := gitx.Diff(m.dir, false)
	m.plain = strings.Split(plain, "\n")
	m.files = m.files[:0]
	for i, l := range m.plain {
		if strings.HasPrefix(l, "diff --git ") {
			m.files = append(m.files, i)
		}
	}
	if strings.TrimSpace(plain) == "" {
		colored = theme.Muted.Render("No uncommitted changes.")
	}
	m.vp.SetContent(colored)
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.SetWidth(m.w)
		m.vp.SetHeight(max(m.h-1, 1))
	case editedMsg:
		if msg.err != nil {
			m.msg = msg.err.Error()
		}
		m.reload()
	case addedMsg:
		m.msg = "added " + msg.label + " to the context"
		if msg.err != nil {
			m.msg = msg.err.Error()
		}
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "n", "]":
			m.jumpFile(1)
			return m, nil
		case "p", "[":
			m.jumpFile(-1)
			return m, nil
		case "r":
			m.reload()
			return m, nil
		case "e", "enter":
			return m, m.edit()
		case "a":
			return m, m.addHunk()
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m *Model) jumpFile(d int) {
	top := m.vp.YOffset()
	if d > 0 {
		for _, f := range m.files {
			if f > top {
				m.vp.SetYOffset(f)
				return
			}
		}
		return
	}
	for i := len(m.files) - 1; i >= 0; i-- {
		if m.files[i] < top {
			m.vp.SetYOffset(m.files[i])
			return
		}
	}
}

var hunkRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)`)

// location finds the file and new-side line at the top of the view.
func (m Model) location() (string, int) {
	top := min(m.vp.YOffset(), len(m.plain)-1)
	file, line := "", 1
	for i := top; i >= 0; i-- {
		l := m.plain[i]
		if line == 1 {
			if g := hunkRE.FindStringSubmatch(l); g != nil {
				n, _ := strconv.Atoi(g[1])
				// Count new-side lines between the hunk header and the top.
				for _, x := range m.plain[i+1 : top+1] {
					if !strings.HasPrefix(x, "-") {
						n++
					}
				}
				line = max(n-1, 1)
			}
		}
		if strings.HasPrefix(l, "+++ b/") {
			file = strings.TrimPrefix(l, "+++ b/")
			break
		}
		if strings.HasPrefix(l, "+++ ") && !strings.HasPrefix(l, "+++ /dev/null") {
			file = strings.TrimPrefix(l, "+++ ")
			break
		}
	}
	return file, line
}

// hunk returns the file and new-side line range of the hunk at the top
// of the view, or else the first hunk on screen (a short diff can't
// scroll).
func (m Model) hunk() (string, int, int) {
	top := min(m.vp.YOffset(), len(m.plain)-1)
	at := -1
	for i := top; i >= 0; i-- {
		if hunkFullRE.MatchString(m.plain[i]) {
			at = i
			break
		}
		if strings.HasPrefix(m.plain[i], "diff --git ") {
			break
		}
	}
	if at < 0 {
		for i := max(top, 0); i < len(m.plain) && i < top+m.vp.Height(); i++ {
			if hunkFullRE.MatchString(m.plain[i]) {
				at = i
				break
			}
		}
	}
	if at < 0 {
		return "", 0, 0
	}
	file := ""
	for i := at; i >= 0; i-- {
		if strings.HasPrefix(m.plain[i], "+++ b/") {
			file = strings.TrimPrefix(m.plain[i], "+++ b/")
			break
		}
	}
	g := hunkFullRE.FindStringSubmatch(m.plain[at])
	from, _ := strconv.Atoi(g[1])
	n := 1
	if g[2] != "" {
		n, _ = strconv.Atoi(g[2])
	}
	return file, from, from + max(n, 1) - 1
}

var hunkFullRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// addHunk attaches the lines of the hunk at the top to the session.
func (m Model) addHunk() tea.Cmd {
	file, from, to := m.hunk()
	if file == "" || from == 0 {
		return func() tea.Msg { return addedMsg{err: errors.New("no hunk on screen")} }
	}
	rng := fmt.Sprintf("%d-%d", from, to)
	session := m.session
	path := filepath.Join(m.dir, file)
	return func() tea.Msg {
		err := ipc.Call("context_add", ipc.AddParams{Session: session, Kind: model.KindLines, Ref: path, Payload: rng}, nil)
		return addedMsg{label: file + ":" + rng, err: err}
	}
}

func (m Model) edit() tea.Cmd {
	file, line := m.location()
	if file == "" {
		return nil
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	args := append(parts[1:], fmt.Sprintf("+%d", line), filepath.Join(m.dir, file))
	c := exec.Command(parts[0], args...)
	c.Dir = m.dir
	return tea.ExecProcess(c, func(err error) tea.Msg { return editedMsg{err} })
}

func (m Model) View() tea.View {
	if m.err != nil {
		return tea.NewView(theme.ErrMsg.Render(m.err.Error()) + "\n" + theme.Faint.Render("q close"))
	}
	status := fmt.Sprintf("%d files · n/p next/prev file · e editor · a add hunk to context · q close", len(m.files))
	if m.msg != "" {
		status = m.msg
	}
	v := tea.NewView(m.vp.View() + "\n" + theme.Faint.Render(theme.Truncate(status, max(m.w, 10))))
	v.AltScreen = true
	return v
}
