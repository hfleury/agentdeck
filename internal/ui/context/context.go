// Package context is the right-hand pane: what is attached to the
// selected session, and the keys to add, pin, share and remove items.
package context

import (
	"fmt"
	"strconv"
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

type mode int

const (
	modeList mode = iota
	modeNote
	modeLines
	modeBudget
	modeChoose // pick another session for an action
)

const headerRows = 3 // title, folder line, blank

// refreshMsg re-reads the panel: files and other sessions change without
// an event.
type refreshMsg struct{}

func refresh() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return refreshMsg{} })
}

type viewMsg struct {
	v   model.ContextView
	err error
}

// row is one line of the list: a section header or an item.
type row struct {
	header string
	item   int // index into view.Items, when header == ""
}

// Model is the context pane.
type Model struct {
	conn   *live.Conn
	st     model.State
	view   model.ContextView
	online bool
	icons  theme.IconSet
	w, h   int

	sel, offset int // index into items order (not rows)
	order       []int
	rows        []row

	mode     mode
	input    textinput.Model
	action   string // for modeChoose: conversation, memory, result, copy, share, shareall
	choices  []model.Session
	choice   int
	help     bool
	notice   string
	isErr    bool
	noticeAt time.Time
	lastAt   time.Time
	lastIdx  int
}

func New() Model {
	in := textinput.New()
	in.Prompt = ""
	return Model{icons: theme.DetectIcons(), input: in, lastIdx: -1}
}

func (m Model) Init() tea.Cmd { return tea.Batch(live.Connect(), refresh()) }

func (m Model) fetch() tea.Cmd {
	id := m.st.Active
	return func() tea.Msg {
		var v model.ContextView
		err := ipc.Call("context", ipc.IDParams{ID: id}, &v)
		return viewMsg{v, err}
	}
}

func (m *Model) say(s string, isErr bool) {
	m.notice, m.isErr, m.noticeAt = s, isErr, time.Now()
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.input.SetWidth(max(m.w-3, 5))
		m.clamp()
	case live.ConnectedMsg:
		m.conn, m.online = msg.Conn, true
		return m, m.conn.Next()
	case live.DisconnectedMsg:
		m.online = false
		return m, live.Reconnect()
	case live.StateMsg:
		prev := m.st.Active
		m.st = model.State(msg)
		cmds := []tea.Cmd{m.conn.Next()}
		if m.st.Active != prev || m.view.SessionID == "" {
			m.sel, m.offset, m.mode = 0, 0, modeList
			cmds = append(cmds, m.fetch())
		}
		return m, tea.Batch(cmds...)
	case live.ContextMsg:
		cmds := []tea.Cmd{m.conn.Next()}
		if string(msg) == m.st.Active {
			cmds = append(cmds, m.fetch())
		}
		return m, tea.Batch(cmds...)
	case live.HistoryMsg, live.ToolsMsg:
		return m, m.conn.Next()
	case live.NoticeMsg:
		return m, m.conn.Next()
	case refreshMsg:
		if m.online && m.st.Active != "" && m.mode == modeList {
			return m, tea.Batch(m.fetch(), refresh())
		}
		return m, refresh()
	case viewMsg:
		if msg.err == nil {
			m.view = msg.v
			m.build()
		} else {
			m.view = model.ContextView{}
			m.build()
		}
	case live.ResultMsg:
		if msg.Err != nil {
			m.say(msg.Err.Error(), true)
		} else if msg.Notice != "" {
			m.say(msg.Notice, false)
		}
		return m, m.fetch()
	case tea.MouseClickMsg:
		return m.click(msg.Mouse())
	case tea.MouseWheelMsg:
		if msg.Mouse().Button == tea.MouseWheelUp {
			m.move(-1)
		} else if msg.Mouse().Button == tea.MouseWheelDown {
			m.move(1)
		}
	case tea.KeyPressMsg:
		switch m.mode {
		case modeList:
			return m.updateList(msg)
		case modeChoose:
			return m.updateChoose(msg)
		default:
			return m.updateInput(msg)
		}
	}
	return m, nil
}

// build groups the items into sections: Pinned, Next prompt, Sent.
func (m *Model) build() {
	var pinned, next, sent []int
	for i, it := range m.view.Items {
		switch {
		case it.Pinned:
			pinned = append(pinned, i)
		case it.SentAt == 0:
			next = append(next, i)
		default:
			sent = append(sent, i)
		}
	}
	m.rows, m.order = nil, nil
	for _, sec := range []struct {
		name string
		idx  []int
	}{{"Pinned", pinned}, {"Next prompt", next}, {"Sent", sent}} {
		if len(sec.idx) == 0 {
			continue
		}
		m.rows = append(m.rows, row{header: sec.name})
		for _, i := range sec.idx {
			m.rows = append(m.rows, row{item: i})
			m.order = append(m.order, i)
		}
	}
	m.clamp()
}

