package transcript

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentcarto/core/domain"
)

// FuzzMarkdown renders arbitrary conversation text. What the document promises
// is that it is valid UTF-8 and that a turn heading appears exactly when
// something was rendered for that turn. (Bodies are reproduced as they are —
// a reply that contains a heading or an unclosed fence keeps it, which is the
// point of not escaping what was said.)
func FuzzMarkdown(f *testing.F) {
	f.Add("やって", "できた", "Bash", "$ go test", "go test\n./...")
	f.Add("", "", "", "", "")
	f.Add("```go", "``` fence", "Write", "x", "a\n```\nb")
	f.Add("# heading", "## another", "", "", "")
	f.Fuzz(func(t *testing.T, prompt, reply, tool, arg, detail string) {
		for _, s := range []string{prompt, reply, tool, arg, detail} {
			if !utf8.ValidString(s) {
				t.Skip()
			}
		}
		c := domain.NewConversation([]domain.ConvNode{{ID: "u1", Events: []domain.Event{
			{Kind: domain.EventUser, Text: prompt, Prompt: prompt},
			{Kind: domain.EventToolCall, ToolName: tool, ToolArg: arg, ToolDetail: detail},
			{Kind: domain.EventAssistant, Text: reply},
		}}})
		s := domain.Session{AgentType: "claude", SessionID: "x", Title: "t"}
		turns := Turns(c, c.ActivePath())
		for _, mode := range []ToolMode{ToolsFull, ToolsLabel, ToolsNone} {
			doc, rendered := Markdown(s, c, turns, Options{Tools: mode})
			if !utf8.ValidString(doc) {
				t.Fatalf("document is not valid UTF-8")
			}
			if got := strings.Count(doc, "\n## Turn "); got != rendered {
				t.Fatalf("Tools=%d: %d turn headings for %d rendered turns\n%s", mode, got, rendered, doc)
			}
			_ = doc
		}
		// A tool call goes into a block whose fence is longer than any backtick run
		// inside it, so the call cannot escape and swallow the document.
		if lines := toolEntry(tool, arg, detail, Options{Tools: ToolsFull}); len(lines) > 3 {
			open, close := strings.TrimSpace(lines[2]), strings.TrimSpace(lines[len(lines)-1])
			if open != close || !strings.HasPrefix(open, "```") {
				t.Fatalf("tool block is not delimited by one fence: %q … %q", open, close)
			}
			for _, ln := range lines[3 : len(lines)-1] {
				if run := longestBacktickRun(ln); run >= len(open) {
					t.Fatalf("a run of %d backticks inside a fence of %d: %q", run, len(open), ln)
				}
			}
		}

		// The outline never carries bodies, whatever the text was.
		if o := Outline(s, c, turns, Options{}); strings.Contains(o, "**USER**") || strings.Contains(o, "**ASSISTANT**") {
			t.Fatalf("outline carries a body:\n%s", o)
		}
	})
}

// longestBacktickRun is the longest run of backticks in a line.
func longestBacktickRun(s string) int {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	return longest
}
