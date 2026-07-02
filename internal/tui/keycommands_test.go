package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentcarto/core/domain"
)

func keyRunes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// In the session list, pressing Esc after a search has been committed clears the search filter.
func TestListEscClearsSearchFilter(t *testing.T) {
	m := Model{view: "time", width: 100, height: 20, sessions: []domain.Session{
		{PluginID: "p", SessionID: "a", Title: "apple", UpdatedAt: time.Unix(2, 0)},
		{PluginID: "p", SessionID: "b", Title: "banana", UpdatedAt: time.Unix(1, 0)},
	}}
	m.query.SetValue("apple")
	m.filter()
	m.cursor = 1
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = u.(Model)
	if m.query.Value() != "" {
		t.Fatalf("Esc did not clear the search query: %q", m.query.Value())
	}
	if m.cursor != 0 {
		t.Fatalf("clearing with Esc did not move the cursor back to the top: %d", m.cursor)
	}
}

func turnListModel(t *testing.T) Model {
	ts := func(h int) time.Time { return time.Date(2026, 6, 23, h, 0, 0, 0, time.Local) }
	// Intentional Japanese (multibyte) headlines: the search tests below query "りんご" (apple)
	// rune-by-rune and assert it matches the two "りんご"-containing turns, exercising
	// multibyte search/navigation. "みかんの話" (a tangerine topic) is the non-matching turn.
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "りんごの話"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "ok"}}},
		{ID: "u2", Parent: "a1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventUser, Text: "みかんの話"}}},
		{ID: "a2", Parent: "u2", Timestamp: ts(4), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "ok2"}}},
		{ID: "u3", Parent: "a2", Timestamp: ts(5), Events: []domain.Event{{Kind: domain.EventUser, Text: "りんご再び"}}},
		{ID: "a3", Parent: "u3", Timestamp: ts(6), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "ok3"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s", CWD: "/repo", Title: "t"}
	m := Model{width: 120, height: 20, detailSession: &s}
	upd, _ := m.Update(convMsg{c: &c, reset: true})
	return upd.(Model)
}

// In the turn list, / starts a search, query input then Enter jumps to the first hit, and n jumps to the next hit.
func TestTurnListSearchNavigatesToHits(t *testing.T) {
	m := turnListModel(t)
	send := func(msg tea.Msg) { u, _ := m.Update(msg); m = u.(Model) }
	send(keyRunes("/"))
	if !m.turnSearching {
		t.Fatal("/ did not start a search")
	}
	for _, r := range "りんご" {
		send(keyRunes(string(r)))
	}
	hits := m.turnListHits()
	if len(hits) != 2 {
		t.Fatalf("hit count for \"りんご\"=%d want 2", len(hits))
	}
	send(tea.KeyMsg{Type: tea.KeyEnter}) // commit and jump to the first hit
	if m.turnSearching {
		t.Fatal("Enter did not end search input")
	}
	first := m.detailCursor
	if first != hits[0] {
		t.Fatalf("Enter did not jump to the first hit: cursor=%d want %d", first, hits[0])
	}
	send(keyRunes("n")) // next hit
	if m.detailCursor != hits[1] {
		t.Fatalf("n did not jump to the next hit: cursor=%d want %d", m.detailCursor, hits[1])
	}
	send(tea.KeyMsg{Type: tea.KeyEsc}) // clear the query
	if m.turnQuery != "" {
		t.Fatalf("Esc did not clear the query: %q", m.turnQuery)
	}
}

// In the full turn view, Tab/[ move between blocks.
func TestTurnFullBlockJump(t *testing.T) {
	m := turnListModel(t)
	send := func(msg tea.Msg) { u, _ := m.Update(msg); m = u.(Model) }
	send(tea.KeyMsg{Type: tea.KeyEnter}) // open the newest turn
	if !m.turnOpen {
		t.Fatal("Enter did not open the full turn view")
	}
	startBlock := m.blockAtCursor()
	send(tea.KeyMsg{Type: tea.KeyTab}) // Tab to the next block heading
	if m.blockAtCursor() <= startBlock && len(m.turnBlocks) > 1 {
		t.Fatalf("Tab did not move to the next block: %d -> %d", startBlock, m.blockAtCursor())
	}
	prev := m.blockAtCursor()
	send(keyRunes("[")) // [ to the previous block
	if m.blockAtCursor() >= prev && prev > 0 {
		t.Fatalf("[ did not move to the previous block: %d -> %d", prev, m.blockAtCursor())
	}
}

