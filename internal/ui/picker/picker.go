// Package picker is the "New session" window: choose a folder (and a
// model), then the session starts. It runs in a tmux popup.
//
// Typing a path browses it: the subfolders of the folder typed so far,
// filtered by the last part. Typing a plain name (no "/") searches folders
// under your home with fd, plus recent folders.
package picker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/sahilm/fuzzy"

	"agentdeck/internal/claude"
	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
	"agentdeck/internal/paths"
	"agentdeck/internal/ui/theme"
)

const (
	listTop     = 4 // rows above the list: title, blank, folder, rule
	doubleClick = 400 * time.Millisecond
)

type entry struct {
	path  string // absolute
	label string
	tag   string // "", "recent", "found"
}

type (
	recentMsg []string
	foundMsg  struct {
		query string
		dirs  []string
	}
	startedMsg struct{ err error }
	searchMsg  struct{ query string }
)

// Model is the picker.
type Model struct {
	in      textinput.Model
	recent  []string
	found   []string // fd results for foundFor
	foundQ  string
	items   []entry
	sel     int
	nav     bool // moved through the list: Enter picks the highlighted folder
	offset  int
	models  []string
	model   int
	err     string
	w, h    int
	busy    bool
	lastAt  time.Time
	lastIdx int
	// Search runs only when typing pauses.
	pending string
	// addTo: in "add to context" mode, the session that gets the file or
	// folder; files are listed too.
	addTo string
	// Group: the new session joins it.
	Group string
}

// New starts the picker in dir.
func New(dir string) Model {
	in := textinput.New()
	in.Prompt = ""
	in.SetValue(withSlash(paths.Tilde(dir)))
	in.CursorEnd()
	in.Focus()
	models := []string{""}
	for _, m := range claude.Models {
		if m.Name != "default" {
			models = append(models, m.Name)
		}
	}
	m := Model{in: in, models: models, lastIdx: -1}
	m.refresh()
	return m
}

// NewAdd opens the picker to add a file or folder to a session's context.
func NewAdd(session, dir string) Model {
	m := New(dir)
	m.addTo = session
	m.refresh()
	return m
}

func withSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, loadRecent())
}

func (m Model) adding() bool { return m.addTo != "" }

// loadRecent gets the folders sessions were started in from the daemon,
// plus zoxide's frequent folders when zoxide is installed.
func loadRecent() tea.Cmd {
	return func() tea.Msg {
		var list []string
		_ = ipc.Call("recent", nil, &list)
		var st model.State
		if ipc.Call("state", nil, &st) == nil {
			for _, s := range st.Sessions {
				list = append(list, s.OriginDir)
			}
		}
		if out, err := exec.Command("zoxide", "query", "-l").Output(); err == nil {
			list = append(list, strings.Split(strings.TrimSpace(string(out)), "\n")...)
		}
		seen := map[string]bool{}
		var uniq []string
		for _, d := range list {
			if d != "" && !seen[d] {
				seen[d] = true
				uniq = append(uniq, d)
			}
		}
		return recentMsg(uniq)
	}
}

// search finds folders (and files, when adding) named like query under
// the home folder with fd.
func search(query string, files bool) tea.Cmd {
	return func() tea.Msg {
		bin, err := exec.LookPath("fd")
		if err != nil {
			return foundMsg{query: query}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		args := []string{"--type", "d"}
		if files {
			args = nil
		}
		args = append(args, "--max-depth", "6", "--ignore-case", "--absolute-path", "--max-results", "200", "--", query, paths.Home())
		out, _ := exec.CommandContext(ctx, bin, args...).Output()
		var dirs []string
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l != "" {
				dirs = append(dirs, strings.TrimSuffix(l, "/"))
			}
		}
		return foundMsg{query: query, dirs: dirs}
	}
}

