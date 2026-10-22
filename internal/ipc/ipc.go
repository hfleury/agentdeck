// Package ipc is the daemon protocol: newline-delimited JSON over a Unix
// socket. A connection carries either one request and its response, or a
// "subscribe" request followed by a stream of events.
package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"agentdeck/internal/paths"
)

// Request is a client call.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response answers a Request. Error is set when the call failed.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Event is pushed to subscribers.
type Event struct {
	Type string          `json:"type"` // "state", "history", "notice"
	Data json.RawMessage `json:"data"`
}

// Call sends one request and decodes the result into out (which may be nil).
func Call(method string, params, out any) error {
	return CallTimeout(method, params, out, 10*time.Second)
}

// CallTimeout is Call with an explicit deadline for the whole exchange.
func CallTimeout(method string, params, out any, d time.Duration) error {
	conn, err := net.DialTimeout("unix", paths.Socket(), d)
	if err != nil {
		return fmt.Errorf("agentdeck daemon not reachable: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(d))
	if err := writeRequest(conn, method, params); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if out != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

// Subscribe opens an event stream. Events arrive on the returned channel
// until the connection drops, then the channel is closed.
func Subscribe() (<-chan Event, func(), error) {
	conn, err := net.DialTimeout("unix", paths.Socket(), 3*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if err := writeRequest(conn, "subscribe", nil); err != nil {
		conn.Close()
		return nil, nil, err
	}
	ch := make(chan Event, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var ev Event
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				ch <- ev
			}
		}
	}()
	return ch, func() { conn.Close() }, nil
}

func writeRequest(conn net.Conn, method string, params any) error {
	req := Request{Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = b
	}
	b, _ := json.Marshal(req)
	_, err := conn.Write(append(b, '\n'))
	return err
}

// Params used by more than one side.

type IDParams struct {
	ID string `json:"id"`
}

type NewParams struct {
	Dir    string `json:"dir"`
	Branch string `json:"branch,omitempty"` // non-empty: start in a new worktree for this branch
	Model  string `json:"model,omitempty"`
	Group  string `json:"group,omitempty"` // the new session joins this group
}

type CloseParams struct {
	ID             string `json:"id"`
	RemoveWorktree bool   `json:"remove_worktree"`
}

type SendParams struct {
	ID   string `json:"id"`   // "" = active session
	Text string `json:"text"` // raw prompt text; the daemon routes it
	// NoHistory keeps the prompt out of history (typed with a leading space).
	NoHistory bool `json:"no_history,omitempty"`
}

// SendResult says what the daemon did with a prompt.
type SendResult struct {
	Kind   string `json:"kind"` // "app", "claude", "text", "empty"
	Notice string `json:"notice,omitempty"`
}

type HookParams struct {
	Session string          `json:"session"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

// HookResult answers a hook; Context is added to the prompt
// (UserPromptSubmit only).
type HookResult struct {
	Context string `json:"context,omitempty"`
}

// AddParams adds a context item; Session "" means the active session.
type AddParams struct {
	Session string `json:"session,omitempty"`
	Kind    string `json:"kind"`
	Ref     string `json:"ref,omitempty"`
	Payload string `json:"payload,omitempty"`
	Title   string `json:"title,omitempty"`
	Pinned  bool   `json:"pinned,omitempty"`
}

// ItemParams names one context item.
type ItemParams struct {
	ID     int64 `json:"id"`
	Pinned bool  `json:"pinned,omitempty"`
}

// ShareParams copies one item (ID) or all items of From to To.
type ShareParams struct {
	ID   int64  `json:"id,omitempty"`
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

type FocusParams struct {
	Pane string `json:"pane"` // "center", "prompt", "sidebar", "context"
}

type HistoryParams struct {
	Session string `json:"session,omitempty"` // "" = all sessions
	Limit   int    `json:"limit,omitempty"`
}
