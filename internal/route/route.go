// Package route decides what happens to text sent from the prompt: an
// agentdeck command, a Claude slash command, or a normal message. The
// decision depends only on the text at send time, so a pasted command
// behaves exactly like a typed one.
package route

import (
	"regexp"
	"strings"
)

// Kind of a routed prompt.
type Kind string

const (
	Empty  Kind = "empty"
	App    Kind = "app"    // run by agentdeck
	Claude Kind = "claude" // typed into Claude as a slash command
	Text   Kind = "text"   // pasted into Claude as a message
)

// AppCommand is a command run by agentdeck itself.
type AppCommand struct {
	Name    string
	ArgHint string
	Desc    string
}

// AppCommands are the deck's own commands. Names are chosen not to clash
// with Claude's; on a clash Claude wins and `//name` picks the deck's.
var AppCommands = []AppCommand{
	{Name: "new", ArgHint: "[folder]", Desc: "start a new session"},
	{Name: "worktree", ArgHint: "<branch>", Desc: "new session in a git worktree of this repo"},
	{Name: "close", Desc: "close this session"},
	{Name: "restart", Desc: "restart (resume) this session"},
	{Name: "changes", Desc: "review this session's uncommitted changes"},
	{Name: "title", ArgHint: "[name]", Desc: "name this session (no name: its last prompt)"},
	{Name: "group", ArgHint: "[group]", Desc: "put this session in a group (no name: no group)"},
}

// IsApp reports whether name is a deck command.
func IsApp(name string) bool {
	for _, c := range AppCommands {
		if c.Name == name {
			return true
		}
	}
	return false
}

// Route is the decision for one prompt.
type Route struct {
	Kind Kind
	Name string // command name without slashes (App, Claude)
	Args string // App: everything after the name
	Text string // Claude, Text: what to send
}

// Classify routes raw prompt text. isBuiltin reports Claude's built-in
// commands; it is only used to resolve clashes with deck commands.
func Classify(raw string, isBuiltin func(string) bool) Route {
	t := Normalize(raw)
	if t == "" {
		return Route{Kind: Empty}
	}
	if strings.HasPrefix(t, `\/`) {
		return Route{Kind: Text, Text: t[1:]}
	}
	if !strings.HasPrefix(t, "/") {
		return Route{Kind: Text, Text: t}
	}
	first, rest, _ := strings.Cut(t, "\n")
	word, args, _ := strings.Cut(first, " ")
	args = strings.TrimSpace(args)
	if rest != "" {
		args = strings.TrimSpace(args + "\n" + rest)
	}

	if strings.HasPrefix(word, "//") {
		name := word[2:]
		if IsApp(name) {
			return Route{Kind: App, Name: name, Args: args}
		}
		return Route{Kind: Text, Text: t}
	}
	name := word[1:]
	// "/" alone, or a path like /etc/hosts: not a command.
	if name == "" || strings.Contains(name, "/") {
		return Route{Kind: Text, Text: t}
	}
	if IsApp(name) && (isBuiltin == nil || !isBuiltin(name)) {
		return Route{Kind: App, Name: name, Args: args}
	}
	return Route{Kind: Claude, Name: name, Text: t}
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?<=>]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// Normalize cleans text the way a paste is cleaned: line endings unified,
// escape sequences and control characters removed, outer whitespace trimmed.
func Normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}