// searchMode: a plain name, not a path.
func searchMode(v string) bool {
	return v != "" && !strings.ContainsRune(v, '/') && !strings.HasPrefix(v, "~") && !strings.HasPrefix(v, ".")
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.in.SetWidth(max(m.w-12, 10))
		m.clamp()
		return m, nil
	case recentMsg:
		m.recent = msg
		m.refresh()
		return m, nil
	case searchMsg:
		if msg.query == m.pending && msg.query == m.in.Value() {
			return m, search(msg.query, m.adding())
		}
		return m, nil
	case foundMsg:
		if msg.query == m.in.Value() {
			m.found, m.foundQ = msg.dirs, msg.query
			m.refresh()
		}
		return m, nil
	case startedMsg:
		m.busy = false
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		return m, tea.Quit
	case tea.PasteMsg:
		m.in.SetValue(m.in.Value() + strings.TrimSpace(msg.Content))
		m.in.CursorEnd()
		return m.changed()
	case tea.MouseClickMsg:
		return m.click(msg.Mouse())
	case tea.MouseWheelMsg:
		if msg.Mouse().Button == tea.MouseWheelUp {
			m.move(-1)
		} else if msg.Mouse().Button == tea.MouseWheelDown {
			m.move(1)
		}
		return m, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc", "ctrl+c":
			return m, tea.Quit
		case "enter":
			return m.start()
		case "tab", "right":
			if msg.String() == "right" && m.in.Position() < len([]rune(m.in.Value())) {
				break
			}
			m.descend()
			return m.changed()
		case "up", "ctrl+p":
			m.move(-1)
			return m, nil
		case "down", "ctrl+n":
			m.move(1)
			return m, nil
		case "ctrl+t":
			if m.adding() {
				return m, nil
			}
			m.model = (m.model + 1) % len(m.models)
			return m, nil
		case "ctrl+u":
			m.in.SetValue("")
			return m.changed()
		}
		before := m.in.Value()
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(msg)
		if m.in.Value() != before {
			nm, c := m.changed()
			return nm, tea.Batch(cmd, c)
		}
		return m, cmd
	}
	var cmd tea.Cmd
	m.in, cmd = m.in.Update(msg)
	return m, cmd
}

// changed runs after the typed text changed.
func (m Model) changed() (tea.Model, tea.Cmd) {
	m.err, m.nav, m.sel, m.offset = "", false, 0, 0
	m.refresh()
	v := m.in.Value()
	if searchMode(v) && v != m.foundQ {
		m.pending = v
		return m, tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return searchMsg{query: v} })
	}
	return m, nil
}

// split turns the typed text into the folder being browsed and the
// filter for its subfolders.
func split(v string) (dir, frag string) {
	full := paths.Expand(v)
	if v == "" || strings.HasSuffix(v, "/") {
		if v == "" {
			return paths.Home(), ""
		}
		return full, ""
	}
	return filepath.Dir(full), filepath.Base(full)
}

// refresh rebuilds the list for the typed text.
func (m *Model) refresh() {
	v := m.in.Value()
	var items []entry
	if searchMode(v) {
		items = append(items, filterRecent(m.recent, v)...)
		seen := map[string]bool{}
		for _, it := range items {
			seen[it.path] = true
		}
		if m.foundQ == v {
			for _, d := range rankPaths(m.found, v) {
				if !seen[d] {
					items = append(items, entry{path: d, label: paths.Tilde(d), tag: "found"})
				}
			}
		}
	} else {
		dir, frag := split(v)
		for _, name := range subdirs(dir, frag, m.adding()) {
			items = append(items, entry{path: filepath.Join(dir, strings.TrimSuffix(name, "/")), label: name})
		}
		if frag == "" && len(items) < 50 {
			for _, it := range filterRecent(m.recent, "") {
				if filepath.Dir(it.path) != dir && it.path != dir {
					items = append(items, it)
				}
			}
		}
	}
	m.items = items
	m.clamp()
}

func filterRecent(recent []string, q string) []entry {
	var out []entry
	list := recent
	if q != "" {
		list = rankPaths(recent, q)
	}
	for _, d := range list {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, entry{path: d, label: paths.Tilde(d), tag: "recent"})
		}
		if len(out) == 8 {
			break
		}
	}
	return out
}