// In the full turn view, ← folds the expanded block under the cursor (mirroring
// → which expands it); when the block is already folded, ← leaves the view.
func TestTurnFullLeftFoldsThenCloses(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Events: []domain.Event{{Kind: domain.EventUser, Text: "q"}}},
		{ID: "a", Parent: "u", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "l1\nl2\nl3"}}},
	})
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/repo", Title: "t"}
	m := Model{width: 120, height: 20, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)
	send := func(msg tea.Msg) { u, _ := m.Update(msg); m = u.(Model) }
	send(tea.KeyMsg{Type: tea.KeyEnter}) // open the newest turn
	if !m.turnOpen {
		t.Fatal("Enter did not open the full turn view")
	}
	blk := -1
	for i, b := range m.turnBlocks {
		if len(b.Body) > 0 {
			blk = i
			break
		}
	}
	if blk < 0 {
		t.Fatal("no foldable block in the fixture")
	}
	m.turnExpanded[blk] = true
	m.turnCursor = m.turnBlockHeaderLine(blk)
	send(tea.KeyMsg{Type: tea.KeyLeft})
	if !m.turnOpen {
		t.Fatal("← closed the view instead of folding the expanded block")
	}
	if m.turnExpanded[blk] {
		t.Fatal("← did not fold the expanded block")
	}
	if m.turnCursor != m.turnBlockHeaderLine(blk) {
		t.Fatalf("cursor did not stay on the folded block header: %d", m.turnCursor)
	}
	send(tea.KeyMsg{Type: tea.KeyLeft})
	if m.turnOpen {
		t.Fatal("second ← did not leave the view")
	}
}

// The turn-list search query is inherited into the full turn view when a turn is opened, jumping to the first hit.
func TestSearchQueryInheritedTurnListToTurnFull(t *testing.T) {
	m := turnListModel(t)
	m.turnQuery = "ok3" // simulate a search already performed in the turn list
	send := func(msg tea.Msg) { u, _ := m.Update(msg); m = u.(Model) }
	send(tea.KeyMsg{Type: tea.KeyEnter}) // open the newest turn (u3+a3 "ok3")
	if m.turnFullQuery != "ok3" {
		t.Fatalf("search not inherited into the full turn view: %q", m.turnFullQuery)
	}
	lines := m.turnFullLines()
	if m.turnCursor < 0 || m.turnCursor >= len(lines) || !strings.Contains(strings.ToLower(lines[m.turnCursor].text), "ok3") {
		t.Fatalf("did not jump to the first hit after inheriting: cursor=%d", m.turnCursor)
	}
}

// On a reset (such as a parent jump), nothing is inherited and the turn opens with an empty query.
func TestSearchQueryNotInheritedWhenTurnQueryEmpty(t *testing.T) {
	m := turnListModel(t)
	m.turnQuery = "" // nothing to inherit from
	send := func(msg tea.Msg) { u, _ := m.Update(msg); m = u.(Model) }
	send(tea.KeyMsg{Type: tea.KeyEnter})
	if m.turnFullQuery != "" {
		t.Fatalf("query should have been empty but was inherited: %q", m.turnFullQuery)
	}
}

// Searching with / in the full turn view auto-expands blocks containing a hit and jumps to the hit line.
func TestTurnFullSearchExpandsAndJumps(t *testing.T) {
	m := turnListModel(t)
	send := func(msg tea.Msg) { u, _ := m.Update(msg); m = u.(Model) }
	send(tea.KeyMsg{Type: tea.KeyEnter}) // open the turn (u3 "りんご再び" + a3)
	send(keyRunes("/"))
	for _, r := range "ok3" {
		send(keyRunes(string(r)))
	}
	if len(m.turnFullHits()) == 0 {
		t.Fatal("in-turn search found no hits (block auto-expansion missed)")
	}
	send(tea.KeyMsg{Type: tea.KeyEnter})
	lines := m.turnFullLines()
	if m.turnCursor < 0 || m.turnCursor >= len(lines) || !strings.Contains(strings.ToLower(lines[m.turnCursor].text), "ok3") {
		t.Fatalf("Enter did not jump to the hit line: cursor=%d", m.turnCursor)
	}
}
