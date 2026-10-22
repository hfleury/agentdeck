package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseNumstat(t *testing.T) {
	a, d := parseNumstat("3\t1\ta.go\n-\t-\timg.png\n10\t0\tb.go\n")
	if a != 13 || d != 1 {
		t.Fatalf("got +%d -%d", a, d)
	}
}

func TestWorktreePath(t *testing.T) {
	if got := WorktreePath("/src/api", "feat/login"); got != "/src/api.worktrees/feat-login" {
		t.Fatal(got)
	}
}

func repo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "api")
	_ = os.MkdirAll(dir, 0o755)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644)
	exec.Command("git", "-C", dir, "add", ".").Run()
	exec.Command("git", "-C", dir, "commit", "-qm", "init").Run()
	return dir
}

func TestStatusAndWorktree(t *testing.T) {
	dir := repo(t)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nTWO\nthree\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x\n"), 0o644)
	g := Status(dir)
	if g.Branch != "main" || g.Added != 2 || g.Deleted != 1 || g.Untracked != 1 {
		t.Fatalf("status = %+v", g)
	}
	diff, err := Diff(dir, false)
	if err != nil || !contains(diff, "+TWO") || !contains(diff, "+++ b/new.txt") {
		t.Fatalf("diff missing changes: %v\n%s", err, diff)
	}

	wt, err := AddWorktree(dir, "feat/x")
	if err != nil {
		t.Fatal(err)
	}
	if Status(wt).Branch != "feat/x" || Dirty(wt) {
		t.Fatalf("worktree status %+v", Status(wt))
	}
	if _, err := AddWorktree(dir, "bad..name"); err == nil {
		t.Fatal("invalid branch accepted")
	}
	if err := RemoveWorktree(dir, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("worktree still there")
	}
	if b := Branches(dir); len(b) != 2 {
		t.Fatalf("branch should be kept: %v", b)
	}
	// Existing branch: reuse it instead of failing.
	if _, err := AddWorktree(dir, "feat/x"); err != nil {
		t.Fatal(err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
