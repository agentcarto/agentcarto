package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

// A reload rebuilds detailRows under an open turn. If the cursor no longer lands
// on a turn row, the turn view has nothing to render: openCurrentTurn closes it.
// Leaving turnOpen set made detailView and turnFullView call each other until the
// stack overflowed.
func TestReloadOffTurnRowClosesTurnView(t *testing.T) {
	ts := time.Date(2026, 7, 10, 1, 0, 0, 0, time.Local)
	turn := []domain.ConvNode{
		{ID: "u1", Timestamp: ts, Events: []domain.Event{{Kind: domain.EventUser, Text: "go", Prompt: "go"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts, Events: []domain.Event{{Kind: domain.EventAssistant, Text: "done"}}},
	}
	c := domain.NewConversation(turn)
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s", CWD: "/proj"}
	m := Model{width: 120, height: 20, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)
	for i, r := range m.detailRows {
		if r.Kind == "turn" {
			m.detailCursor = i
		}
	}
	m.openCurrentTurn(true)
	if !m.turnOpen {
		t.Fatal("test setup: the turn did not open")
	}

	// Park the cursor off any turn row, as a reload that rebuilds the rows can, then
	// let the reload path (reset=false) run.
	m.detailCursor = -1
	m.openCurrentTurn(false)
	if m.turnOpen {
		t.Fatal("the turn view stayed open with the cursor off a turn row")
	}
	// Rendering must terminate and show the turn list, not recurse.
	if out := m.detailView(); strings.TrimSpace(out) == "" {
		t.Fatal("detailView rendered nothing")
	}

	// Even if some future path sets turnOpen without a turn under the cursor, the
	// render falls back to the turn list. Before the fix this overflowed the stack.
	m.turnOpen = true
	m.detailCursor = -1
	if out := m.detailView(); strings.TrimSpace(out) == "" {
		t.Fatal("detailView rendered nothing with turnOpen and no turn row")
	}
}
