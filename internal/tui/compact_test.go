package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/core/domain"
	tea "github.com/charmbracelet/bubbletea"
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
	// Newest on top: turns[0]=u2 (public index 1, carried badge), turns[1]=u1 (index 0).
	if turns[0].TurnIndex != 1 || !turns[0].Badge {
		t.Fatalf("» badge not carried over to the newest turn: idx=%d badge=%v", turns[0].TurnIndex, turns[0].Badge)
	}
	if turns[1].TurnIndex != 0 || turns[1].Badge {
		t.Fatalf("first turn has wrong state: idx=%d badge=%v", turns[1].TurnIndex, turns[1].Badge)
	}
	m.turnOpen = true
	m.detailCursor = m.firstTurnRow()
	if got := m.detailView(); !strings.Contains(got, "turn #2/2") {
		t.Fatalf("open turn header should use the contiguous public number:\n%s", got)
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

func TestCompactBoundaryKeepsForksReachable(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after bool
	}{
		{"middle", true, true},
		{"trailing", true, false},
		{"leading", false, true},
		{"only", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := domain.Session{PluginID: "codex", SessionID: "parent"}
			children := []domain.Session{
				{PluginID: "codex", SessionID: "child1", ParentSessionID: "parent", UpdatedAt: time.Unix(1, 0)},
				{PluginID: "codex", SessionID: "child2", ParentSessionID: "parent", UpdatedAt: time.Unix(2, 0)},
			}
			var nodes []domain.ConvNode
			path := []string{}
			if tc.before {
				nodes = append(nodes, domain.ConvNode{ID: "before", Events: []domain.Event{{Kind: domain.EventUser, Text: "before", Prompt: "before"}}})
				path = append(path, "before")
			}
			prev := ""
			if tc.before {
				prev = "before"
			}
			nodes = append(nodes, domain.ConvNode{ID: "compact", Parent: prev, Events: []domain.Event{{Kind: domain.EventUser, RawType: domain.RawCompactSummary, Text: "hidden summary"}}})
			path = append(path, "compact")
			if tc.after {
				nodes = append(nodes, domain.ConvNode{ID: "after", Parent: "compact", Events: []domain.Event{{Kind: domain.EventUser, Text: "after", Prompt: "after"}}})
				path = append(path, "after")
			}
			origins := map[string]app.NodeOrigin{}
			for _, id := range path {
				origins[id] = app.NodeOrigin{Session: parent, NodeID: id, ActiveLeaf: id == path[len(path)-1]}
			}
			for _, s := range children {
				nodes = append(nodes, domain.ConvNode{ID: s.SessionID, Parent: "compact", Events: []domain.Event{{Kind: domain.EventUser, Text: s.SessionID, Prompt: s.SessionID}}})
				origins[s.SessionID] = app.NodeOrigin{Session: s, NodeID: s.SessionID, ActiveLeaf: true}
			}
			c := domain.NewConversation(nodes)
			c.ActiveLeaf = path[len(path)-1]
			c.ForkRoots = []string{"child1", "child2"}
			m := Model{width: 180, height: 30, detailSession: &parent, sessions: append([]domain.Session{parent}, children...)}
			u, _ := m.Update(convMsg{c: &c, origins: origins, reset: true})
			m = u.(Model)
			var branches []detailRow
			wantTurns := 0
			if tc.before {
				wantTurns++
			}
			if tc.after {
				wantTurns++
			}
			turns := 0
			for _, row := range m.detailRows {
				if row.Kind == "turn" {
					if row.TurnIndex != wantTurns-1-turns {
						t.Fatalf("turn index=%d", row.TurnIndex)
					}
					turns++
				}
				if row.Kind == "branch" {
					branches = append(branches, row)
				}
			}
			if turns != wantTurns || len(branches) != 2 {
				t.Fatalf("turns=%d want %d; forks=%d want 2 path=%v rows=%v", turns, wantTurns, len(branches), m.currentDetailPath(), m.detailRows)
			}
			if branches[0].Root != "child2" || branches[1].Root != "child1" {
				t.Fatalf("unexpected sibling order: %v", branches)
			}
			if branches[0].LastBranch || !branches[1].LastBranch {
				t.Fatal("wrong sibling connectors")
			}
			view := stripANSI(m.detailView())
			if strings.Contains(view, "hidden summary") {
				t.Fatalf("compact text leaked: %s", view)
			}
			for _, child := range children {
				focused := m
				for i, row := range focused.detailRows {
					if row.Kind == "branch" && row.Root == child.SessionID {
						focused.detailCursor = i
					}
				}
				u, cmd := focused.Update(tea.KeyMsg{Type: tea.KeyEnter})
				got := u.(Model)
				if cmd != nil || got.detailSession == nil || got.detailSession.Key() != child.Key() {
					t.Fatalf("Enter failed for %s", child.SessionID)
				}
				if len(got.detailTurns) != 1 || got.detailTurns[0][0] != child.SessionID {
					t.Fatalf("wrong child conversation: %v", got.detailTurns)
				}
			}
		})
	}
}
