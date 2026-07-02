package tui

import (
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

// Handling of /compact boundaries: a summary-only compact turn does not get its own row;
// the » badge is carried over to the next real turn.
func TestCompactSummaryOnlyTurnSkippedAndBadgeCarried(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "first question", Prompt: "first question"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
		// Summary-only /compact boundary node (no real content).
		{ID: "c1", Parent: "a1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventUser, Text: "(auto summary)", RawType: "compact_summary"}}},
		{ID: "u2", Parent: "c1", Timestamp: ts(4), Events: []domain.Event{{Kind: domain.EventUser, Text: "next question", Prompt: "next question"}}},
	})
	s := domain.Session{PluginID: "claude", SessionID: "x"}
	m := Model{width: 120, height: 20, detailSession: &s}
	upd, _ := m.Update(convMsg{c: &c, reset: true})
	m = upd.(Model)

	var turns []detailRow
	for _, r := range m.detailRows {
		if r.Kind == "turn" {
			turns = append(turns, r)
		}
	}
	// Summary-only turns are not emitted -> 2 displayed turns (u1, u2).
	if len(turns) != 2 {
		t.Fatalf("displayed turn count=%d want 2 (summary-only compact is skipped)", len(turns))
	}
	// Newest on top: turns[0]=u2 (chron2, carried badge), turns[1]=u1 (chron0).
	if turns[0].TurnIndex != 2 || !turns[0].Badge {
		t.Fatalf("» badge not carried over to the newest turn: idx=%d badge=%v", turns[0].TurnIndex, turns[0].Badge)
	}
	if turns[1].TurnIndex != 0 || turns[1].Badge {
		t.Fatalf("first turn has wrong state: idx=%d badge=%v", turns[1].TurnIndex, turns[1].Badge)
	}
}

// A /compact turn that carries real content is not skipped; it gets the » badge.
func TestCompactTurnWithContentGetsBadgeNotSkipped(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "question", Prompt: "question"}}},
		// A compact summary plus real content (assistant) within the same turn.
		{ID: "c1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventUser, Text: "(summary)", RawType: "compact_summary"}}},
		{ID: "a1", Parent: "c1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "work"}}},
	})
	s := domain.Session{PluginID: "claude", SessionID: "x"}
	m := Model{width: 120, height: 20, detailSession: &s}
	upd, _ := m.Update(convMsg{c: &c, reset: true})
	m = upd.(Model)
	var turns []detailRow
	for _, r := range m.detailRows {
		if r.Kind == "turn" {
			turns = append(turns, r)
		}
	}
	// Two turns: u1 and compact(c1,a1). The compact turn gets the badge.
	if len(turns) != 2 {
		t.Fatalf("displayed turn count=%d want 2", len(turns))
	}
	if !turns[0].Badge {
		t.Fatalf("compact turn with real content is missing the » badge")
	}
}