func (m Model) current() (model.ItemView, bool) {
	if m.sel < 0 || m.sel >= len(m.order) {
		return model.ItemView{}, false
	}
	return m.view.Items[m.order[m.sel]], true
}

// rowOf is the row index of the selected item.
func (m Model) rowOf(sel int) int {
	n := -1
	for r, x := range m.rows {
		if x.header == "" {
			n++
			if n == sel {
				return r
			}
		}
	}
	return 0
}

func (m Model) listRows() int { return max(m.h-headerRows-m.footerRows(), 1) }

func (m Model) footerRows() int {
	n := 4 // notice + three hint lines
	if m.mode == modeNote || m.mode == modeLines || m.mode == modeBudget {
		n += 2
	}
	if m.help {
		n += 7
	}
	return n
}

func (m *Model) move(d int) {
	m.sel += d
	m.clamp()
}

func (m *Model) clamp() {
	if m.sel >= len(m.order) {
		m.sel = len(m.order) - 1
	}
	if m.sel < 0 {
		m.sel = 0
	}
	r := m.rowOf(m.sel)
	if r > 0 && m.rows[r-1].header != "" {
		r-- // keep the section header in view
	}
	if r < m.offset {
		m.offset = r
	}
	if rr := m.rowOf(m.sel); rr >= m.offset+m.listRows() {
		m.offset = rr - m.listRows() + 1
	}
}

func call(op, method string, params any) tea.Cmd { return live.Call(op, method, params) }

func (m Model) updateList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	it, ok := m.current()
	sid := m.st.Active
	if sid == "" && msg.String() != "esc" && msg.String() != "?" {
		m.say("no session selected", true)
		return m, nil
	}
	switch msg.String() {
	case "j", "down":
		m.move(1)
	case "k", "up":
		m.move(-1)
	case "g", "home":
		m.sel = 0
		m.clamp()
	case "G", "end":
		m.sel = len(m.order) - 1
		m.clamp()
	case "enter", "v":
		if ok {
			return m, call("show", "show", ipc.ItemParams{ID: it.ID})
		}
	case "a":
		return m, call("add", "add_picker", ipc.IDParams{ID: sid})
	case "n":
		return m.startInput(modeNote, "")
	case "l":
		return m.startInput(modeLines, "")
	case "b":
		return m.startInput(modeBudget, strconv.Itoa(m.view.Budget))
	case "h":
		return m.choose("conversation")
	case "m":
		return m.choose("memory")
	case "r":
		return m.choose("result")
	case "c":
		return m.choose("copy")
	case "s":
		if ok {
			return m.choose("share")
		}
	case "S":
		if len(m.order) > 0 {
			return m.choose("shareall")
		}
	case "p":
		if ok {
			if it.Pinned {
				m.say("unpinned: it goes once", false)
			} else {
				m.say("pinned: sent again when it changes", false)
			}
			return m, call("pin", "context_pin", ipc.ItemParams{ID: it.ID, Pinned: !it.Pinned})
		}
	case "R":
		if ok {
			m.say("goes again with the next prompt", false)
			return m, call("resend", "context_resend", ipc.ItemParams{ID: it.ID})
		}
	case "x", "delete":
		if ok {
			if it.SentAt != 0 {
				m.say("removed (Claude still knows what it saw)", false)
			} else {
				m.say("removed", false)
			}
			return m, call("remove", "context_remove", ipc.ItemParams{ID: it.ID})
		}
	case "esc", "tab":
		return m, call("focus", "focus", ipc.FocusParams{Pane: "center"})
	case "?":
		m.help = !m.help
		m.clamp()
	}
	return m, nil
}

func (m Model) startInput(md mode, val string) (tea.Model, tea.Cmd) {
	m.mode = md
	m.input.SetValue(val)
	m.input.CursorEnd()
	switch md {
	case modeNote:
		m.input.Placeholder = "e.g. we use sqlc, not GORM"
	case modeLines:
		m.input.Placeholder = "path:40-72"
	default:
		m.input.Placeholder = ""
	}
	return m, m.input.Focus()
}

