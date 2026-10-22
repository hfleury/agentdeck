package store

import (
	"path/filepath"
	"testing"

	"agentdeck/internal/model"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSessionsRoundTrip(t *testing.T) {
	s := open(t)
	a := model.Session{ID: "a", Title: "fix bug", State: model.StateDone, CreatedAt: 1, Git: model.GitInfo{Branch: "main", Added: 3}}
	b := model.Session{ID: "b", CreatedAt: 2}
	for _, x := range []model.Session{b, a} {
		if err := s.SaveSession(x); err != nil {
			t.Fatal(err)
		}
	}
	a.Title = "renamed"
	_ = s.SaveSession(a)
	got, err := s.Sessions()
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got[0].ID != "a" || got[0].Title != "renamed" || got[0].Git.Added != 3 {
		t.Fatalf("order or update wrong: %+v", got[0])
	}
	_ = s.DeleteSession("a")
	if got, _ := s.Sessions(); len(got) != 1 {
		t.Fatalf("delete failed: %v", got)
	}
}

func TestHistoryDedupesConsecutivePerSession(t *testing.T) {
	s := open(t)
	add := func(sess, text string) bool {
		_, ok, err := s.AddHistory(sess, text, "deck")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !add("a", "one") || add("a", "one") {
		t.Fatal("consecutive duplicate should be skipped")
	}
	if !add("b", "one") {
		t.Fatal("same text in another session is a new entry")
	}
	if !add("a", "two") || !add("a", "one") {
		t.Fatal("non-consecutive repeat should be stored")
	}
	all, _ := s.History("", 0)
	if len(all) != 4 {
		t.Fatalf("all = %v", all)
	}
	own, _ := s.History("a", 0)
	if len(own) != 3 || own[0].Text != "one" || own[2].Text != "one" || own[1].Text != "two" {
		t.Fatalf("own = %v", own)
	}
	last, _ := s.History("a", 1)
	if len(last) != 1 || last[0].Text != "one" {
		t.Fatalf("limit keeps the newest: %v", last)
	}
}

func TestKV(t *testing.T) {
	s := open(t)
	_ = s.Set("active", "x")
	_ = s.Set("active", "y")
	if s.Get("active") != "y" || s.Get("missing") != "" {
		t.Fatal("kv broken")
	}
}

func TestToolCalls(t *testing.T) {
	s := open(t)
	a, _ := s.StartTool("x", "u1", "Bash", "go test", `{"command":"go test"}`)
	_, _ = s.StartTool("x", "", "Read", "a.go", "")
	if _, err := s.FinishTool("x", "u1", "Bash", "go test", "", "FAIL", model.ToolFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishTool("x", "", "Read", "a.go", "", "content", model.ToolOK); err != nil {
		t.Fatal(err)
	}
	// Finished without a start: still recorded.
	if _, err := s.FinishTool("x", "u9", "Edit", "b.go", "", "", model.ToolOK); err != nil {
		t.Fatal(err)
	}
	calls, _ := s.ToolCalls("x", 10)
	if len(calls) != 3 || calls[0].Status != model.ToolFailed || calls[1].Status != model.ToolOK || calls[2].Tool != "Edit" {
		t.Fatalf("calls = %+v", calls)
	}
	full, _ := s.ToolCall(a.ID)
	if full.Output != "FAIL" || full.Input == "" {
		t.Fatalf("full = %+v", full)
	}
	_, _ = s.StartTool("x", "u2", "Bash", "sleep", "")
	_ = s.StopRunningTools("x")
	calls, _ = s.ToolCalls("x", 1)
	if calls[0].Status != model.ToolFailed {
		t.Fatalf("interrupted call should be failed: %+v", calls[0])
	}
}

func TestContextItems(t *testing.T) {
	s := open(t)
	it, err := s.AddItem(model.ContextItem{SessionID: "x", Kind: model.KindNote, Payload: "use sqlc"})
	if err != nil || it.ID == 0 {
		t.Fatal(err)
	}
	_, _ = s.AddItem(model.ContextItem{SessionID: "x", Kind: model.KindFile, Ref: "/a.go", Pinned: true})
	_, _ = s.AddItem(model.ContextItem{SessionID: "y", Kind: model.KindNote, Payload: "other"})
	_ = s.MarkSent(it.ID, "h1")
	_ = s.SetPinned(it.ID, true)
	items, _ := s.Items("x")
	if len(items) != 2 || items[0].SentHash != "h1" || items[0].SentAt == 0 || !items[0].Pinned || !items[1].Pinned {
		t.Fatalf("items = %+v", items)
	}
	_ = s.MarkSent(it.ID, "")
	if got, _ := s.Item(it.ID); got.SentAt != 0 {
		t.Fatal("clearing the hash should clear sent_at")
	}
	_ = s.DeleteItem(it.ID)
	_ = s.DeleteItems("y")
	if a, _ := s.Items("x"); len(a) != 1 {
		t.Fatal(a)
	}
	if b, _ := s.Items("y"); len(b) != 0 {
		t.Fatal(b)
	}
}
