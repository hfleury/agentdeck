package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentdeck/internal/paths"
)

// Command is one slash command Claude understands.
type Command struct {
	Name    string // without the slash
	Desc    string
	ArgHint string
	Source  string // "claude", "custom", "skill", "plugin"
}

// Builtins are Claude Code's own slash commands. The list is maintained by
// hand; a command missing here still works, it just isn't suggested.
var Builtins = []Command{
	{Name: "add-dir", Desc: "add a working directory", ArgHint: "<path>"},
	{Name: "agents", Desc: "manage subagents"},
	{Name: "bashes", Desc: "list background shells"},
	{Name: "bug", Desc: "report a bug"},
	{Name: "clear", Desc: "clear the conversation"},
	{Name: "compact", Desc: "summarize the conversation to free context", ArgHint: "[instructions]"},
	{Name: "config", Desc: "open settings"},
	{Name: "context", Desc: "show context usage"},
	{Name: "cost", Desc: "show token usage and cost"},
	{Name: "doctor", Desc: "check the installation"},
	{Name: "exit", Desc: "exit Claude"},
	{Name: "export", Desc: "export the conversation"},
	{Name: "fast", Desc: "toggle fast mode"},
	{Name: "feedback", Desc: "send feedback"},
	{Name: "help", Desc: "show help"},
	{Name: "hooks", Desc: "manage hooks"},
	{Name: "ide", Desc: "connect to an IDE"},
	{Name: "init", Desc: "create CLAUDE.md for this project"},
	{Name: "login", Desc: "sign in"},
	{Name: "logout", Desc: "sign out"},
	{Name: "mcp", Desc: "manage MCP servers"},
	{Name: "memory", Desc: "edit memory files"},
	{Name: "model", Desc: "choose the model", ArgHint: "[model]"},
	{Name: "output-style", Desc: "choose the output style"},
	{Name: "permissions", Desc: "manage tool permissions"},
	{Name: "plugin", Desc: "manage plugins"},
	{Name: "pr-comments", Desc: "show pull request comments"},
	{Name: "release-notes", Desc: "show release notes"},
	{Name: "resume", Desc: "resume a conversation"},
	{Name: "review", Desc: "review a pull request"},
	{Name: "rewind", Desc: "rewind the conversation or code"},
	{Name: "security-review", Desc: "security review of pending changes"},
	{Name: "status", Desc: "show status"},
	{Name: "statusline", Desc: "set up the status line"},
	{Name: "terminal-setup", Desc: "set up terminal keys"},
	{Name: "todos", Desc: "show the todo list"},
	{Name: "upgrade", Desc: "upgrade your plan"},
	{Name: "usage", Desc: "show plan usage"},
	{Name: "vim", Desc: "toggle vim mode"},
}

// Models are the arguments suggested for /model.
var Models = []Command{
	{Name: "default", Desc: "the recommended model"},
	{Name: "opus", Desc: "most capable"},
	{Name: "sonnet", Desc: "everyday work"},
	{Name: "haiku", Desc: "fast and cheap"},
	{Name: "fable", Desc: "Fable"},
}

// IsBuiltin reports whether name is one of Claude's built-in commands.
func IsBuiltin(name string) bool {
	for _, c := range Builtins {
		if c.Name == name {
			return true
		}
	}
	return false
}

// Catalog lists every slash command available to a session started in cwd:
// built-ins, custom commands, skills, and those of installed plugins.
func Catalog(cwd string) []Command {
	home := paths.ClaudeHome()
	var out []Command
	for _, c := range Builtins {
		c.Source = "claude"
		out = append(out, c)
	}
	project := projectDirs(cwd)
	for _, d := range append([]string{home}, project...) {
		out = append(out, scanCommands(filepath.Join(d, "commands"), "", "custom")...)
		out = append(out, scanSkills(filepath.Join(d, "skills"), "", "skill")...)
	}
	for _, p := range plugins(home, cwd) {
		out = append(out, scanCommands(filepath.Join(p.path, "commands"), p.name+":", "plugin")...)
		out = append(out, scanSkills(filepath.Join(p.path, "skills"), p.name+":", "plugin")...)
	}
	seen := map[string]bool{}
	uniq := out[:0]
	for _, c := range out {
		if !seen[c.Name] {
			seen[c.Name] = true
			uniq = append(uniq, c)
		}
	}
	return uniq
}

// projectDirs returns the .claude folders from cwd up to the repo root (or /).
func projectDirs(cwd string) []string {
	var out []string
	for d := cwd; d != "" && d != "/" && d != paths.Home(); d = filepath.Dir(d) {
		if fi, err := os.Stat(filepath.Join(d, ".claude")); err == nil && fi.IsDir() {
			out = append(out, filepath.Join(d, ".claude"))
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			break
		}
	}
	return out
}

func scanCommands(dir, prefix, source string) []Command {
	var out []Command
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		fm := frontmatter(p)
		out = append(out, Command{
			Name:    prefix + strings.TrimSuffix(d.Name(), ".md"),
			Desc:    fm["description"],
			ArgHint: fm["argument-hint"],
			Source:  source,
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func scanSkills(dir, prefix, source string) []Command {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Command
	for _, e := range entries {
		file := filepath.Join(dir, e.Name(), "SKILL.md")
		if _, err := os.Stat(file); err != nil {
			continue
		}
		fm := frontmatter(file)
		name := fm["name"]
		if name == "" {
			name = e.Name()
		}
		out = append(out, Command{Name: prefix + name, Desc: fm["description"], Source: source})
	}
	return out
}

type plugin struct{ name, path string }

// plugins reads installed_plugins.json: user-scope plugins, plus local and
// project ones installed for a folder that contains cwd.
func plugins(home, cwd string) []plugin {
	b, err := os.ReadFile(filepath.Join(home, "plugins", "installed_plugins.json"))
	if err != nil {
		return nil
	}
	var f struct {
		Plugins map[string][]struct {
			Scope       string `json:"scope"`
			ProjectPath string `json:"projectPath"`
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	var out []plugin
	for key, installs := range f.Plugins {
		name, _, _ := strings.Cut(key, "@")
		for _, in := range installs {
			if in.Scope == "user" || (in.ProjectPath != "" && within(cwd, in.ProjectPath)) {
				out = append(out, plugin{name: name, path: in.InstallPath})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// frontmatter reads the simple key: value lines of a YAML front matter
// block. Folded values (key: > / |) are joined into one line.
func frontmatter(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return out
	}
	var folded string
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		if folded != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			out[folded] = strings.TrimSpace(out[folded] + " " + strings.TrimSpace(line))
			continue
		}
		folded = ""
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(k, " ") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if v == ">" || v == "|" || v == ">-" || v == "|-" {
			folded = k
			v = ""
		}
		out[k] = strings.Trim(v, `"'`)
	}
	return out
}
