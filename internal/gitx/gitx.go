// Package gitx runs the few git commands agentdeck needs.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"agentdeck/internal/model"
)

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// Toplevel returns the repository root containing dir, or "" if dir isn't in a repo.
func Toplevel(dir string) string {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return out
}

// Status returns branch and line changes (tracked, against HEAD) plus the
// number of untracked files. A folder outside git gives an empty GitInfo.
func Status(dir string) model.GitInfo {
	var g model.GitInfo
	out, err := git(dir, "status", "--porcelain=v2", "--branch", "--untracked-files=normal")
	if err != nil {
		return g
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			g.Branch = strings.TrimPrefix(line, "# branch.head ")
			if g.Branch == "(detached)" {
				if sha, err := git(dir, "rev-parse", "--short", "HEAD"); err == nil {
					g.Branch = sha
				}
			}
		case strings.HasPrefix(line, "? "):
			g.Untracked++
		}
	}
	g.Added, g.Deleted = parseNumstat(gitOr(dir, "diff", "--numstat", "HEAD"))
	return g
}

func gitOr(dir string, args ...string) string {
	out, _ := git(dir, args...)
	return out
}

func parseNumstat(out string) (add, del int) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		a, err1 := strconv.Atoi(f[0]) // "-" for binary files
		d, err2 := strconv.Atoi(f[1])
		if err1 == nil {
			add += a
		}
		if err2 == nil {
			del += d
		}
	}
	return
}

// WorktreePath is where a branch's worktree goes: <repo>/../<repo>.worktrees/<branch>.
func WorktreePath(repo, branch string) string {
	return filepath.Join(filepath.Dir(repo), filepath.Base(repo)+".worktrees", strings.ReplaceAll(branch, "/", "-"))
}

// AddWorktree creates a worktree for branch (creating the branch from HEAD
// if it doesn't exist) and returns its path.
func AddWorktree(repo, branch string) (string, error) {
	if repo == "" {
		return "", errors.New("not a git repository")
	}
	if err := ValidBranch(repo, branch); err != nil {
		return "", err
	}
	path := WorktreePath(repo, branch)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists", path)
	}
	var err error
	if _, e := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); e == nil {
		_, err = git(repo, "worktree", "add", path, branch)
	} else {
		_, err = git(repo, "worktree", "add", "-b", branch, path)
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

// ValidBranch checks a branch name with git's own rules.
func ValidBranch(repo, branch string) error {
	if branch == "" {
		return errors.New("branch name is empty")
	}
	if _, err := git(repo, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	return nil
}

// Dirty reports whether the worktree has uncommitted or untracked changes.
func Dirty(dir string) bool {
	out, err := git(dir, "status", "--porcelain")
	return err != nil || out != ""
}

// RemoveWorktree removes a clean worktree (the branch is kept).
func RemoveWorktree(repo, path string) error {
	_, err := git(repo, "worktree", "remove", path)
	return err
}

// Branches lists local branch names.
func Branches(dir string) []string {
	out, err := git(dir, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// Files lists tracked and untracked (not ignored) files, relative to dir.
func Files(dir string) []string {
	out, err := git(dir, "ls-files", "--cached", "--others", "--exclude-standard")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// Diff returns the uncommitted changes (tracked against HEAD, plus untracked
// files as new-file diffs), with color if color is true.
func Diff(dir string, color bool) (string, error) {
	c := "--color=never"
	if color {
		c = "--color=always"
	}
	tracked, err := git(dir, "diff", c, "HEAD")
	if err != nil {
		// No commits yet: diff against the empty tree.
		tracked, err = git(dir, "diff", c, "--cached")
		if err != nil {
			return "", err
		}
	}
	var b strings.Builder
	b.WriteString(tracked)
	untracked, _ := git(dir, "ls-files", "--others", "--exclude-standard")
	for _, f := range strings.Split(untracked, "\n") {
		if f == "" {
			continue
		}
		// --no-index exits 1 when files differ, so ignore the error.
		cmd := exec.Command("git", "-C", dir, "diff", c, "--no-index", "--", "/dev/null", f)
		out, _ := cmd.Output()
		if b.Len() > 0 && len(out) > 0 {
			b.WriteString("\n")
		}
		b.Write(bytes.TrimRight(out, "\n"))
	}
	return b.String(), nil
}

// Changed lists uncommitted changes as "status path" lines (git status --short).
func Changed(dir string) []string {
	out, err := git(dir, "status", "--short", "--untracked-files=normal")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}
