// Package sidebar is the sessions list pane.
package sidebar

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
	"agentdeck/internal/paths"
	"agentdeck/internal/ui/live"
	"agentdeck/internal/ui/theme"
)

const headerRows = 2 // title + blank

type mode int

const (
	modeList mode = iota
	modeBranch
	modeConfirmClose
	modeName      // your name for a session
	modeGroup     // the group of a session
	modeGroupName // renaming a group
)

// row is one entry of the list: a group header (one line) or a session
// (two lines).
type row struct {
	group   string // header: the group; session: its group
	header  bool
	session int // index into st.Sessions
}

func (r row) lines() int {
	if r.header {
		return 1
	}
	return 2
}

type tickMsg struct{}

type callsMsg struct {
	session string
	calls   []model.ToolCall
}

// Model is the sidebar.
type Model struct {
	calls []model.ToolCall // tool-call stream of the active session

	conn   *live.Conn
	st     model.State
	online bool
	icons  theme.IconSet
	frame  int

	cursor, offset int
	w, h           int
	focused        bool

	rows []row

	mode        mode
	input       textinput.Model
	target      model.Session // session a prompt (branch, close, name, group) refers to
	targetGroup string        // group being renamed

	notice    string
	noticeErr bool
	noticeAt  time.Time
	help      bool
}

// New returns the sidebar model.
func New() Model {
	in := textinput.New()
	in.Prompt = ""
	return Model{icons: theme.DetectIcons(), input: in, focused: true}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(live.Connect(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.input.SetWidth(max(m.w-4, 10))
		m.clamp()
	case tea.FocusMsg:
		m.focused = true
	case tea.BlurMsg:
		m.focused = false
	case tickMsg:
		m.frame++
		if m.notice != "" && time.Since(m.noticeAt) > 6*time.Second {
			m.notice = ""
		}
		return m, tick()
	case live.ConnectedMsg:
		m.conn, m.online = msg.Conn, true
		return m, m.conn.Next()
	case live.DisconnectedMsg:
		m.online = false
		return m, live.Reconnect()
	case live.StateMsg:
		prevActive := m.st.Active
		var keep string // keep the cursor on the same row
		if r, ok := m.row(); ok {
			keep = r.group
			if !r.header {
				keep = "s:" + m.st.Sessions[r.session].ID
			}
		}
		m.st = model.State(msg)
		m.build()
		m.cursorToKey(keep)
		if m.st.Active != prevActive {
			m.cursorTo(m.st.Active)
			m.calls = nil
			m.clamp()
			return m, tea.Batch(m.conn.Next(), m.fetchCalls())
		}
		m.clamp()
		return m, m.conn.Next()
	case live.NoticeMsg:
		m.say(string(msg), false)
		return m, m.conn.Next()
	case live.ToolsMsg:
		cmds := []tea.Cmd{m.conn.Next()}
		if string(msg) == m.st.Active {
			cmds = append(cmds, m.fetchCalls())
		}
		return m, tea.Batch(cmds...)
	case callsMsg:
		if msg.session == m.st.Active {
			m.calls = msg.calls
			m.clamp()
		}
		return m, nil
	case live.HistoryMsg, live.ContextMsg:
		return m, m.conn.Next()
	case live.ResultMsg:
		if msg.Err != nil {
			m.say(msg.Err.Error(), true)
		} else if msg.Notice != "" {
			m.say(msg.Notice, false)
		}
	case tea.MouseClickMsg:
		return m.click(msg.Mouse())
	case tea.MouseWheelMsg:
		switch msg.Mouse().Button {
		case tea.MouseWheelUp:
			m.move(-1)
		case tea.MouseWheelDown:
			m.move(1)
		}
	case tea.KeyPressMsg:
		if m.mode != modeList {
			return m.updatePrompt(msg)
		}
		return m.updateList(msg)
	}
	return m, nil
}

func (m *Model) say(s string, isErr bool) {
	m.notice, m.noticeErr, m.noticeAt = s, isErr, time.Now()
}

// build lays out the list: sessions without a group first, then each
// group (sorted by name) with its sessions unless it is folded.
func (m *Model) build() {
	m.rows = m.rows[:0]
	for i, x := range m.st.Sessions {
		if x.Group == "" {
			m.rows = append(m.rows, row{session: i})
		}
	}
	for _, g := range m.st.Groups() {
		m.rows = append(m.rows, row{group: g, header: true})
		if m.st.IsCollapsed(g) {
			continue
		}
		for i, x := range m.st.Sessions {
			if x.Group == g {
				m.rows = append(m.rows, row{group: g, session: i})
			}
		}
	}
	m.clamp()
}

func (m Model) row() (row, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return row{}, false
	}
	return m.rows[m.cursor], true
}

