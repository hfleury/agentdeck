// agentdeck runs many Claude Code sessions from one terminal screen.
//
//	agentdeck                 start (or reattach to) the deck
//	agentdeck daemon          state owner; started inside tmux by the launcher
//	agentdeck ui <pane>       sidebar | context | empty
//	agentdeck palette         command window (Alt-K; runs in a tmux popup)
//	agentdeck hook <event>    called by Claude Code hooks
//	agentdeck review <dir>    diff viewer (runs in a tmux popup)
//	agentdeck pick [dir]      new-session window (runs in a tmux popup)
//	agentdeck send <text>     send to the active session (deck commands run here)
//	agentdeck add <path>[:a-b] [--pin] [--session id]   attach to a session's context
//	agentdeck note <text>     attach a note to the active session
//	agentdeck context [id]    print a session's context items as JSON
//	agentdeck show <item>     an item's text (popup); agentdeck tools <session> [call]: tool calls
//	agentdeck new [dir]       start a session
//	agentdeck state           print the daemon's state as JSON
//	agentdeck kill-server     stop everything, including all sessions
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"agentdeck/internal/contextkit"
	"agentdeck/internal/daemon"
	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
	"agentdeck/internal/paths"
	"agentdeck/internal/tmux"
	uicontext "agentdeck/internal/ui/context"
	"agentdeck/internal/ui/empty"
	"agentdeck/internal/ui/palette"
	"agentdeck/internal/ui/picker"
	"agentdeck/internal/ui/review"
	"agentdeck/internal/ui/sidebar"
	"agentdeck/internal/ui/tools"
	"agentdeck/internal/ui/viewer"
)

//go:embed tmux.conf
var tmuxConf string

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "":
		err = launch()
	case "daemon":
		err = daemon.Run(self())
	case "ui":
		err = runUI(args)
	case "hook":
		hook(args)
	case "palette":
		_, err = tea.NewProgram(palette.New()).Run()
	case "pick":
		// pick [--group G] [--add SESSION] [dir]
		dir, group, addTo := paths.Home(), "", ""
		for i := 0; i < len(args); i++ {
			switch {
			case args[i] == "--group" && i+1 < len(args):
				group = args[i+1]
				i++
			case args[i] == "--add" && i+1 < len(args):
				addTo = args[i+1]
				i++
			default:
				dir = args[i]
			}
		}
		if addTo != "" {
			_, err = tea.NewProgram(picker.NewAdd(addTo, dir)).Run()
			break
		}
		p := picker.New(dir)
		p.Group = group
		_, err = tea.NewProgram(p).Run()
	case "show":
		id, _ := strconv.ParseInt(first(args), 10, 64)
		_, err = tea.NewProgram(viewer.New(id)).Run()
	case "tools":
		var id int64
		if len(args) > 1 {
			id, _ = strconv.ParseInt(args[1], 10, 64)
		}
		_, err = tea.NewProgram(tools.New(first(args), id)).Run()
	case "add":
		err = addCmd(args)
	case "note":
		err = ipc.Call("context_add", ipc.AddParams{Kind: model.KindNote, Payload: strings.Join(args, " ")}, nil)
	case "relayout":
		err = ipc.CallTimeout("relayout", nil, nil, 3*time.Second)
	case "review":
		dir, _ := os.Getwd()
		if len(args) > 0 {
			dir = args[0]
		}
		session := ""
		if len(args) > 1 {
			session = args[1]
		}
		_, err = tea.NewProgram(review.New(dir, session)).Run()
	case "send":
		var res ipc.SendResult
		err = ipc.Call("send", ipc.SendParams{Text: strings.Join(args, " ")}, &res)
		if err == nil && res.Notice != "" {
			fmt.Println(res.Notice)
		}
	case "new":
		p := ipc.NewParams{}
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--branch", "-b":
				if i+1 < len(args) {
					p.Branch = args[i+1]
					i++
				}
			case "--model", "-m":
				if i+1 < len(args) {
					p.Model = args[i+1]
					i++
				}
			default:
				p.Dir = args[i]
			}
		}
		if p.Dir == "" {
			p.Dir, _ = os.Getwd()
		}
		var s model.Session
		if err = ipc.Call("new", p, &s); err == nil {
			fmt.Println(s.ID)
		}
	case "state":
		var st model.State
		if err = ipc.Call("state", nil, &st); err == nil {
			b, _ := json.MarshalIndent(st, "", "  ")
			fmt.Println(string(b))
		}
	case "context":
		var v model.ContextView
		if err = ipc.Call("context", ipc.IDParams{ID: first(args)}, &v); err == nil {
			b, _ := json.MarshalIndent(v, "", "  ")
			fmt.Println(string(b))
		}
	case "kill-server":
		_, err = tmuxClient().Run("kill-server")
	case "-h", "--help", "help":
		fmt.Println("usage: agentdeck [daemon | ui <pane> | hook <event> | review <dir> | send <text> | new [dir] [-b branch] [-m model] | state | kill-server]")
	default:
		err = fmt.Errorf("unknown command %q (try agentdeck help)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentdeck:", err)
		os.Exit(1)
	}
}