// rankPaths orders paths by a fuzzy match of q against their last part,
// falling back to the whole path.
func rankPaths(list []string, q string) []string {
	base := make([]string, len(list))
	for i, p := range list {
		base[i] = filepath.Base(p)
	}
	matches := fuzzy.Find(q, base)
	sort.SliceStable(matches, func(i, j int) bool {
		pi := strings.HasPrefix(strings.ToLower(matches[i].Str), strings.ToLower(q))
		pj := strings.HasPrefix(strings.ToLower(matches[j].Str), strings.ToLower(q))
		if pi != pj {
			return pi
		}
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return len(list[matches[i].Index]) < len(list[matches[j].Index])
	})
	out := make([]string, 0, len(matches))
	for _, mt := range matches {
		out = append(out, list[mt.Index])
	}
	return out
}

// subdirs lists folders in dir matching frag (prefix matches first, then
// fuzzy), as "name/"; with files, files too. Hidden entries only when frag
// starts with ".".
func subdirs(dir, frag string, files bool) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
				isDir = fi.IsDir()
			}
		}
		if (!isDir && !files) || (strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(frag, ".")) {
			continue
		}
		if isDir {
			names = append(names, e.Name()+"/")
		} else {
			names = append(names, e.Name())
		}
	}
	// Folders first, then files, each alphabetically.
	sort.Slice(names, func(i, j int) bool {
		di, dj := strings.HasSuffix(names[i], "/"), strings.HasSuffix(names[j], "/")
		if di != dj {
			return di
		}
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})
	if frag == "" {
		return names
	}
	trimmed := make([]string, len(names))
	for i, n := range names {
		trimmed[i] = strings.TrimSuffix(n, "/")
	}
	ranked := rankPaths(trimmed, frag)
	isDir := map[string]bool{}
	for _, n := range names {
		if strings.HasSuffix(n, "/") {
			isDir[strings.TrimSuffix(n, "/")] = true
		}
	}
	for i, n := range ranked {
		if isDir[n] {
			ranked[i] = n + "/"
		}
	}
	return ranked
}

// descend puts the highlighted folder into the input, to browse into it.
func (m *Model) descend() {
	if len(m.items) == 0 {
		return
	}
	p := m.items[m.sel].path
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		m.in.SetValue(paths.Tilde(p)) // a file: nothing to open
	} else {
		m.in.SetValue(withSlash(paths.Tilde(p)))
	}
	m.in.CursorEnd()
}

// target is the folder Enter would start in.
func (m Model) target() (string, bool) {
	if m.nav && len(m.items) > 0 {
		return m.items[m.sel].path, true
	}
	v := m.in.Value()
	if searchMode(v) {
		if len(m.items) > 0 {
			return m.items[0].path, true
		}
		return "", false
	}
	p := paths.Expand(v)
	if fi, err := os.Stat(p); err == nil && (fi.IsDir() || m.adding()) {
		return p, true
	}
	// A partial name: the top folder, when its name starts with it.
	if _, frag := split(v); frag != "" && len(m.items) > 0 && m.items[0].tag == "" &&
		strings.HasPrefix(strings.ToLower(filepath.Base(m.items[0].path)), strings.ToLower(frag)) {
		return m.items[0].path, true
	}
	return "", false
}

func (m Model) start() (tea.Model, tea.Cmd) {
	if m.busy {
		return m, nil
	}
	dir, ok := m.target()
	if !ok {
		m.err = "not a folder: " + m.in.Value()
		if m.adding() {
			m.err = "not found: " + m.in.Value()
		}
		return m, nil
	}
	m.busy = true
	if m.adding() {
		session := m.addTo
		return m, func() tea.Msg {
			// The daemon decides file or folder from the path.
			return startedMsg{err: ipc.Call("context_add", ipc.AddParams{Session: session, Kind: model.KindFile, Ref: dir}, nil)}
		}
	}
	mdl, group := m.models[m.model], m.Group
	return m, func() tea.Msg {
		var s model.Session
		return startedMsg{err: ipc.Call("new", ipc.NewParams{Dir: dir, Model: mdl, Group: group}, &s)}
	}
}

func (m *Model) move(d int) {
	if len(m.items) == 0 {
		return
	}
	m.sel = min(max(m.sel+d, 0), len(m.items)-1)
	m.nav = true
	m.clamp()
}

// listRows: the rows left after the header (listTop) and the rule,
// model and status lines below the list.
func (m Model) listRows() int { return max(m.h-listTop-3, 1) }

