package daemon

import (
	"testing"
	"time"

	"agentdeck/internal/claude"
	"agentdeck/internal/model"
	"agentdeck/internal/tmux"
)

func TestNotificationState(t *testing.T) {
	tests := []struct {
		p    claude.HookPayload
		want string
	}{
		{claude.HookPayload{NotificationType: "permission_prompt"}, model.StateInput},
		{claude.HookPayload{NotificationType: "elicitation_dialog"}, model.StateInput},
		{claude.HookPayload{NotificationType: "idle_prompt"}, model.StateDone},
		{claude.HookPayload{Message: "Claude needs your permission to use Bash"}, model.StateInput},
		{claude.HookPayload{Message: "Claude is waiting for your input"}, model.StateDone},
		{claude.HookPayload{NotificationType: "auth_success"}, ""},
	}
	for _, tt := range tests {
		if got := notificationState(tt.p); got != tt.want {
			t.Errorf("%+v: got %q, want %q", tt.p, got, tt.want)
		}
	}
}

func TestDeckEchoMatching(t *testing.T) {
	s := sent{text: "fix it\nplease", at: time.Now()}
	if !s.matches("fix it please") || !s.matches(`<pasted_content id="1">fix it
please</pasted_content>`) {
		t.Fatal("echo not recognized")
	}
	if s.matches("something typed in claude") {
		t.Fatal("a different prompt is not the echo")
	}
	if (sent{text: "x", at: time.Now().Add(-time.Hour)}).matches("x") {
		t.Fatal("old sends don't match")
	}
}

func TestSlots(t *testing.T) {
	p := func(id string, left, top int) tmux.Pane {
		return tmux.Pane{ID: id, Session: deckSession, Left: left, Top: top}
	}
	panes := map[string]tmux.Pane{
		"%8": p("%8", 0, 0), "%3": p("%3", 35, 0), "%4": p("%4", 185, 0),
		"%1": {ID: "%1", Session: agentsSession},
	}
	l, c, r, ok := slots(panes)
	if !ok || l != "%8" || c != "%3" || r != "%4" {
		t.Fatalf("got %s %s %s %v", l, c, r, ok)
	}
	panes["%2"] = p("%2", 0, 35)
	if _, _, _, ok := slots(panes); ok {
		t.Fatal("a fourth pane is not the deck shape")
	}
}
