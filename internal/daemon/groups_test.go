package daemon

import (
	"path/filepath"
	"testing"

	"agentdeck/internal/ipc"
	"agentdeck/internal/model"
	"agentdeck/internal/store"
)

func testDaemon(t *testing.T, ids ...string) *Daemon {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &Daemon{st: st, subs: map[chan ipc.Event]struct{}{}}
	for i, id := range ids {
		s := &model.Session{ID: id, Order: i + 1, CreatedAt: int64(i)}
		d.sessions = append(d.sessions, s)
		d.save(s)
	}
	return d
}

func order(d *Daemon) string {
	out := ""
	for _, s := range d.sessions {
		out += s.ID
	}
	return out
}

func TestGroupsAndMove(t *testing.T) {
	d := testDaemon(t, "a", "b", "c", "d")
	if err := d.SetGroup("b", "  Work  stuff "); err != nil {
		t.Fatal(err)
	}
	_ = d.SetGroup("d", "Work stuff")
	if d.find("b").Group != "Work stuff" {
		t.Fatalf("group name is tidied: %q", d.find("b").Group)
	}
	// Joining a group puts the session at the end: a c b d.
	if got := order(d); got != "acbd" {
		t.Fatalf("order %s", got)
	}
	// Move within the group only: d goes before b; a moves past c.
	_ = d.Move("d", -1)
	_ = d.Move("a", 1)
	if got := order(d); got != "cadb" {
		t.Fatalf("after moves %s", got)
	}
	_ = d.Move("c", -1) // already first of the ungrouped: no change
	if got := order(d); got != "cadb" {
		t.Fatalf("no-op move %s", got)
	}
	// Order survives a reload from the store.
	list, _ := d.st.Sessions()
	d.sessions = nil
	for i := range list {
		s := list[i]
		d.sessions = append(d.sessions, &s)
	}
	d.sortSessions()
	if got := order(d); got != "cadb" {
		t.Fatalf("after reload %s", got)
	}

	_ = d.Collapse("Work stuff", true)
	if st := d.snapshot(); !st.IsCollapsed("Work stuff") {
		t.Fatal("collapse not in state")
	}
	_ = d.RenameGroup("Work stuff", "Lab")
	st := d.snapshot()
	if d.find("b").Group != "Lab" || d.find("d").Group != "Lab" || !st.IsCollapsed("Lab") || st.IsCollapsed("Work stuff") {
		t.Fatalf("rename group: %+v", st.Collapsed)
	}
	if g := st.Groups(); len(g) != 1 || g[0] != "Lab" {
		t.Fatalf("groups %v", g)
	}
	_ = d.SetGroup("b", "")
	if d.find("b").Group != "" {
		t.Fatal("empty group removes it")
	}
}

func TestRenameKeepsNameOverPrompts(t *testing.T) {
	d := testDaemon(t, "a")
	d.hooked = map[string]bool{}
	d.deckSent = map[string]sent{}
	_ = d.Rename("a", "news feed")
	d.Hook(ipc.HookParams{Session: "a", Event: "UserPromptSubmit", Payload: []byte(`{"prompt":"fix the parser"}`)})
	s := d.find("a")
	if s.DisplayTitle() != "news feed" || s.Title != "fix the parser" {
		t.Fatalf("name %q title %q", s.Name, s.Title)
	}
	_ = d.Rename("a", "")
	if d.find("a").DisplayTitle() != "fix the parser" {
		t.Fatal("empty name goes back to the last prompt")
	}
}
