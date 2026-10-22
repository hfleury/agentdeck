// Package live turns the daemon's event stream into Bubble Tea messages,
// reconnecting when the daemon restarts.
package live

import (
	"encoding/json"
	"time"

	tea "charm.land/bubbletea/v2"

	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
)

// Messages delivered to the panes.
type (
	StateMsg     model.State
	HistoryMsg   model.HistoryEntry
	NoticeMsg    string
	ConnectedMsg struct{ Conn *Conn }
	// ContextMsg and ToolsMsg say a session's context items or tool calls
	// changed; panes fetch what they need.
	ContextMsg      string
	ToolsMsg        string
	DisconnectedMsg struct{}
)

// Conn is an open subscription.
type Conn struct {
	ch    <-chan ipc.Event
	close func()
}

// Connect subscribes to the daemon.
func Connect() tea.Cmd {
	return func() tea.Msg {
		ch, closeFn, err := ipc.Subscribe()
		if err != nil {
			return DisconnectedMsg{}
		}
		return ConnectedMsg{&Conn{ch: ch, close: closeFn}}
	}
}

// Reconnect waits a second, then connects again.
func Reconnect() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return Connect()() })
}

// Next waits for the next event. Call it again after each message.
func (c *Conn) Next() tea.Cmd {
	return func() tea.Msg {
		for ev := range c.ch {
			switch ev.Type {
			case "state":
				var s model.State
				if json.Unmarshal(ev.Data, &s) == nil {
					return StateMsg(s)
				}
			case "history":
				var h model.HistoryEntry
				if json.Unmarshal(ev.Data, &h) == nil {
					return HistoryMsg(h)
				}
			case "context", "tools":
				var id string
				if json.Unmarshal(ev.Data, &id) == nil {
					if ev.Type == "context" {
						return ContextMsg(id)
					}
					return ToolsMsg(id)
				}
			case "notice":
				var n string
				if json.Unmarshal(ev.Data, &n) == nil {
					return NoticeMsg(n)
				}
			}
		}
		return DisconnectedMsg{}
	}
}

// ResultMsg carries the outcome of an RPC started with Call.
type ResultMsg struct {
	Op     string
	Notice string
	Err    error
}

// Call runs an RPC in the background and reports it as a ResultMsg.
func Call(op, method string, params any) tea.Cmd {
	return func() tea.Msg {
		var res ipc.SendResult
		err := ipc.Call(method, params, &res)
		return ResultMsg{Op: op, Notice: res.Notice, Err: err}
	}
}
