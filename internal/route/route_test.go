package route

import "testing"

func builtin(name string) bool { return name == "model" || name == "compact" || name == "close" }

func TestClassify(t *testing.T) {
	tests := []struct {
		name, in string
		want     Route
	}{
		{"pasted claude command with unknown arg", "/model ahsjnda", Route{Kind: Claude, Name: "model", Text: "/model ahsjnda"}},
		{"trailing newline from copy", "/model opus\n", Route{Kind: Claude, Name: "model", Text: "/model opus"}},
		{"CRLF and leading spaces", "  /compact\r\n", Route{Kind: Claude, Name: "compact", Text: "/compact"}},
		{"ANSI color codes from a terminal copy", "\x1b[1m/model\x1b[0m opus", Route{Kind: Claude, Name: "model", Text: "/model opus"}},
		{"unknown command still goes to claude", "/brand-new-thing x", Route{Kind: Claude, Name: "brand-new-thing", Text: "/brand-new-thing x"}},
		{"plugin command", "/acme:plan T-12", Route{Kind: Claude, Name: "acme:plan", Text: "/acme:plan T-12"}},
		{"app command", "/new ~/api", Route{Kind: App, Name: "new", Args: "~/api"}},
		{"app command without args", "/restart", Route{Kind: App, Name: "restart"}},
		{"clash: claude wins", "/close", Route{Kind: Claude, Name: "close", Text: "/close"}},
		{"clash: // picks the app", "//close", Route{Kind: App, Name: "close"}},
		{"// with a non-app name is text", "//comment", Route{Kind: Text, Text: "//comment"}},
		{"backslash escape", `\/etc/hosts is wrong`, Route{Kind: Text, Text: "/etc/hosts is wrong"}},
		{"path is not a command", "/etc/hosts is wrong", Route{Kind: Text, Text: "/etc/hosts is wrong"}},
		{"lone slash", "/", Route{Kind: Text, Text: "/"}},
		{"plain text", "fix the bug\nin api.go", Route{Kind: Text, Text: "fix the bug\nin api.go"}},
		{"multi-line command", "/review\nfocus on auth", Route{Kind: Claude, Name: "review", Text: "/review\nfocus on auth"}},
		{"empty", " \n\t ", Route{Kind: Empty}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.in, builtin); got != tt.want {
				t.Fatalf("Classify(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeKeepsTabsAndNewlines(t *testing.T) {
	if got := Normalize("a\tb\r\nc\x07"); got != "a\tb\nc" {
		t.Fatalf("got %q", got)
	}
}
