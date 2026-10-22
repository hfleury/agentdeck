// Package tmux is a thin wrapper over the tmux CLI, always talking to
// agentdeck's own server (tmux -L agentdeck), never the user's.
package tmux

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Client runs tmux commands against one server.
type Client struct {
	Server string // -L name
	Config string // -f file, used when the server is started
	Env    []string
}

// Run executes tmux with args and returns trimmed stdout.
func (c *Client) Run(args ...string) (string, error) {
	return c.RunInput("", args...)
}

// RunInput is Run with stdin.
func (c *Client) RunInput(stdin string, args ...string) (string, error) {
	full := []string{"-L", c.Server}
	if c.Config != "" {
		full = append(full, "-f", c.Config)
	}
	full = append(full, args...)
	cmd := exec.Command("tmux", full...)
	if c.Env != nil {
		cmd.Env = c.Env
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("tmux %s: %s", args[0], msg)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// HasSession reports whether a session exists.
func (c *Client) HasSession(name string) bool {
	_, err := c.Run("has-session", "-t", "="+name)
	return err == nil
}

// Pane describes one pane from list-panes.
type Pane struct {
	ID         string
	Session    string
	Dead       bool
	DeadStatus int
	Width      int
	Height     int
	Window     string // window id (@3)
	Left, Top  int
}

// Panes lists every pane on the server.
func (c *Client) Panes() (map[string]Pane, error) {
	out, err := c.Run("list-panes", "-a", "-F", "#{pane_id}\t#{session_name}\t#{pane_dead}\t#{pane_dead_status}\t#{pane_width}\t#{pane_height}\t#{window_id}\t#{pane_left}\t#{pane_top}")
	if err != nil {
		return nil, err
	}
	panes := map[string]Pane{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 9 {
			continue
		}
		st, _ := strconv.Atoi(f[3])
		w, _ := strconv.Atoi(f[4])
		h, _ := strconv.Atoi(f[5])
		l, _ := strconv.Atoi(f[7])
		t, _ := strconv.Atoi(f[8])
		panes[f[0]] = Pane{ID: f[0], Session: f[1], Dead: f[2] == "1", DeadStatus: st, Width: w, Height: h,
			Window: f[6], Left: l, Top: t}
	}
	return panes, nil
}

// NewWindow starts cmd in a new detached window of session (created if
// missing) and returns the new pane id.
func (c *Client) NewWindow(session, name, dir string, env map[string]string, cmd string) (string, error) {
	var args []string
	if c.HasSession(session) {
		args = []string{"new-window", "-d", "-t", session + ":", "-n", name}
	} else {
		args = []string{"new-session", "-d", "-s", session, "-n", name, "-x", "200", "-y", "50"}
	}
	args = append(args, "-c", dir, "-P", "-F", "#{pane_id}")
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, cmd)
	return c.Run(args...)
}

// SendText types text literally into a pane (no key names interpreted).
func (c *Client) SendText(pane, text string) error {
	_, err := c.Run("send-keys", "-t", pane, "-l", "--", text)
	return err
}

// SendKeys sends named keys (Enter, Escape, S-Enter, …).
func (c *Client) SendKeys(pane string, keys ...string) error {
	_, err := c.Run(append([]string{"send-keys", "-t", pane}, keys...)...)
	return err
}

// Paste delivers text as one bracketed paste, so multi-line text stays one
// message in the receiving program.
func (c *Client) Paste(pane, text string) error {
	if _, err := c.RunInput(text, "load-buffer", "-b", "agentdeck-send", "-"); err != nil {
		return err
	}
	_, err := c.Run("paste-buffer", "-p", "-d", "-b", "agentdeck-send", "-t", pane)
	return err
}

// Quote quotes s for a tmux/sh command line.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CleanEnv returns the current environment without the variables that make
// a nested claude skip saving transcripts, and without TMUX (so agentdeck's
// server is independent of any tmux the user is in).
func CleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "TMUX" || k == "TMUX_PANE" || k == "CLAUDECODE" || strings.HasPrefix(k, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