func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// addCmd attaches a file, a folder or lines (path:40-72) to the active
// session, or to --session <id>; --pin keeps it.
func addCmd(args []string) error {
	p := ipc.AddParams{Kind: model.KindFile}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pin", "-p":
			p.Pinned = true
		case "--session", "-s":
			if i+1 < len(args) {
				p.Session = args[i+1]
				i++
			}
		default:
			p.Ref = args[i]
		}
	}
	if p.Ref == "" {
		return fmt.Errorf("usage: agentdeck add <path>[:from-to] [--pin] [--session id]")
	}
	if i := strings.LastIndex(p.Ref, ":"); i > 0 {
		if _, _, err := contextkit.ParseRange(p.Ref[i+1:]); err == nil {
			p.Kind, p.Payload, p.Ref = model.KindLines, p.Ref[i+1:], p.Ref[:i]
		}
	}
	if !filepath.IsAbs(p.Ref) && !strings.HasPrefix(p.Ref, "~") {
		wd, _ := os.Getwd()
		p.Ref = filepath.Join(wd, p.Ref)
	}
	var it model.ContextItem
	if err := ipc.Call("context_add", p, &it); err != nil {
		return err
	}
	fmt.Printf("added %s %s\n", it.Kind, paths.Tilde(it.Ref))
	return nil
}

func self() string {
	p, err := os.Executable()
	if err != nil {
		return "agentdeck"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func tmuxClient() *tmux.Client {
	conf := filepath.Join(paths.StateDir(), "tmux.conf")
	return &tmux.Client{Server: paths.TmuxServer(), Config: conf, Env: tmux.CleanEnv()}
}

// launch starts the tmux server and daemon if needed, builds the layout,
// and attaches this terminal to the deck.
func launch() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("tmux is not installed (sudo pacman -S tmux)")
	}
	if err := os.MkdirAll(paths.StateDir(), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(paths.StateDir(), "tmux.conf"), []byte(tmuxConf), 0o600); err != nil {
		return err
	}
	tm := tmuxClient()
	// The config is read when the server starts; reload it into a server
	// that is already running, so changes apply without stopping sessions.
	if tm.HasSession("sys") || tm.HasSession("deck") {
		if _, err := tm.Run("source-file", tm.Config); err != nil {
			fmt.Fprintln(os.Stderr, "agentdeck: tmux.conf:", err)
		}
	}
	if err := ensureDaemon(tm); err != nil {
		return err
	}
	w, h, _ := term.GetSize(os.Stdout.Fd())
	if err := ipc.Call("layout", struct{ W, H int }{w, h}, nil); err != nil {
		return err
	}
	if os.Getenv("AGENTDECK_NO_ATTACH") != "" {
		return nil
	}
	bin, _ := exec.LookPath("tmux")
	argv := []string{"tmux", "-L", tm.Server, "attach-session", "-t", "deck"}
	return syscall.Exec(bin, argv, tm.Env)
}

func ensureDaemon(tm *tmux.Client) error {
	if ipc.CallTimeout("ping", nil, nil, time.Second) == nil {
		return nil
	}
	cmd := tmux.Quote(self()) + " daemon"
	var err error
	if tm.HasSession("sys") {
		_, err = tm.Run("respawn-window", "-k", "-t", "sys:0", cmd)
	} else {
		_, err = tm.Run("new-session", "-d", "-s", "sys", "-n", "daemon", cmd)
	}
	if err != nil {
		return err
	}
	for i := 0; i < 50; i++ {
		if ipc.CallTimeout("ping", nil, nil, 200*time.Millisecond) == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the daemon did not start; see %s", filepath.Join(paths.StateDir(), "daemon.log"))
}

func runUI(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: agentdeck ui sidebar|context|empty")
	}
	var m tea.Model
	switch args[0] {
	case "sidebar":
		m = sidebar.New()
	case "context":
		m = uicontext.New()
	case "empty":
		m = empty.New()
	default:
		return fmt.Errorf("unknown pane %q", args[0])
	}
	// A pane that crashes would leave a hole in the layout; restart it.
	for {
		_, err := tea.NewProgram(m).Run()
		if err == nil {
			return nil
		}
		fmt.Fprintln(os.Stderr, "agentdeck ui:", err)
		time.Sleep(time.Second)
	}
}

// hook forwards a Claude Code hook event to the daemon. It never fails.
// It prints only for UserPromptSubmit, when there is context to attach:
// whatever a hook prints is given to Claude.
func hook(args []string) {
	session := os.Getenv("AGENTDECK_SESSION")
	if session == "" || len(args) == 0 {
		return
	}
	payload, _ := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if !json.Valid(payload) {
		payload = []byte("{}")
	}
	wait := time.Second
	if args[0] == "UserPromptSubmit" {
		wait = 4 * time.Second // it returns the context to attach
	}
	var res ipc.HookResult
	if ipc.CallTimeout("hook", ipc.HookParams{Session: session, Event: args[0], Payload: payload}, &res, wait) != nil {
		return
	}
	if res.Context != "" {
		out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": res.Context,
		}})
		os.Stdout.Write(append(out, '\n'))
	}
}
