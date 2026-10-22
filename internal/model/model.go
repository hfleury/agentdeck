// Package model holds the types shared by the daemon and the UI panes.
package model

import (
	"sort"
	"strings"
)

// Session states, in the order a turn normally goes through them.
const (
	StateIdle    = "idle"    // started, no prompt yet
	StateWorking = "working" // a turn is running
	StateTool    = "tool"    // a tool call is running
	StateInput   = "input"   // Claude waits for you (permission prompt, question)
	StateDone    = "done"    // turn finished, waiting for your next prompt
	StateExited  = "exited"  // the claude process ended
)

// Session is one Claude session as shown in the sidebar.
type Session struct {
	ID        string `json:"id"`             // agentdeck's id; also the first Claude session id
	ClaudeID  string `json:"claude_id"`      // current Claude session id (changes after /clear)
	Title     string `json:"title"`          // last prompt, "" until the first one
	Name      string `json:"name,omitempty"` // your own name for it; shown instead of Title
	Group     string `json:"group,omitempty"`
	Order     int    `json:"order"` // position in the list (within its group)
	Cwd       string `json:"cwd"`   // where claude runs (the worktree, if any)
	OriginDir string `json:"origin_dir"`
	Worktree  string `json:"worktree,omitempty"`
	Branch    string `json:"branch,omitempty"` // worktree branch
	Model     string `json:"model,omitempty"`
	Pane      string `json:"pane,omitempty"` // tmux pane id (%12)
	State     string `json:"state"`
	Tool      string `json:"tool,omitempty"` // tool running, when State == StateTool
	ExitCode  int    `json:"exit_code,omitempty"`
	Unread    bool   `json:"unread"`
	CreatedAt int64  `json:"created_at"`

	Git GitInfo `json:"git"`
}

// DisplayTitle is what the list shows: your name for the session, else
// its last prompt, else "new session".
func (s Session) DisplayTitle() string {
	switch {
	case s.Name != "":
		return s.Name
	case s.Title != "":
		return s.Title
	}
	return "new session"
}

// GitInfo is the working-tree summary shown in the subtitle.
type GitInfo struct {
	Branch    string `json:"branch,omitempty"`
	Added     int    `json:"added,omitempty"`
	Deleted   int    `json:"deleted,omitempty"`
	Untracked int    `json:"untracked,omitempty"`
}

// State is the snapshot the daemon pushes to every UI after each change.
type State struct {
	Sessions  []Session `json:"sessions"`
	Active    string    `json:"active"` // session shown in the center pane
	Focused   bool      `json:"focused"`
	Collapsed []string  `json:"collapsed,omitempty"` // folded groups
}

// IsCollapsed reports whether a group is folded in the list.
func (s State) IsCollapsed(group string) bool {
	for _, g := range s.Collapsed {
		if g == group {
			return true
		}
	}
	return false
}

// Groups lists the group names in use, sorted (case-insensitive).
func (s State) Groups() []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s.Sessions {
		if x.Group != "" && !seen[x.Group] {
			seen[x.Group] = true
			out = append(out, x.Group)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// Find returns the session with the given id.
func (s State) Find(id string) (Session, bool) {
	for _, x := range s.Sessions {
		if x.ID == id {
			return x, true
		}
	}
	return Session{}, false
}

// HistoryEntry is one sent prompt.
type HistoryEntry struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
	At        int64  `json:"at"`
}

// Context item kinds.
const (
	KindFile         = "file"
	KindFolder       = "folder"
	KindLines        = "lines"
	KindNote         = "note"
	KindConversation = "conversation" // another session's messages (live)
	KindMemory       = "memory"       // another project's CLAUDE.md + memory files (live)
	KindResult       = "result"       // another session's last answer + changed files (live)
	KindToolOutput   = "tool_output"
)

// ContextItem is something attached to a session for Claude to see.
type ContextItem struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`               // path, or a session id
	Payload   string `json:"payload,omitempty"` // note text, "40-72", tool output
	Title     string `json:"title,omitempty"`   // label for refs that need one (session title, tool call)
	Pinned    bool   `json:"pinned"`            // sent again whenever it changes; else once
	SentHash  string `json:"sent_hash,omitempty"`
	SentAt    int64  `json:"sent_at,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// ItemView is a context item as the panel shows it.
type ItemView struct {
	ContextItem
	Label   string `json:"label"`
	Tokens  int    `json:"tokens"`
	Pending bool   `json:"pending"` // goes with the next prompt
	Error   string `json:"error,omitempty"`
}

// ContextView is a session's context panel.
type ContextView struct {
	SessionID string     `json:"session_id"`
	Items     []ItemView `json:"items"`
	Budget    int        `json:"budget"`
	Next      int        `json:"next"` // tokens going with the next prompt
}

// Tool call statuses.
const (
	ToolRunning = "running"
	ToolOK      = "ok"
	ToolFailed  = "failed"
)

// ToolCall is one entry of the tool-call stream.
type ToolCall struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	Tool      string `json:"tool"`
	Summary   string `json:"summary"`
	Status    string `json:"status"`
	Input     string `json:"input,omitempty"`
	Output    string `json:"output,omitempty"`
	At        int64  `json:"at"`
}
