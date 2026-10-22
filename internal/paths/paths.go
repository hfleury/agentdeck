// Package paths resolves where agentdeck keeps its files, following the XDG
// base directory spec. Every location can be overridden with an environment
// variable, which the tests use to run fully isolated.
package paths

import (
	"os"
	"path/filepath"
)

// StateDir holds the SQLite database, generated settings and logs.
// $AGENTDECK_STATE_DIR, else $XDG_STATE_HOME/agentdeck, else ~/.local/state/agentdeck.
func StateDir() string {
	if d := os.Getenv("AGENTDECK_STATE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "agentdeck")
	}
	return filepath.Join(home(), ".local", "state", "agentdeck")
}

// Socket is the daemon's Unix socket.
// $AGENTDECK_SOCK, else $XDG_RUNTIME_DIR/agentdeck/agentdeck.sock, else in StateDir.
func Socket() string {
	if s := os.Getenv("AGENTDECK_SOCK"); s != "" {
		return s
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "agentdeck", "agentdeck.sock")
	}
	return filepath.Join(StateDir(), "agentdeck.sock")
}

// TmuxServer is the name of agentdeck's own tmux server (tmux -L <name>).
func TmuxServer() string {
	if s := os.Getenv("AGENTDECK_TMUX"); s != "" {
		return s
	}
	return "agentdeck"
}

// ClaudeBin is the agent command; tests point it at scripts/fake-claude.
func ClaudeBin() string {
	if s := os.Getenv("AGENTDECK_CLAUDE"); s != "" {
		return s
	}
	return "claude"
}

// ClaudeHome is ~/.claude (or $CLAUDE_CONFIG_DIR).
func ClaudeHome() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(home(), ".claude")
}

// Home returns the user's home directory.
func Home() string { return home() }

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "/"
	}
	return h
}

// Tilde shortens a path under the home directory to ~/…
func Tilde(p string) string {
	h := home()
	if p == h {
		return "~"
	}
	if rel, err := filepath.Rel(h, p); err == nil && rel != "." && !startsWithDotDot(rel) {
		return "~/" + rel
	}
	return p
}

// Expand turns a leading ~ into the home directory and makes the path absolute.
func Expand(p string) string {
	if p == "~" {
		return home()
	}
	if len(p) > 1 && p[:2] == "~/" {
		p = filepath.Join(home(), p[2:])
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func startsWithDotDot(rel string) bool {
	return rel == ".." || (len(rel) > 2 && rel[:3] == "../")
}
