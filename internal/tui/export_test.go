package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

// Only the prompt and the reply keep their bodies; the other events the export
// keeps become one-line entries, and the rest are dropped.
func TestEventMarkdownSelection(t *testing.T) {
	cases := []struct {
		name  string
		event domain.Event
		want  []string
	}{
		{"user", domain.Event{Kind: domain.EventUser, Text: "やって\n", Prompt: "やって"}, []string{"**USER**", "", "やって"}},
		{"queued", domain.Event{Kind: domain.EventQueued, Text: "急いで"}, []string{"**USER (queued)**", "", "急いで"}},
		{"assistant", domain.Event{Kind: domain.EventAssistant, Text: "できた"}, []string{"**ASSISTANT**", "", "できた"}},
		{"tool call", domain.Event{Kind: domain.EventToolCall, ToolName: "Read", ToolArg: "a.go"}, []string{"- Read a.go"}},
		{"multiline detail", domain.Event{Kind: domain.EventToolCall, ToolName: "Bash",
			ToolArg: "$ cat <<EOF body EOF", ToolDetail: "cat <<EOF\nbody\nEOF"},
			[]string{"- Bash", "", "  ```", "  cat <<EOF", "  body", "  EOF", "  ```"}},
		{"single-line detail", domain.Event{Kind: domain.EventToolCall, ToolName: "Bash",
			ToolArg: "$ go test ./...", ToolDetail: "go test ./..."}, []string{"- Bash $ go test ./..."}},
		{"task keeps its label only", domain.Event{Kind: domain.EventTask, ToolArg: "abc [done]", ToolDetail: "a\nreport"},
			[]string{"- TASK abc [done]"}},
		{"no arg", domain.Event{Kind: domain.EventToolCall, ToolName: "Read"}, []string{"- Read"}},
		{"unnamed tool", domain.Event{Kind: domain.EventToolCall, ToolArg: "x"}, []string{"- tool x"}},
		{"task", domain.Event{Kind: domain.EventTask, ToolArg: "explore"}, []string{"- TASK explore"}},
		{"attachment", domain.Event{Kind: domain.EventAttachment, ToolArg: "a.go"}, []string{"- attachment a.go"}},
		{"injected system", domain.Event{Kind: domain.EventUser, Text: "reminder"}, nil},
		{"thinking", domain.Event{Kind: domain.EventReasoning, Text: "考える"}, nil},
		{"tool result", domain.Event{Kind: domain.EventToolResult, Text: "package main"}, nil},
		{"system", domain.Event{Kind: domain.EventSystem, Text: "note"}, nil},
		{"empty reply", domain.Event{Kind: domain.EventAssistant, Text: "  "}, nil},
	}
	for _, c := range cases {
		got := eventMarkdown(c.event)
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s: eventMarkdown=%q want %q", c.name, got, c.want)
		}
	}
	// Nothing is truncated: a long single-line argument comes out whole.
	long := strings.Repeat("x", 3000)
	if got := eventMarkdown(domain.Event{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: long}); !strings.HasSuffix(got[0], long) {
		t.Errorf("a long argument was cut: %.60q…", got[0])
	}
}

// An argument that holds code fences of its own cannot break out of its block.
func TestToolEntryFenceOutgrowsTheArgument(t *testing.T) {
	got := toolEntry("Write", "# doc ```go x := 1 ```", "# doc\n```go\nx := 1\n```\n")
	if got[2] != "  ````" || got[len(got)-1] != "  ````" {
		t.Fatalf("fence=%q/%q want a four-backtick fence\n%q", got[2], got[len(got)-1], got)
	}
	for _, ln := range got[3 : len(got)-1] {
		if ln == "  ````" {
			t.Fatalf("the argument's own fence matched the block's:\n%q", got)
		}
	}
}

// Consecutive entries render as one tight list, not a paragraph each.
func TestTightenLists(t *testing.T) {
	in := []string{"- a", "", "- b", "", "  - c", "", "**USER**", "", "text",
		"- Bash", "", "  ```", "  - x", "", "  - y", "  ```", "- next"}
	want := []string{"- a", "- b", "  - c", "", "**USER**", "", "text",
		"- Bash", "", "  ```", "  - x", "", "  - y", "  ```", "- next"}
	if got := tightenLists(in); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tightenLists=%q want %q", got, want)
	}
}

// The document carries the session's metadata, then every displayed turn in
// chronological order under the number the turn view shows.
func TestExportMarkdownDocument(t *testing.T) {
	ts := func(h int) time.Time { return time.Date(2026, 8, 21, h, 4, 12, 0, time.UTC) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(10), Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって", Timestamp: ts(10)}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(11), Events: []domain.Event{
			{Kind: domain.EventToolCall, ToolName: "Read", ToolArg: "a.go", Text: "{}", Timestamp: ts(11)},
			{Kind: domain.EventToolResult, Text: "package main", Timestamp: ts(11)},
			{Kind: domain.EventReasoning, Text: "考える", Timestamp: ts(11)},
			{Kind: domain.EventAssistant, Text: "できた", Timestamp: ts(11)},
		}},
		{ID: "u2", Parent: "a1", Timestamp: ts(12), Events: []domain.Event{{Kind: domain.EventUser, Text: "次", Prompt: "次", Timestamp: ts(12)}}},
	})
	s := domain.Session{
		PluginID: "claude", AgentType: "claude", SessionID: "8f3a2b1c-4d5e", CWD: "/repo", Title: "りんごの話",
		StartedAt: ts(10), UpdatedAt: ts(12),
	}
	m := Model{width: 120, height: 20, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)

	got, _ := m.exportMarkdown()
	for _, want := range []string{
		"# りんごの話\n",
		"- **Agent**: claude\n",
		"- **Session**: 8f3a2b1c-4d5e\n",
		"- **CWD**: `/repo`\n",
		"- **Started**: 2026-08-21 10:04:12\n",
		"- **Updated**: 2026-08-21 12:04:12\n",
		"- **Turns**: 2\n",
		"## Turn 1 — 2026-08-21 10:04:12\n",
		"**USER**\n\nやって\n",
		"- Read a.go\n",
		"**ASSISTANT**\n\nできた\n",
		"## Turn 2 — 2026-08-21 12:04:12\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("export is missing %q\n---\n%s", want, got)
		}
	}
	// Tool output and thinking are left out.
	for _, unwanted := range []string{"package main", "考える", "result ("} {
		if strings.Contains(got, unwanted) {
			t.Errorf("export contains %q, which it should drop\n---\n%s", unwanted, got)
		}
	}
	// Chronological: turn 1 before turn 2, the reverse of the on-screen order.
	if strings.Index(got, "## Turn 1") > strings.Index(got, "## Turn 2") {
		t.Error("turns are not in chronological order")
	}
}