// current is the session under the cursor (not a group header).
func (m Model) current() (model.Session, bool) {
	r, ok := m.row()
	if !ok || r.header {
		return model.Session{}, false
	}
	return m.st.Sessions[r.session], true
}

// cursorTo puts the cursor on a session, or on its group's header when
// the group is folded.
func (m *Model) cursorTo(id string) {
	for i, r := range m.rows {
		if !r.header && m.st.Sessions[r.session].ID == id {
			m.cursor = i
			m.clamp()
			return
		}
	}
	if s, ok := m.st.Find(id); ok && s.Group != "" {
		m.cursorToKey(s.Group)
	}
}

// cursorToKey finds "s:<id>" (a session) or a group name.
func (m *Model) cursorToKey(key string) {
	if key == "" {
		return
	}
	if id, ok := strings.CutPrefix(key, "s:"); ok {
		m.cursorTo(id)
		return
	}
	for i, r := range m.rows {
		if r.header && r.group == key {
			m.cursor = i
			m.clamp()
			return
		}
	}
}

func (m *Model) move(delta int) {
	m.cursor += delta
	m.clamp()
}

// listLines is the height available to the list.
func (m Model) listLines() int {
	return max(m.h-headerRows-m.footerRows()-m.toolRows(), 2)
}

// toolRows is the height of the tool-call stream: a header and the newest
// calls, up to a third of the pane.
func (m Model) toolRows() int {
	if len(m.calls) == 0 {
		return 0
	}
	room := (m.h - headerRows - m.footerRows()) / 3
	return 1 + min(len(m.calls), max(min(room, 12), 2))
}

func (m Model) fetchCalls() tea.Cmd {
	id := m.st.Active
	if id == "" {
		return nil
	}
	return func() tea.Msg {
		var c []model.ToolCall
		_ = ipc.Call("tools", ipc.HistoryParams{Session: id, Limit: 30}, &c)
		return callsMsg{id, c}
	}
}

// shownCalls are the calls in the stream, oldest first.
func (m Model) shownCalls() []model.ToolCall {
	n := m.toolRows() - 1
	if n <= 0 {
		return nil
	}
	return m.calls[max(len(m.calls)-n, 0):]
}

func showTools(session string, id int64) tea.Cmd {
	return live.Call("tools", "show_tools", ipc.ShareParams{To: session, ID: id})
}

func (m Model) renderCalls(w int) string {
	var b strings.Builder
	b.WriteString(theme.Muted.Render("Tool calls") + "\n")
	for _, c := range m.shownCalls() {
		tool := theme.Truncate(c.Tool, 8)
		room := w - 3 - lipgloss.Width(tool) - 1
		summary := theme.Faint.Render(theme.Truncate(c.Summary, max(room, 0)))
		if c.Status == model.ToolFailed {
			summary = theme.ErrMsg.Render(theme.Truncate(c.Summary, max(room, 0)))
		}
		b.WriteString(" " + m.icons.ToolIcon(c.Status, m.frame) + " " + theme.Bold.Render(tool) + " " + summary + "\n")
	}
	return b.String()
}

func (m Model) footerRows() int {
	n := 3 // notice line + two key-hint lines
	if m.mode != modeList {
		n += 2
	}
	if m.help {
		n += 9
	}
	return n
}

func (m *Model) clamp() {
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	// Scroll until the cursor's row fits.
	for m.offset < m.cursor {
		used := 0
		for i := m.offset; i <= m.cursor && i < len(m.rows); i++ {
			used += m.rows[i].lines()
		}
		if used <= m.listLines() {
			break
		}
		m.offset++
	}
}

// rowAt is the row index at screen line y, or -1.
func (m Model) rowAt(y int) int {
	line := headerRows
	for i := m.offset; i < len(m.rows); i++ {
		n := m.rows[i].lines()
		if line+n > headerRows+m.listLines() {
			break
		}
		if y >= line && y < line+n {
			return i
		}
		line += n
	}
	return -1
}