func (m Model) updateInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		m.input.Blur()
		return m, nil
	case "enter":
		val := strings.TrimSpace(m.input.Value())
		md := m.mode
		m.mode = modeList
		m.input.Blur()
		if val == "" {
			return m, nil
		}
		sid := m.st.Active
		switch md {
		case modeNote:
			return m, call("add", "context_add", ipc.AddParams{Session: sid, Kind: model.KindNote, Payload: val})
		case modeLines:
			path, rng, ok := cutRange(val)
			if !ok {
				m.say("write it as path:40-72", true)
				return m, nil
			}
			return m, call("add", "context_add", ipc.AddParams{Session: sid, Kind: model.KindLines, Ref: path, Payload: rng})
		case modeBudget:
			n, err := strconv.Atoi(strings.TrimSuffix(strings.ToLower(val), "k"))
			if err != nil {
				m.say("a number of tokens, e.g. 20000", true)
				return m, nil
			}
			if strings.HasSuffix(strings.ToLower(val), "k") {
				n *= 1000
			}
			return m, call("budget", "budget", map[string]any{"session": sid, "budget": n})
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// cutRange splits "path:40-72".
func cutRange(s string) (string, string, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// choose lists the other sessions for an action.
func (m Model) choose(action string) (tea.Model, tea.Cmd) {
	m.choices = nil
	for _, s := range m.st.Sessions {
		if s.ID != m.st.Active {
			m.choices = append(m.choices, s)
		}
	}
	if len(m.choices) == 0 {
		m.say("there is no other session", true)
		return m, nil
	}
	m.mode, m.action, m.choice = modeChoose, action, 0
	return m, nil
}

func (m Model) updateChoose(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		m.choice = min(m.choice+1, len(m.choices)-1)
	case "k", "up":
		m.choice = max(m.choice-1, 0)
	case "esc", "q":
		m.mode = modeList
	case "enter":
		m.mode = modeList
		return m, m.doChoice(m.choices[m.choice])
	}
	return m, nil
}

func (m Model) doChoice(other model.Session) tea.Cmd {
	sid := m.st.Active
	switch m.action {
	case "conversation":
		return call("add", "context_add", ipc.AddParams{Session: sid, Kind: model.KindConversation, Ref: other.ID})
	case "memory":
		return call("add", "context_add", ipc.AddParams{Session: sid, Kind: model.KindMemory, Ref: other.ID})
	case "result":
		return call("add", "context_add", ipc.AddParams{Session: sid, Kind: model.KindResult, Ref: other.ID})
	case "copy":
		return call("share", "context_share", ipc.ShareParams{From: other.ID, To: sid})
	case "share":
		if it, ok := m.current(); ok {
			return call("share", "context_share", ipc.ShareParams{ID: it.ID, To: other.ID})
		}
	case "shareall":
		return call("share", "context_share", ipc.ShareParams{From: sid, To: other.ID})
	}
	return nil
}

func (m Model) click(ms tea.Mouse) (tea.Model, tea.Cmd) {
	if ms.Button != tea.MouseLeft {
		return m, nil
	}
	r := m.offset + ms.Y - headerRows
	if m.mode == modeChoose {
		idx := ms.Y - headerRows - 1
		if idx >= 0 && idx < len(m.choices) {
			m.mode = modeList
			return m, m.doChoice(m.choices[idx])
		}
		return m, nil
	}
	if ms.Y < headerRows || r >= len(m.rows) || m.rows[r].header != "" {
		return m, nil
	}
	n := -1
	for i := 0; i <= r; i++ {
		if m.rows[i].header == "" {
			n++
		}
	}
	double := n == m.lastIdx && time.Since(m.lastAt) < 400*time.Millisecond
	m.sel, m.lastIdx, m.lastAt = n, n, time.Now()
	if double {
		if it, ok := m.current(); ok {
			return m, call("show", "show", ipc.ItemParams{ID: it.ID})
		}
	}
	return m, nil
}

// --- view ---------------------------------------------------------------------

func (m Model) View() tea.View {
	w := max(m.w, 10)
	var b strings.Builder
	head := theme.Title.Render("Context")
	if m.view.SessionID != "" {
		head += theme.Faint.Render(fmt.Sprintf("  %s/%s next", theme.Kilo(m.view.Next), theme.Kilo(m.view.Budget)))
	}
	b.WriteString(theme.Truncate(head, w+40) + "\n")
	s, ok := m.st.Find(m.st.Active)
	switch {
	case !m.online:
		b.WriteString(theme.ErrMsg.Render("daemon offline") + "\n\n")
	case !ok:
		b.WriteString(theme.Muted.Render("No session selected.") + "\n\n")
	default:
		info := paths.Tilde(s.Cwd)
		branch := s.Git.Branch
		if s.Branch != "" {
			branch = s.Branch
		}
		if branch != "" {
			info += " · " + branch
		}
		if s.Model != "" {
			info += " · " + s.Model
		}
		b.WriteString(theme.Faint.Render(theme.TruncateLeft(info, w)) + "\n\n")
	}
	used := headerRows
	if m.mode == modeChoose {
		used += m.viewChoose(&b, w)
	} else if len(m.rows) == 0 && ok {
		for _, l := range []string{"Nothing attached yet.", "", "a  file or folder", "n  note", "h  another session's chat", "?  all keys"} {
			b.WriteString(theme.Muted.Render(theme.Truncate(l, w)) + "\n")
			used++
		}
	} else {
		for i := m.offset; i < len(m.rows) && i < m.offset+m.listRows(); i++ {
			b.WriteString(m.viewRow(m.rows[i], w) + "\n")
			used++
		}
	}
	for ; used < m.h-m.footerRows(); used++ {
		b.WriteString("\n")
	}
	b.WriteString(m.footer(w))
	v := tea.NewView(b.String())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m Model) viewRow(r row, w int) string {
	if r.header != "" {
		return theme.Muted.Render(r.header)
	}
	it := m.view.Items[r.item]
	selected := len(m.order) > 0 && m.order[m.sel] == r.item
	marker := " "
	if selected {
		marker = lipgloss.NewStyle().Foreground(theme.Accent).Render("›")
	}
	right := theme.Kilo(it.Tokens)
	style := lipgloss.NewStyle()
	switch {
	case it.Error != "":
		right, style = it.Error, lipgloss.NewStyle().Foreground(theme.Red)
	case it.Pinned && it.Pending:
		right = "↑" + right
	case !it.Pinned && it.SentAt != 0:
		style = theme.Faint
	}
	icon := m.icons.KindIcon(it.Kind)
	room := w - 4 - lipgloss.Width(right) - 1
	label := theme.Truncate(it.Label, max(room, 4))
	line := marker + icon + " " + style.Render(label)
	pad := w - lipgloss.Width(line) - lipgloss.Width(right)
	line += strings.Repeat(" ", max(pad, 1)) + style.Render(right)
	if selected {
		return lipgloss.NewStyle().Background(theme.SelBg).Width(w).Render(line)
	}
	return line
}

func (m Model) viewChoose(b *strings.Builder, w int) int {
	title := map[string]string{
		"conversation": "Add the chat of:", "memory": "Add the memory of:", "result": "Add the result of:",
		"copy": "Copy all items of:", "share": "Share this item with:", "shareall": "Share all items with:",
	}[m.action]
	b.WriteString(theme.Bold.Render(theme.Truncate(title, w)) + "\n")
	n := 1
	for i, s := range m.choices {
		t := s.DisplayTitle()
		line := "  " + theme.Truncate(t, w-2)
		if i == m.choice {
			line = lipgloss.NewStyle().Background(theme.SelBg).Width(w).Render(
				lipgloss.NewStyle().Foreground(theme.Accent).Render("› ") + theme.Truncate(t, w-2))
		}
		b.WriteString(line + "\n")
		n++
		if n >= m.listRows() {
			break
		}
	}
	return n
}

func (m Model) footer(w int) string {
	var b strings.Builder
	switch m.mode {
	case modeNote:
		b.WriteString(theme.Bold.Render("Note") + theme.Faint.Render(" ⏎ add") + "\n" + m.input.View() + "\n")
	case modeLines:
		b.WriteString(theme.Bold.Render("Lines") + theme.Faint.Render(" path:from-to") + "\n" + m.input.View() + "\n")
	case modeBudget:
		b.WriteString(theme.Bold.Render("Budget") + theme.Faint.Render(" tokens") + "\n" + m.input.View() + "\n")
	}
	if m.notice != "" && time.Since(m.noticeAt) < 6*time.Second {
		style := theme.OkMsg
		if m.isErr {
			style = theme.ErrMsg
		}
		b.WriteString(style.Render(theme.Truncate(m.notice, w)))
	}
	b.WriteString("\n")
	if m.help {
		for _, l := range []string{
			"l  lines (path:40-72)",
			"m  another project's memory",
			"r  another session's result",
			"c  copy another's items",
			"s/S share item/all",
			"R  send again  b budget",
			"↑ pinned, will resend",
		} {
			b.WriteString(theme.Muted.Render(theme.Truncate(l, w)) + "\n")
		}
	}
	if m.mode == modeChoose {
		b.WriteString(theme.Faint.Render(theme.Truncate("⏎ choose · esc cancel", w)) + "\n\n")
		return b.String()
	}
	b.WriteString(theme.Faint.Render(theme.Truncate("a file  n note  h chat", w)) + "\n")
	b.WriteString(theme.Faint.Render(theme.Truncate("p pin  x remove  ⏎ view", w)) + "\n")
	b.WriteString(theme.Faint.Render(theme.Truncate("? more  ^G back", w)))
	return b.String()
}
