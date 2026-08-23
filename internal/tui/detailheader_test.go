package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

// The working directory has a header line to itself, so a deep path is readable in full
// instead of being middle-truncated to share the line with the title.
func TestDetailHeaderShowsFullCWDOnItsOwnLine(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Date(2026, 6, 23, 1, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "ask", Prompt: "ask"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Date(2026, 6, 23, 1, 0, 2, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
	})
	cwd := "/very/long/project/path/to/some/deeply/nested/repository"
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: cwd, Title: "the title"}
	m := Model{width: 120, height: 12, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)

	out := stripANSI(m.detailView())
	lines := strings.Split(out, "\n")
	if len(lines) != 12 {
		t.Fatalf("detail view should fill terminal height, got %d lines:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[1], cwd) {
		t.Fatalf("cwd line (line 1) must show the full path %q, got %q\n%s", cwd, lines[1], out)
	}
	if strings.Contains(lines[1], "the title") {
		t.Fatalf("cwd line must not carry the title: %q", lines[1])
	}
	if !strings.Contains(lines[2], "the title") {
		t.Fatalf("title line (line 2) missing the title, got %q\n%s", lines[2], out)
	}
	if !strings.Contains(lines[len(lines)-1], "o resume") {
		t.Fatalf("footer should stay on the last line, got %q", lines[len(lines)-1])
	}
}

// A terminal narrower than the path falls back to shortCWD's middle truncation rather
// than overflowing the line.
func TestDetailHeaderCWDLineFitsNarrowTerminal(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Date(2026, 6, 23, 1, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "ask", Prompt: "ask"}}},
	})
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/very/long/project/path/to/some/deeply/nested/repository", Title: "t"}
	m := Model{width: 40, height: 10, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)

	line := strings.Split(stripANSI(m.detailView()), "\n")[1]
	if w := len([]rune(line)); w > m.width-1 {
		t.Fatalf("cwd line width=%d exceeds terminal width-1=%d: %q", w, m.width-1, line)
	}
	if !strings.Contains(line, "…") {
		t.Fatalf("a path wider than the terminal should be truncated with an ellipsis, got %q", line)
	}
}