func (m Model) click(ms tea.Mouse) (tea.Model, tea.Cmd) {
	// A tool call: open it with its input and output.
	if top := m.h - m.footerRows() - m.toolRows(); m.toolRows() > 0 && ms.Y > top && ms.Y < top+m.toolRows() && m.mode == modeList {
		if i := ms.Y - top - 1; i < len(m.shownCalls()) {
			return m, showTools(m.st.Active, m.shownCalls()[i].ID)
		}
		return m, nil
	}
	if m.mode != modeList {
		return m, nil
	}
	idx := m.rowAt(ms.Y)
	if idx < 0 {
		return m, nil
	}
	m.cursor = idx
	r := m.rows[idx]
	if r.header {
		if ms.Button == tea.MouseLeft {
			return m, collapseCmd(r.group, !m.st.IsCollapsed(r.group))
		}
		return m, nil
	}
	s := m.st.Sessions[r.session]
	switch ms.Button {
	case tea.MouseLeft:
		// Open it and put the keyboard in Claude's input box.
		return m, openCmd(s.ID)
	case tea.MouseMiddle:
		m.target = s
		m.mode = modeConfirmClose
	}
	return m, nil
}

// openCmd shows a session and moves the keyboard to it.
func openCmd(id string) tea.Cmd {
	return tea.Sequence(selectCmd(id), focusCmd("center"))
}

func collapseCmd(group string, collapsed bool) tea.Cmd {
	return live.Call("collapse", "collapse", map[string]any{"Group": group, "Collapsed": collapsed})
}

func selectCmd(id string) tea.Cmd {
	return live.Call("select", "select", ipc.IDParams{ID: id})
}

func focusCmd(pane string) tea.Cmd {
	return live.Call("focus", "focus", ipc.FocusParams{Pane: pane})
}

func (m Model) updateList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	cur, ok := m.current()
	r, onRow := m.row()
	if onRow && r.header {
		switch msg.String() {
		case "enter", "l", "space", "h", "right", "left":
			return m, collapseCmd(r.group, !m.st.IsCollapsed(r.group))
		case "e":
			m.mode, m.targetGroup = modeGroupName, r.group
			m.input.SetValue(r.group)
			m.input.Placeholder = ""
			m.input.CursorEnd()
			return m, m.input.Focus()
		}
	}
	switch msg.String() {
	case "j", "down":
		m.move(1)
	case "k", "up":
		m.move(-1)
	case "home":
		m.cursor = 0
		m.clamp()
	case "end":
		m.cursor = len(m.rows) - 1
		m.clamp()
	case "J", "shift+down":
		if ok {
			return m, live.Call("move", "move", map[string]any{"ID": cur.ID, "Delta": 1})
		}
	case "K", "shift+up":
		if ok {
			return m, live.Call("move", "move", map[string]any{"ID": cur.ID, "Delta": -1})
		}
	case "e":
		if ok {
			m.mode, m.target = modeName, cur
			m.input.SetValue(cur.Name)
			m.input.Placeholder = theme.Truncate(cur.Title, 40)
			m.input.CursorEnd()
			return m, m.input.Focus()
		}
	case "g":
		if ok {
			m.mode, m.target = modeGroup, cur
			m.input.SetValue(cur.Group)
			m.input.Placeholder = "new or existing group"
			m.input.CursorEnd()
			return m, m.input.Focus()
		}
	case "enter", "l":
		if ok && cur.State == model.StateExited {
			m.say("resuming…", false)
			return m, tea.Sequence(live.Call("restart", "restart", ipc.IDParams{ID: cur.ID}), openCmd(cur.ID))
		}
		if ok {
			return m, openCmd(cur.ID)
		}
	case "space":
		if ok {
			return m, selectCmd(cur.ID) // show it, stay in the list
		}
	case "tab", "esc":
		return m, focusCmd("center")
	case ":":
		return m, live.Call("palette", "palette", nil)
	case "t":
		if m.st.Active != "" {
			return m, showTools(m.st.Active, 0)
		}
	case "n":
		// The new session joins the group the cursor is in.
		dir, group := "", ""
		if ok {
			dir, group = cur.OriginDir, cur.Group
		} else if onRow && r.header {
			group = r.group
		}
		return m, live.Call("picker", "picker", ipc.NewParams{Dir: dir, Group: group})
	case "w":
		if !ok {
			m.say("select a session in the repository first", true)
			return m, nil
		}
		m.target = cur
		m.mode = modeBranch
		m.input.SetValue("")
		return m, m.input.Focus()
	case "d":
		if ok {
			return m, live.Call("review", "review", ipc.IDParams{ID: cur.ID})
		}
	case "x":
		if ok {
			m.target = cur
			m.mode = modeConfirmClose
		}
	case "R":
		if ok {
			m.say("restarting…", false)
			return m, live.Call("restart", "restart", ipc.IDParams{ID: cur.ID})
		}
	case "?":
		m.help = !m.help
		m.clamp()
	}
	return m, nil
}

