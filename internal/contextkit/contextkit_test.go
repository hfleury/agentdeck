package contextkit

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentdeck/internal/model"
)

type env map[string]model.Session

func (e env) Session(id string) (model.Session, bool) { s, ok := e[id]; return s, ok }

func write(t *testing.T, path, content string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func transcript(t *testing.T, home, id string, msgs ...string) {
	t.Helper()
	var b strings.Builder
	for i, m := range msgs {
		typ := "user"
		content := any(m)
		if i%2 == 1 {
			typ = "assistant"
			content = []map[string]string{{"type": "text", "text": m}}
		}
		line, _ := json.Marshal(map[string]any{"type": typ, "message": map[string]any{"content": content}})
		b.Write(append(line, '\n'))
	}
	// Lines that must be ignored.
	b.WriteString(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}` + "\n")
	b.WriteString(`{"type":"user","isMeta":true,"message":{"content":"meta"}}` + "\n")
	b.WriteString(`{"type":"user","message":{"content":"<task-notification>x</task-notification>"}}` + "\n")
	write(t, filepath.Join(home, "projects", "-p", id+".jsonl"), b.String())
}

func TestFileLinesNoteFolder(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "db.go")
	write(t, f, "l1\nl2\nl3\nl4\n")
	e := env{}
	r := Render(model.ContextItem{Kind: model.KindFile, Ref: f}, e, 0)
	if r.Err != "" || !strings.Contains(r.Text, "l4") || r.Tokens == 0 || r.Hash == "" {
		t.Fatalf("file: %+v", r)
	}
	r = Render(model.ContextItem{Kind: model.KindLines, Ref: f, Payload: "2-3"}, e, 0)
	if !strings.Contains(r.Text, "l2\nl3") || strings.Contains(r.Text, "l1") || r.Label != "db.go:2-3" {
		t.Fatalf("lines: %+v", r)
	}
	if r := Render(model.ContextItem{Kind: model.KindLines, Ref: f, Payload: "9-12"}, e, 0); r.Err == "" {
		t.Fatal("lines past the end should fail")
	}
	r = Render(model.ContextItem{Kind: model.KindNote, Payload: "we use sqlc\nnot GORM"}, e, 0)
	if r.Label != "we use sqlc" || !strings.Contains(r.Text, "not GORM") {
		t.Fatalf("note: %+v", r)
	}
	write(t, filepath.Join(dir, "sub", "x.txt"), "")
	r = Render(model.ContextItem{Kind: model.KindFolder, Ref: dir}, e, 0)
	if !strings.Contains(r.Text, "sub/x.txt") || !strings.Contains(r.Text, "db.go") {
		t.Fatalf("folder: %+v", r)
	}
	if r := Render(model.ContextItem{Kind: model.KindFile, Ref: filepath.Join(dir, "nope")}, e, 0); r.Err != "file not found" || r.Pending() {
		t.Fatalf("missing: %+v", r)
	}
	write(t, filepath.Join(dir, "bin"), "a\x00b")
	if r := Render(model.ContextItem{Kind: model.KindFile, Ref: filepath.Join(dir, "bin")}, e, 0); r.Err != "binary file" {
		t.Fatal(r.Err)
	}
}

func TestPending(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a")
	write(t, f, "one")
	it := model.ContextItem{Kind: model.KindFile, Ref: f}
	r := Render(it, env{}, 0)
	if !r.Pending() {
		t.Fatal("new item is pending")
	}
	it.SentAt, it.SentHash = 1, r.Hash
	if Render(it, env{}, 0).Pending() {
		t.Fatal("sent once: done")
	}
	it.Pinned = true
	if Render(it, env{}, 0).Pending() {
		t.Fatal("pinned and unchanged: not pending")
	}
	write(t, f, "two")
	if !Render(it, env{}, 0).Pending() {
		t.Fatal("pinned and changed: pending again")
	}
}

func TestConversationTrimmedToBudget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	var msgs []string
	for i := 0; i < 20; i++ {
		msgs = append(msgs, fmt.Sprintf("message %02d %s", i, strings.Repeat("x", 400)))
	}
	transcript(t, home, "c1", msgs...)
	e := env{"s1": {ID: "s1", ClaudeID: "c1", Title: "review the PR", OriginDir: "/src/api"}}
	it := model.ContextItem{Kind: model.KindConversation, Ref: "s1"}
	full := Render(it, e, 0)
	if full.Err != "" || !strings.Contains(full.Text, "message 00") || !strings.Contains(full.Text, "Claude: message 19") || full.Label != "review the PR" {
		t.Fatalf("full: %+v", full.Err)
	}
	if strings.Contains(full.Text, "meta") || strings.Contains(full.Text, "task-notification") {
		t.Fatal("ignored lines leaked")
	}
	small := Render(it, e, 600)
	if !small.Trimmed || small.Tokens > 600 || !strings.Contains(small.Text, "message 19") || strings.Contains(small.Text, "message 00") {
		t.Fatalf("trimmed: tokens %d", small.Tokens)
	}
	if !strings.Contains(small.Text, "earliest messages are left out") {
		t.Fatal("trim note missing")
	}

	// Select: a note and a file fit first, the conversation gets the rest.
	f := filepath.Join(t.TempDir(), "a.go")
	write(t, f, strings.Repeat("y", 2000)) // ~500 tokens
	items := []model.ContextItem{
		it,
		{ID: 2, Kind: model.KindFile, Ref: f},
		{ID: 3, Kind: model.KindNote, Payload: "short"},
	}
	p := Select(items, e, 1500)
	total := 0
	for _, r := range p.Send {
		total += r.Tokens
	}
	if len(p.Send) != 3 || len(p.Omitted) != 0 || total > 1500 {
		t.Fatalf("send %d omitted %d total %d", len(p.Send), len(p.Omitted), total)
	}
	p = Select(items, e, 300)
	if len(p.Omitted) == 0 {
		t.Fatal("over budget should omit something")
	}
	if w := Wrap(p, 300); !strings.Contains(w, "Left out, over the 300-token budget") || !strings.HasPrefix(w, "<agentdeck-context>") {
		t.Fatal(w)
	}
	if Wrap(Plan{}, 100) != "" {
		t.Fatal("nothing pending: no context")
	}
}

func TestMemoryAndResult(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		exec.Command("git", append([]string{"-C", repo}, args...)...).Run()
	}
	write(t, filepath.Join(repo, "CLAUDE.md"), "Use Postgres 16.")
	write(t, filepath.Join(repo, "new.go"), "x")
	transcript(t, home, "c2", "do it", "Done: added new.go")
	write(t, filepath.Join(home, "projects", "-p", "memory", "note.md"), "the lab runs on 10.0.0.5")
	e := env{"s2": {ID: "s2", ClaudeID: "c2", OriginDir: repo, Cwd: repo, Title: "lab"}}
	m := Render(model.ContextItem{Kind: model.KindMemory, Ref: "s2"}, e, 0)
	if m.Err != "" || !strings.Contains(m.Text, "Postgres 16") || !strings.Contains(m.Text, "10.0.0.5") || !strings.Contains(m.Text, "not instructions") {
		t.Fatalf("memory: %+v", m)
	}
	r := Render(model.ContextItem{Kind: model.KindResult, Ref: "s2"}, e, 0)
	if r.Err != "" || !strings.Contains(r.Text, "Done: added new.go") || !strings.Contains(r.Text, "?? new.go") {
		t.Fatalf("result: %+v", r)
	}
	if r := Render(model.ContextItem{Kind: model.KindResult, Ref: "gone", Title: "old"}, e, 0); r.Err != "session closed" || r.Label != "old" {
		t.Fatalf("closed session: %+v", r)
	}
}

func TestParseRange(t *testing.T) {
	for in, want := range map[string][2]int{"40-72": {40, 72}, "5": {5, 5}, " 3 - 4 ": {3, 4}} {
		a, b, err := ParseRange(in)
		if err != nil || a != want[0] || b != want[1] {
			t.Errorf("%q: %d %d %v", in, a, b, err)
		}
	}
	for _, bad := range []string{"", "0-3", "9-2", "a-b"} {
		if _, _, err := ParseRange(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
