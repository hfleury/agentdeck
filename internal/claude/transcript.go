package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// Message is one turn of a conversation, text only.
type Message struct {
	Role string // "user" or "assistant"
	Text string
}

// ReadTranscript returns the user and assistant text of a transcript, in
// order. Tool calls and results, thinking, images, meta lines, sub-agent
// lines and prompts Claude submits by itself are left out; consecutive
// assistant text blocks are joined into one message.
func ReadTranscript(path string) ([]Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var line struct {
			Type        string `json:"type"`
			IsMeta      bool   `json:"isMeta"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.IsMeta || line.IsSidechain {
			continue
		}
		if line.Type != "user" && line.Type != "assistant" {
			continue
		}
		text := contentText(line.Message.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if line.Type == "user" {
			if IsSystemPrompt(text) {
				continue
			}
			text = stripPasted(text)
		}
		if n := len(out); n > 0 && line.Type == "assistant" && out[n-1].Role == "assistant" {
			out[n-1].Text += "\n\n" + text
			continue
		}
		out = append(out, Message{Role: line.Type, Text: strings.TrimSpace(text)})
	}
	return out, sc.Err()
}

// contentText extracts the text of a message content: a string, or the
// text parts of a list.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b []string
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			b = append(b, p.Text)
		}
	}
	return strings.Join(b, "\n")
}

// stripPasted removes <pasted_content> wrappers, keeping their content.
func stripPasted(s string) string {
	for {
		i := strings.Index(s, "<pasted_content")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], ">")
		if j < 0 {
			break
		}
		s = s[:i] + s[i+j+1:]
	}
	return strings.ReplaceAll(s, "</pasted_content>", "")
}