func (m Model) updatePrompt(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.mode == modeConfirmClose {
		switch msg.String() {
		case "y", "Y", "enter":
			m.mode = modeList
			return m, live.Call("close", "close", ipc.CloseParams{ID: m.target.ID, RemoveWorktree: true})
		case "k", "K":
			m.mode = modeList
			return m, live.Call("close", "close", ipc.CloseParams{ID: m.target.ID, RemoveWorktree: false})
		case "n", "N", "esc", "q":
			m.mode = modeList
		}
		return m, nil
	}
	switch msg.String() {
	case "esc":
		m.mode = modeList
		m.input.Blur()
		return m, nil
	case "tab":
		if m.mode == modeGroup {
			m.completeGroup()
			return m, nil
		}
	case "enter":
		val := strings.TrimSpace(m.input.Value())
		md := m.mode
		m.mode = modeList
		m.input.Blur()
		switch md {
		case modeName:
			return m, live.Call("rename", "rename", map[string]string{"ID": m.target.ID, "Title": val})
		case modeGroup:
			return m, live.Call("group", "set_group", map[string]string{"ID": m.target.ID, "Group": val})
		case modeGroupName:
			if val == "" {
				return m, nil
			}
			return m, live.Call("group", "rename_group", map[string]string{"Old": m.targetGroup, "New": val})
		}
		m.say("creating worktree…", false)
		return m, live.Call("new", "new", ipc.NewParams{Dir: m.target.OriginDir, Branch: val, Model: m.target.Model, Group: m.target.Group})
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// completeGroup completes the typed group from the existing ones; again
// cycles through the matches.
func (m *Model) completeGroup() {
	val := strings.ToLower(m.input.Value())
	var matches []string
	for _, g := range m.st.Groups() {
		if strings.HasPrefix(strings.ToLower(g), val) {
			matches = append(matches, g)
		}
	}
	if len(matches) == 0 {
		matches = m.st.Groups() // nothing matches: cycle through all
	}
	if len(matches) == 0 {
		return
	}
	next := matches[0]
	for i, g := range matches {
		if strings.EqualFold(g, m.input.Value()) {
			next = matches[(i+1)%len(matches)]
		}
	}
	m.input.SetValue(next)
	m.input.CursorEnd()
}

// View renders the pane.
func (m Model) View() tea.View {
	var b strings.Builder
	w := max(m.w, 10)

	head := theme.Title.Render("Sessions")
	if n := len(m.st.Sessions); n > 0 {
		head += theme.Faint.Render(fmt.Sprintf(" %d", n))
	}
	if !m.online {
		head += theme.ErrMsg.Render("  daemon offline")
	}
	b.WriteString(head + "\n\n")

	used := headerRows
	if len(m.st.Sessions) == 0 {
		b.WriteString(theme.Muted.Render(" No sessions yet.\n Press n to start one.") + "\n")
		used += 2
	}
	for i := m.offset; i < len(m.rows); i++ {
		r := m.rows[i]
		if used+r.lines() > headerRows+m.listLines() {
			break
		}
		if r.header {
			b.WriteString(m.renderGroup(r.group, i == m.cursor, w) + "\n")
		} else {
			b.WriteString(m.renderItem(m.st.Sessions[r.session], i == m.cursor, w))
		}
		used += r.lines()
	}
	for ; used < m.h-m.footerRows()-m.toolRows(); used++ {
		b.WriteString("\n")
	}
	if m.toolRows() > 0 {
		b.WriteString(m.renderCalls(w))
	}
	b.WriteString(m.footer(w))

	v := tea.NewView(b.String())
	v.MouseMode = tea.MouseModeCellMotion
	v.ReportFocus = true
	v.AltScreen = true
	return v
}

// renderGroup is a group header: ▾ open, ▸ folded with a count and the
// icons of members that need you or are working.
func (m Model) renderGroup(g string, isCursor bool, w int) string {
	marker := " "
	if isCursor && m.focused {
		marker = lipgloss.NewStyle().Foreground(theme.Accent).Render("›")
	}
	folded := m.st.IsCollapsed(g)
	arrow := "▾"
	if folded {
		arrow = "▸"
	}
	n, active := 0, false
	var states []string
	for _, s := range m.st.Sessions {
		if s.Group != g {
			continue
		}
		n++
		active = active || s.ID == m.st.Active
		if folded && (s.State == model.StateInput || s.State == model.StateWorking || s.State == model.StateTool || s.Unread) {
			states = append(states, m.icons.Icon(s.State, m.frame))
		}
	}
	tail := theme.Faint.Render(fmt.Sprintf(" %d", n))
	if len(states) > 0 {
		tail += " " + strings.Join(states, "")
	}
	room := w - 3 - lipgloss.Width(tail)
	line := marker + theme.Muted.Render(arrow) + " " + lipgloss.NewStyle().Bold(true).Foreground(theme.Accent).Render(theme.Truncate(g, max(room, 4))) + tail
	if folded && active {
		return lipgloss.NewStyle().Background(theme.SelBg).Width(w).Render(line)
	}
	return line
}

func (m Model) renderItem(s model.Session, isCursor bool, w int) string {
	active := s.ID == m.st.Active
	marker := " "
	if isCursor && m.focused {
		marker = lipgloss.NewStyle().Foreground(theme.Accent).Render("›")
	}
	title := s.DisplayTitle()
	titleStyle := lipgloss.NewStyle()
	if s.Unread {
		titleStyle = titleStyle.Bold(true)
	}
	if s.Name == "" && s.Title == "" {
		titleStyle = titleStyle.Foreground(theme.Gray)
	}
	line1 := marker + m.icons.Icon(s.State, m.frame) + " " + titleStyle.Render(theme.Truncate(title, w-4))

	branch := s.Git.Branch
	if s.Branch != "" {
		branch = s.Branch
	}
	var info string
	if s.State == model.StateExited {
		info = "  exited · ⏎ resumes"
	} else if branch != "" {
		info = "  " + branch
		if s.Git.Added > 0 || s.Git.Deleted > 0 {
			info += fmt.Sprintf(" +%d −%d", s.Git.Added, s.Git.Deleted)
		}
		if s.Git.Untracked > 0 {
			info += fmt.Sprintf(" ?%d", s.Git.Untracked)
		}
	}
	// The folder gives way first (shortened from the left), so the
	// branch, changes and exit hint stay visible.
	const lead = "  └ "
	folder := theme.TruncateLeft(paths.Tilde(s.OriginDir), max(w-len([]rune(lead))-lipgloss.Width(info), 6))
	line2 := theme.Faint.Render(theme.Truncate(lead+folder+info, w))

	if active {
		bg := lipgloss.NewStyle().Background(theme.SelBg).Width(w)
		return bg.Render(line1) + "\n" + bg.Render(line2) + "\n"
	}
	return line1 + "\n" + line2 + "\n"
}

func (m Model) footer(w int) string {
	var b strings.Builder
	switch m.mode {
	case modeBranch:
		b.WriteString(theme.Bold.Render("Worktree branch") + theme.Faint.Render(" (new or existing)") + "\n")
		b.WriteString("> " + m.input.View() + "\n")
	case modeName:
		b.WriteString(theme.Bold.Render("Name") + theme.Faint.Render(" (empty: last prompt)") + "\n")
		b.WriteString("> " + m.input.View() + "\n")
	case modeGroup:
		b.WriteString(theme.Bold.Render("Group") + theme.Faint.Render(" tab: existing · empty: none") + "\n")
		b.WriteString("> " + m.input.View() + "\n")
	case modeGroupName:
		b.WriteString(theme.Bold.Render("Rename group") + "\n")
		b.WriteString("> " + m.input.View() + "\n")
	case modeConfirmClose:
		t := m.target.DisplayTitle()
		b.WriteString(theme.Bold.Render("Close ") + theme.Truncate(t, w-7) + "?\n")
		if m.target.Worktree != "" {
			b.WriteString(theme.Faint.Render("y close + remove worktree · k keep it · n no") + "\n")
		} else {
			b.WriteString(theme.Faint.Render("y close · n cancel") + "\n")
		}
	}
	notice := ""
	if m.notice != "" {
		style := theme.OkMsg
		if m.noticeErr {
			style = theme.ErrMsg
		}
		notice = style.Render(theme.Truncate(m.notice, w))
	}
	b.WriteString(notice + "\n")
	if m.help {
		for _, l := range []string{
			"j/k move  ⏎/click open",
			"space show, stay here",
			"n new  w worktree",
			"e name  g group  J/K move",
			"⏎ on a group: fold/unfold",
			"d diff  x close  R restart",
			"t tool calls (or click one)",
			"Alt-K or : commands",
			"^A here/back  ^G context",
		} {
			b.WriteString(theme.Muted.Render(theme.Truncate(l, w)) + "\n")
		}
	}
	b.WriteString(theme.Faint.Render(theme.Truncate("n new  e name  g group", w)) + "\n")
	b.WriteString(theme.Faint.Render(theme.Truncate("x close  Alt-K cmds  ? help", w)))
	return b.String()
}
