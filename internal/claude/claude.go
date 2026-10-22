// Package claude knows how to start Claude Code, wire its hooks to
// agentdeck, find its transcripts, and list its slash commands.
package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"agentdeck/internal/paths"
	"agentdeck/internal/tmux"
)

// HookEvents are the Claude Code hook events agentdeck listens to.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Notification", "Stop", "SessionEnd"}

// WriteHookSettings writes the --settings file that sends every hook event
// to `<self> hook <event>`. The user's own settings files are not touched.
func WriteHookSettings(path, self string) error {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	type matcher struct {
		Matcher string `json:"matcher,omitempty"`
		Hooks   []hook `json:"hooks"`
	}
	hooks := map[string][]matcher{}
	for _, ev := range HookEvents {
		m := matcher{Hooks: []hook{{Type: "command", Command: tmux.Quote(self) + " hook " + ev, Timeout: 5}}}
		if strings.HasPrefix(ev, "PreToolUse") || strings.HasPrefix(ev, "PostToolUse") {
			m.Matcher = "*"
		}
		hooks[ev] = []matcher{m}
	}
	b, err := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Start describes how to launch one session.
type Start struct {
	ID       string // agentdeck id, used as --session-id for a new conversation
	ResumeID string // non-empty: --resume this Claude session id
	Model    string
	Settings string
	AddDirs  []string // extra folders the session may read (--add-dir)
}

// StartCommand builds the shell command tmux runs in the session's pane.
func StartCommand(s Start) string {
	args := []string{"exec", tmux.Quote(paths.ClaudeBin())}
	if s.ResumeID != "" {
		args = append(args, "--resume", tmux.Quote(s.ResumeID))
	} else {
		args = append(args, "--session-id", tmux.Quote(s.ID))
	}
	if s.Settings != "" {
		args = append(args, "--settings", tmux.Quote(s.Settings))
	}
	if s.Model != "" {
		args = append(args, "--model", tmux.Quote(s.Model))
	}
	for _, d := range s.AddDirs {
		args = append(args, "--add-dir", tmux.Quote(d))
	}
	return strings.Join(args, " ")
}

// TranscriptExists reports whether Claude saved a transcript for the
// session id, i.e. whether it can be resumed.
func TranscriptExists(id string) bool {
	if id == "" {
		return false
	}
	m, _ := filepath.Glob(filepath.Join(paths.ClaudeHome(), "projects", "*", id+".jsonl"))
	return len(m) > 0
}

// HookPayload holds the hook input fields agentdeck uses.
type HookPayload struct {
	SessionID        string          `json:"session_id"`
	TranscriptPath   string          `json:"transcript_path"`
	Cwd              string          `json:"cwd"`
	HookEventName    string          `json:"hook_event_name"`
	Prompt           string          `json:"prompt"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	ToolUseID        string          `json:"tool_use_id"`
	ToolResponse     json.RawMessage `json:"tool_response"`
	Error            string          `json:"error"`
	Message          string          `json:"message"`
	NotificationType string          `json:"notification_type"`
	Source           string          `json:"source"`
}

// ToolSummary is a one-line description of a tool call.
func (p HookPayload) ToolSummary() string {
	var in map[string]any
	_ = json.Unmarshal(p.ToolInput, &in)
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "description", "prompt"} {
		if v, ok := in[k].(string); ok && v != "" {
			return firstLine(v, 120)
		}
	}
	return ""
}

var systemPromptRE = regexp.MustCompile(`^\s*<[a-zA-Z][a-zA-Z0-9_-]*[\s>]`)

// IsSystemPrompt reports prompts Claude Code submits by itself, such as
// <task-notification> when a background task ends. They are not yours:
// not a title, not history.
func IsSystemPrompt(s string) bool {
	return systemPromptRE.MatchString(s) && !strings.HasPrefix(strings.TrimSpace(s), "<pasted_content")
}

// ToolInputText is the tool input as readable text.
func (p HookPayload) ToolInputText() string {
	return prettyJSON(p.ToolInput)
}

// ToolOutputText is the tool result as readable text: stdout and stderr
// for shell commands, the error for a failed call, otherwise the JSON.
func (p HookPayload) ToolOutputText() string {
	if p.Error != "" {
		return p.Error
	}
	var r map[string]any
	if json.Unmarshal(p.ToolResponse, &r) == nil {
		if f, ok := r["file"].(map[string]any); ok { // Read
			if c, ok := f["content"].(string); ok {
				return c
			}
		}
		out, _ := r["stdout"].(string)
		errOut, _ := r["stderr"].(string)
		if out != "" || errOut != "" {
			return strings.TrimRight(strings.TrimRight(out, "\n")+"\n"+errOut, "\n")
		}
	}
	var s string
	if json.Unmarshal(p.ToolResponse, &s) == nil {
		return s
	}
	return prettyJSON(p.ToolResponse)
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// CleanPrompt turns a submitted prompt into a sidebar title: pasted-content
// wrappers removed, whitespace collapsed.
func CleanPrompt(s string) string {
	for _, tag := range []string{"pasted_content"} {
		for {
			i := strings.Index(s, "<"+tag)
			if i < 0 {
				break
			}
			j := strings.Index(s[i:], "</"+tag+">")
			if j < 0 {
				s = s[:i]
				break
			}
			s = s[:i] + "[pasted]" + s[i+j+len("</"+tag+">"):]
		}
	}
	return strings.Join(strings.Fields(s), " ")
}

func firstLine(s string, max int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// TranscriptPath returns the transcript file of a Claude session id, or "".
func TranscriptPath(id string) string {
	if id == "" {
		return ""
	}
	m, _ := filepath.Glob(filepath.Join(paths.ClaudeHome(), "projects", "*", id+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

// ProjectDir is ~/.claude/projects/<folder with non-alphanumerics as ->,
// where Claude keeps a folder's transcripts and memory.
func ProjectDir(cwd string) string {
	enc := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, cwd)
	return filepath.Join(paths.ClaudeHome(), "projects", enc)
}
