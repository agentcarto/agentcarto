package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentcarto/core/domain"
)

// rewoundDetail opens a session that was rewound to after its turn #2, whose
// rewound line was itself rewound to after its own turn #1, and descends into
// the first rewind the way the reader does.
func rewoundDetail(t *testing.T) Model {
	t.Helper()
	ts := func(s int) time.Time { return time.Date(2026, 6, 23, 1, 0, s, 0, time.Local) }
	user := func(id, parent string, s int, text string) domain.ConvNode {
		return domain.ConvNode{ID: id, Parent: parent, Timestamp: ts(s), Events: []domain.Event{{Kind: domain.EventUser, Text: text, Prompt: text}}}
	}
	reply := func(id, parent string, s int) domain.ConvNode {
		return domain.ConvNode{ID: id, Parent: parent, Timestamp: ts(s), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "reply " + id}}}
	}
	c := domain.NewConversation([]domain.ConvNode{
		user("u1", "", 1, "ask"), reply("a1", "u1", 2),
		user("u2", "a1", 3, "second"), reply("a2", "u2", 4),
		// The line abandoned by the rewind, and the rewind inside it.
		user("r1", "a2", 5, "asked before rewind"), reply("r2", "r1", 6),
		user("n1", "r2", 7, "abandoned again"), reply("n2", "n1", 8),
		user("r3", "r2", 9, "deeper"), reply("r4", "r3", 10),
		// The session carried on here, after both.
		user("p3", "a2", 20, "after rewind"), reply("p4", "p3", 21),
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "session1", CWD: "/repo", Title: "ask"}
	m := Model{width: 120, height: 20, detailSession: &s, sessions: []domain.Session{s}}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)
	return descendInto(t, m, "r1")
}

// descendInto selects the branch row of root and enters it.
func descendInto(t *testing.T, m Model, root string) Model {
	t.Helper()
	for i, r := range m.detailRows {
		if r.Kind == "branch" && r.Root == root {
			m.detailCursor = i
			u, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			return u.(Model)
		}
	}
	t.Fatalf("no branch row for %s:\n%s", root, stripANSI(m.detailView()))
	return m
}

// A rewound line, like a fork, ends with a row under its turn #1 naming the turn
// it was rewound to, and the heading and the footer offer the same move.
func TestRewoundLineEndsWithRowToWhereItBranched(t *testing.T) {
	m := rewoundDetail(t)
	last := m.detailRows[len(m.detailRows)-1]
	if last.Kind != "origin" {
		t.Fatalf("last row kind=%q, want the origin row under turn #1", last.Kind)
	}
	// The line's oldest turn, numbered on from the #2 it branched after. Its own
	// branch rows sit between it and the origin row.
	for i := len(m.detailRows) - 2; i >= 0; i-- {
		if r := m.detailRows[i]; r.Kind == "turn" {
			if r.TurnIndex != 2 {
				t.Fatalf("the turn above the origin row is #%d, want #3", r.TurnIndex+1)
			}
			break
		}
	}
	view := stripANSI(m.detailView())
	if !strings.Contains(view, "↳#2 rewound from second") {
		t.Fatalf("the row does not name the turn the line was rewound to:\n%s", view)
	}
	if !strings.Contains(view, "(p)") || !strings.Contains(view, "p parent") {
		t.Fatalf("the heading and the footer do not offer the move the row makes:\n%s", view)
	}
}

// A rewound line goes on with the session's numbers rather than starting again
// at #1, at every depth, and the origin row names the turn it branched after by
// the number the line it came from shows.
func TestRewoundLinesNumberOnFromWhereTheyBranched(t *testing.T) {
	m := rewoundDetail(t)
	if got := turnNumbers(m); got != "4,3" {
		t.Fatalf("rewound line numbers %s, want 4,3 (on from #2)", got)
	}
	m = descendInto(t, m, "n1")
	if got := turnNumbers(m); got != "4" {
		t.Fatalf("nested rewound line numbers %s, want 4 (on from #3)", got)
	}
	if view := stripANSI(m.detailView()); !strings.Contains(view, "↳#3 rewound from asked before rewind") {
		t.Fatalf("the nested rewind does not name the #3 it branched after:\n%s", view)
	}
}

// The turn view counts up to the newest turn's number, not to how many turns
// the line lists, which for a rewound line are fewer.
func TestTurnViewOfARewoundLineCountsToItsNewestNumber(t *testing.T) {
	m := rewoundDetail(t)
	m.detailCursor = m.firstTurnRow()
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = u.(Model)
	if view := stripANSI(m.detailView()); !strings.Contains(view, "turn #4/4") {
		t.Fatalf("the newest turn of the rewound line is not shown as #4/4:\n%s", view)
	}
}

// turnNumbers lists the numbers of the turn rows, top to bottom.
func turnNumbers(m Model) string {
	var out []string
	for _, r := range m.detailRows {
		if r.Kind == "turn" {
			out = append(out, fmt.Sprint(r.TurnIndex+1))
		}
	}
	return strings.Join(out, ",")
}

// Every way out of a rewound line lands on the branch row that leads back into
// it, as jumping to a fork's parent lands on the fork point.
func TestLeavingARewoundLineLandsOnItsBranchRow(t *testing.T) {
	keys := map[string]tea.KeyMsg{
		"enter on the row": {Type: tea.KeyEnter},
		"p":                {Type: tea.KeyRunes, Runes: []rune{'p'}},
		"q":                {Type: tea.KeyRunes, Runes: []rune{'q'}},
		"left":             {Type: tea.KeyLeft},
		"esc":              {Type: tea.KeyEsc},
		"h":                {Type: tea.KeyRunes, Runes: []rune{'h'}},
	}
	for name, key := range keys {
		t.Run(name, func(t *testing.T) {
			m := rewoundDetail(t)
			m.detailCursor = len(m.detailRows) - 1
			u, _ := m.Update(key)
			m = u.(Model)
			if len(m.detailPathStack) != 1 {
				t.Fatalf("still in the rewound line: frames=%d", len(m.detailPathStack))
			}
			row, ok := m.selectedDetailRow()
			if !ok || row.Kind != "branch" || row.Root != "r1" {
				t.Fatalf("cursor is on %+v, want the branch row of the rewound line", row)
			}
			if m.detailCursor < m.detailOffset || m.detailCursor >= m.detailOffset+m.detailBodyRows() {
				t.Fatalf("branch row is outside the viewport: cursor=%d offset=%d", m.detailCursor, m.detailOffset)
			}
		})
	}
}

// The base line of a session that is not a fork has nowhere to lead back to.
func TestBaseLineOffersNoOriginAfterLeavingARewind(t *testing.T) {
	m := rewoundDetail(t)
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = u.(Model)
	for _, r := range m.detailRows {
		if r.Kind == "origin" {
			t.Fatalf("the base line offers an origin row:\n%s", stripANSI(m.detailView()))
		}
	}
	if view := stripANSI(m.detailView()); strings.Contains(view, "(p)") || strings.Contains(view, "p parent") {
		t.Fatalf("the base line offers p:\n%s", view)
	}
}