// A /compact boundary is marked, because the agent stopped seeing what came before it.
func TestExportMarksCompactBoundary(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "first", Prompt: "first"}}},
		{ID: "c1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventUser, Text: "(summary)", RawType: "compact_summary"}}},
		{ID: "u2", Parent: "c1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventUser, Text: "second", Prompt: "second"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "x", Title: "t"}
	m := Model{width: 120, height: 20, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)
	got, _ := m.exportMarkdown()
	if n := strings.Count(got, "_(context compacted here)_"); n != 1 {
		t.Fatalf("compact notices=%d want 1\n---\n%s", n, got)
	}
	// It belongs to the turn after the boundary, not the one before it.
	if strings.Index(got, "_(context compacted here)_") < strings.Index(got, "second") {
		if strings.Index(got, "first") > strings.Index(got, "_(context compacted here)_") {
			t.Error("the compact notice is attached to the wrong turn")
		}
	}
}

// A session with no title still produces a usable heading.
func TestExportUntitledSession(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "hi", Prompt: "hi"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "x"}
	m := Model{width: 120, height: 20, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)
	got, _ := m.exportMarkdown()
	if !strings.HasPrefix(got, "# (untitled)\n") {
		t.Fatalf("export of an untitled session starts with %.40q", got)
	}
	if strings.Contains(got, "**CWD**") {
		t.Error("a session without a cwd still got a CWD line")
	}
}

// The file name carries agent, short id and date, with anything unsafe folded away.
func TestExportFileName(t *testing.T) {
	day := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	cases := []struct{ agent, id, want string }{
		{"claude", "8f3a2b1c-4d5e-6f70", "claude-8f3a2b1c-20260821.md"},
		{"copilot", "a/b:c", "copilot-a-b-c-20260821.md"},
		{"", "", "session-20260821.md"},
	}
	for _, c := range cases {
		if got := exportFileName(c.agent, c.id, day); got != c.want {
			t.Errorf("exportFileName(%q, %q)=%q want %q", c.agent, c.id, got, c.want)
		}
	}
}

// An export never overwrites: the second one lands next to the first.
func TestWriteNewFileNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	first, err := writeNewFile(dir, "s-20260821.md", "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := writeNewFile(dir, "s-20260821.md", "two")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "s-20260821.md" || filepath.Base(second) != "s-20260821-2.md" {
		t.Fatalf("names=%q, %q want s-20260821.md, s-20260821-2.md", filepath.Base(first), filepath.Base(second))
	}
	if b, err := os.ReadFile(first); err != nil || string(b) != "one" {
		t.Fatalf("the first export was not left intact: %q %v", b, err)
	}
	if b, err := os.ReadFile(second); err != nil || string(b) != "two" {
		t.Fatalf("second export content=%q %v", b, err)
	}
}

// x writes the session to the directory agentcarto runs in and reports the file.
func TestExportKeyWritesFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "できた"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "8f3a2b1c-4d5e", CWD: "/repo", Title: "t"}
	m := Model{width: 120, height: 20, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)

	u, _ = m.Update(keyRunes("x"))
	m = u.(Model)
	if !strings.HasPrefix(m.flash, "Exported 1 turn to ./claude-8f3a2b1c-") {
		t.Fatalf("flash=%q want an export notice naming the file", m.flash)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory holds %d files (%v), want the single export", len(entries), err)
	}
	body, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := m.exportMarkdown()
	if string(body) != doc {
		t.Fatalf("the file does not hold the rendered document:\n%s", body)
	}
	if !strings.Contains(string(body), "**ASSISTANT**\n\nできた") {
		t.Fatalf("exported file is missing the reply:\n%s", body)
	}
}

// A turn holding only injected system messages produces no heading: an empty
// "## Turn 7" would be noise, and the numbering follows the turn view anyway.
func TestExportSkipsTurnsWithNothingToShow(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	c := domain.NewConversation([]domain.ConvNode{
		// Prompt-less user event: an injected reminder, dropped by the export.
		{ID: "s1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "<reminder>"}}},
		{ID: "u1", Parent: "s1", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "できた"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "x", Title: "t"}
	m := Model{width: 120, height: 20, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)
	got, turns := m.exportMarkdown()
	if n := strings.Count(got, "## Turn "); n != 1 {
		t.Fatalf("turn headings=%d want 1 (the system-only turn is skipped)\n---\n%s", n, got)
	}
	// The stated count is what the document holds, not what the screen lists.
	if turns != 1 || !strings.Contains(got, "- **Turns**: 1") {
		t.Errorf("turns=%d and the header disagree with the single heading\n---\n%s", turns, got)
	}
	if strings.Contains(got, "<reminder>") {
		t.Errorf("an injected system message reached the export:\n%s", got)
	}
}