func (m *Model) clamp() {
	if m.sel >= len(m.items) {
		m.sel = max(len(m.items)-1, 0)
	}
	rows := m.listRows()
	if m.sel < m.offset {
		m.offset = m.sel
	}
	if m.sel >= m.offset+rows {
		m.offset = m.sel - rows + 1
	}
}

func (m Model) click(ms tea.Mouse) (tea.Model, tea.Cmd) {
	if ms.Button != tea.MouseLeft {
		return m, nil
	}
	row := ms.Y - listTop
	if row >= 0 && row < m.listRows() {
		idx := m.offset + row
		if idx >= len(m.items) {
			return m, nil
		}
		double := idx == m.lastIdx && time.Since(m.lastAt) < doubleClick
		m.lastAt, m.lastIdx = time.Now(), idx
		m.sel, m.nav = idx, true
		if double {
			return m.start() // double-click: start here
		}
		return m, nil
	}
	if ms.Y == listTop+m.listRows()+1 && !m.adding() { // model row
		m.model = (m.model + 1) % len(m.models)
	}
	return m, nil
}

func (m Model) View() tea.View {
	w := max(m.w, 20)
	accent := lipgloss.NewStyle().Foreground(theme.Accent)
	rule := theme.Faint.Render(strings.Repeat("─", w))
	var b strings.Builder
	if m.adding() {
		b.WriteString(theme.Title.Render("Add to context…") + "\n\n")
		b.WriteString(theme.Muted.Render("Path    ") + m.in.View() + "\n")
	} else {
		head := theme.Title.Render("Start a session in…")
		if m.Group != "" {
			head += theme.Faint.Render("  group " + m.Group)
		}
		b.WriteString(head + "\n\n")
		b.WriteString(theme.Muted.Render("Folder  ") + m.in.View() + "\n")
	}
	b.WriteString(rule + "\n")

	rows := m.listRows()
	for i := 0; i < rows; i++ {
		idx := m.offset + i
		if idx >= len(m.items) {
			if i == 0 {
				msg := "no folders here"
				if searchMode(m.in.Value()) && m.foundQ != m.in.Value() {
					msg = "searching…"
				}
				b.WriteString(theme.Faint.Render("  "+msg) + "\n")
			} else {
				b.WriteString("\n")
			}
			continue
		}
		it := m.items[idx]
		marker := "  "
		if idx == m.sel {
			marker = accent.Render("› ")
		}
		tag := ""
		if it.tag != "" {
			tag = it.tag
		}
		room := w - 2 - lipgloss.Width(tag) - 1
		label := theme.TruncateLeft(it.label, room)
		line := marker + label
		if tag != "" {
			line += strings.Repeat(" ", max(w-lipgloss.Width(line)-lipgloss.Width(tag), 1)) + theme.Faint.Render(tag)
		}
		if idx == m.sel {
			line = lipgloss.NewStyle().Background(theme.SelBg).Width(w).Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(rule + "\n")

	var ms []string
	for i, x := range m.models {
		if x == "" {
			x = "default"
		}
		if i == m.model {
			ms = append(ms, accent.Bold(true).Render(x))
		} else {
			ms = append(ms, theme.Faint.Render(x))
		}
	}
	if m.adding() {
		b.WriteString(theme.Faint.Render(theme.Truncate("a file goes with the next prompt; a folder is made readable", w)) + "\n")
	} else {
		b.WriteString(theme.Muted.Render("Model   ") + strings.Join(ms, theme.Faint.Render(" · ")) + theme.Faint.Render("   ^T") + "\n")
	}

	status := theme.Faint.Render(theme.Truncate("⏎ start · tab open folder · ↑↓ choose · type a name to search · esc cancel", w))
	if m.err != "" {
		status = theme.ErrMsg.Render(theme.Truncate(m.err, w))
	} else if m.busy {
		status = theme.OkMsg.Render("starting…")
	} else if dir, ok := m.target(); ok {
		verb := "⏎ start in "
		if m.adding() {
			verb = "⏎ add "
		}
		status = theme.Faint.Render(verb) + theme.Truncate(paths.Tilde(dir), w-20) + theme.Faint.Render(" · esc cancel")
	}
	b.WriteString(status)

	v := tea.NewView(b.String())
	v.MouseMode = tea.MouseModeCellMotion
	v.AltScreen = true
	return v
}
