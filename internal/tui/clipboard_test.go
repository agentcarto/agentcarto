package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

// A turn is copied as the labeled exchange it is: prompt and reply in order,
// with the tool traffic and injected system messages left out.
func TestTurnCopyTextKeepsPromptAndReply(t *testing.T) {
	events := []domain.Event{
		{Kind: domain.EventUser, Text: "system reminder"}, // no Prompt: injected, not typed
		{Kind: domain.EventUser, Text: "調べて", Prompt: "調べて"},
		{Kind: domain.EventAssistant, Text: "まず読む\n"},
		{Kind: domain.EventToolCall, Text: `{"path":"a.go"}`, ToolName: "Read"},
		{Kind: domain.EventToolResult, Text: "package main"},
		{Kind: domain.EventQueued, Text: "急いで", Prompt: "急いで"},
		{Kind: domain.EventAssistant, Text: "  "}, // whitespace only: nothing to copy
		{Kind: domain.EventAssistant, Text: "結論"},
	}
	want := "USER\n調べて\n\nASSISTANT\nまず読む\n\nUSER (queued)\n急いで\n\nASSISTANT\n結論"
	if got := turnCopyText(events); got != want {
		t.Fatalf("turnCopyText=%q want %q", got, want)
	}
	if got := turnCopyText(events[:1]); got != "" {
		t.Fatalf("a turn with neither prompt nor reply yielded %q, want empty", got)
	}
}

// A block with a body copies the body; a body-less block falls back to its label.
func TestBlockCopyText(t *testing.T) {
	if got, want := blockCopyText(turnBlock{Label: "ASSISTANT", Body: []string{"line1", "line2", ""}}), "line1\nline2"; got != want {
		t.Fatalf("blockCopyText=%q want %q", got, want)
	}
	if got, want := blockCopyText(turnBlock{Label: "  Bash ls -la  "}), "Bash ls -la"; got != want {
		t.Fatalf("blockCopyText of a body-less block=%q want %q", got, want)
	}
}

func copyTestModel(t *testing.T) Model {
	t.Helper()
	ts := func(h int) time.Time { return time.Date(2026, 8, 21, h, 0, 0, 0, time.Local) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "できた"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s", CWD: "/repo", Title: "t"}
	m := Model{width: 120, height: 20, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)
	for i, r := range m.detailRows {
		if r.Kind == "turn" {
			m.detailCursor = i
			return m
		}
	}
	t.Fatal("test setup: no turn row")
	return m
}

// In the turn list, y copies the selected turn's exchange and says so. The
// returned command is never run: it would write the clipboard of whoever runs
// the tests.
func TestTurnListCopyKey(t *testing.T) {
	m := copyTestModel(t)
	u, _ := m.Update(keyRunes("y"))
	m = u.(Model)
	// USER + prompt + blank + ASSISTANT + reply
	if !strings.HasPrefix(m.flash, "Copied 5 lines ") {
		t.Fatalf("flash=%q want a 5-line copy notice", m.flash)
	}
	if _, cmd := m.copyTurnExchange(); cmd == nil {
		t.Fatal("copying a turn with a reply returned no command")
	}
}

// A turn with neither prompt nor reply reports that instead of copying nothing.
func TestTurnListCopyKeyWithNothingToCopy(t *testing.T) {
	m := copyTestModel(t)
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "t1", Timestamp: time.Date(2026, 8, 21, 1, 0, 0, 0, time.Local),
			Events: []domain.Event{{Kind: domain.EventToolResult, Text: "output"}}},
	})
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)
	for i, r := range m.detailRows {
		if r.Kind == "turn" {
			m.detailCursor = i
		}
	}
	u, _ = m.Update(keyRunes("y"))
	m = u.(Model)
	if !strings.Contains(m.flash, "no prompt or reply") {
		t.Fatalf("flash=%q want a nothing-to-copy notice", m.flash)
	}
	if _, cmd := m.copyTurnExchange(); cmd != nil {
		t.Fatal("a turn with nothing to copy still returned a copy command")
	}
}

// In the full turn view, y copies the block under the cursor.
func TestTurnFullCopyKey(t *testing.T) {
	m := copyTestModel(t)
	m.openCurrentTurn(true)
	if !m.turnOpen {
		t.Fatal("test setup: the turn did not open")
	}
	for i, line := range m.turnFullLines() {
		if strings.Contains(line.text, "できた") {
			m.turnCursor = i
			break
		}
	}
	u, _ := m.Update(keyRunes("y"))
	m = u.(Model)
	if !strings.HasPrefix(m.flash, "Copied 1 line ") {
		t.Fatalf("flash=%q want a single-line copy notice", m.flash)
	}
	if _, cmd := m.copyTurnBlock(); cmd == nil {
		t.Fatal("copying a block with a body returned no command")
	}
}
