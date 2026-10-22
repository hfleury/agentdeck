package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hooks.json")
	if err := WriteHookSettings(p, "/opt/agent deck/agentdeck"); err != nil {
		t.Fatal(err)
	}
	var f struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Type, Command string }
		}
	}
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, ev := range HookEvents {
		h := f.Hooks[ev]
		if len(h) != 1 || h[0].Hooks[0].Command != "'/opt/agent deck/agentdeck' hook "+ev {
			t.Fatalf("%s: %+v", ev, h)
		}
	}
	if f.Hooks["PreToolUse"][0].Matcher != "*" {
		t.Fatal("tool hooks need a matcher")
	}
}

func TestStartCommand(t *testing.T) {
	t.Setenv("AGENTDECK_CLAUDE", "claude")
	got := StartCommand(Start{ID: "abc", Settings: "/s.json", Model: "opus"})
	if got != "exec 'claude' --session-id 'abc' --settings '/s.json' --model 'opus'" {
		t.Fatal(got)
	}
	got = StartCommand(Start{ID: "abc", ResumeID: "def"})
	if !strings.Contains(got, "--resume 'def'") || strings.Contains(got, "--session-id") {
		t.Fatal(got)
	}
}

func TestCleanPrompt(t *testing.T) {
	in := "fix this\n<pasted_content id=\"1\">long\nlog</pasted_content>  please"
	if got := CleanPrompt(in); got != "fix this [pasted] please" {
		t.Fatalf("got %q", got)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCatalog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	proj := filepath.Join(t.TempDir(), "proj")
	write(t, filepath.Join(proj, ".git", "HEAD"), "")
	write(t, filepath.Join(home, "commands", "deploy.md"), "---\ndescription: Deploy it\nargument-hint: <env>\n---\nbody")
	write(t, filepath.Join(home, "skills", "pdf", "SKILL.md"), "---\nname: pdf\ndescription: >\n  Work with\n  PDF files\n---\n")
	write(t, filepath.Join(proj, ".claude", "commands", "lint.md"), "no front matter")
	plug := filepath.Join(home, "plugins", "cache", "m", "tools", "1.0")
	write(t, filepath.Join(plug, "commands", "plan.md"), "---\ndescription: \"Plan a task\"\n---\n")
	write(t, filepath.Join(plug, "skills", "review-x", "SKILL.md"), "---\nname: review-x\n---\n")
	other := filepath.Join(home, "plugins", "cache", "m", "other", "1.0")
	write(t, filepath.Join(other, "commands", "secret.md"), "")
	inst := map[string]any{"version": 2, "plugins": map[string]any{
		"tools@m": []map[string]string{{"scope": "user", "installPath": plug}},
		"other@m": []map[string]string{{"scope": "local", "projectPath": "/elsewhere", "installPath": other}},
	}}
	b, _ := json.Marshal(inst)
	write(t, filepath.Join(home, "plugins", "installed_plugins.json"), string(b))

	got := map[string]Command{}
	for _, c := range Catalog(filepath.Join(proj, "sub")) {
		got[c.Name] = c
	}
	want := map[string]string{
		"model":          "claude",
		"deploy":         "custom",
		"pdf":            "skill",
		"lint":           "custom",
		"tools:plan":     "plugin",
		"tools:review-x": "plugin",
	}
	for name, src := range want {
		if got[name].Source != src {
			t.Errorf("%s: got %+v, want source %s", name, got[name], src)
		}
	}
	if got["deploy"].Desc != "Deploy it" || got["deploy"].ArgHint != "<env>" {
		t.Errorf("deploy: %+v", got["deploy"])
	}
	if got["pdf"].Desc != "Work with PDF files" {
		t.Errorf("folded description: %q", got["pdf"].Desc)
	}
	if got["tools:plan"].Desc != "Plan a task" {
		t.Errorf("quoted description: %q", got["tools:plan"].Desc)
	}
	if _, ok := got["other:secret"]; ok {
		t.Error("plugin installed for another project must not show")
	}
}

func TestIsSystemPrompt(t *testing.T) {
	for _, s := range []string{"<task-notification> <task-id>x</task-id>", "  <command-name>/x</command-name>", "<local-command-stdout>ok"} {
		if !IsSystemPrompt(s) {
			t.Errorf("%q should be a system prompt", s)
		}
	}
	for _, s := range []string{"fix <b> tags", "/model opus", "<pasted_content id=1>log</pasted_content>", "< not a tag", "<3 thanks"} {
		if IsSystemPrompt(s) {
			t.Errorf("%q is the user's", s)
		}
	}
}
