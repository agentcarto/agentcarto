package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/core/domain"
)

// forkConversation is the tree a fork is read from: the turn it was made at,
// then the parent's own continuation beside the fork's. Opening the fork session
// anchors the view at the root and focuses the fork's line, which is what
// focusLeaf ("f2") selects.
func forkConversation() domain.Conversation {
	ts := func(s int) time.Time { return time.Date(2026, 6, 23, 1, 0, s, 0, time.Local) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "ask", Prompt: "ask"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
		// The parent carried on here, later than the fork did.
		{ID: "p2", Parent: "a1", Timestamp: ts(8), Events: []domain.Event{{Kind: domain.EventUser, Text: "on without the fork", Prompt: "on without the fork"}}},
		// The fork's own line.
		{ID: "f1", Parent: "a1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventUser, Text: "asked in the fork", Prompt: "asked in the fork"}}},
		{ID: "f2", Parent: "f1", Timestamp: ts(4), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answered in the fork"}}},
	})
	// The host marks where the fork's own nodes begin when it stitches the two
	// sessions into one tree; the heading reads it to tell a fork from a rewind.
	c.ForkRoots = []string{"f1"}
	return c
}

// forkDetail opens a fork session the way the TUI does, with the frame on its
// own line focused automatically. listed says whether the parent session is in
// the list.
func forkDetail(listed bool) Model {
	c := forkConversation()
	child := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "child", CWD: "/repo", ParentSessionID: "parent", ForkAt: "a1"}
	m := Model{width: 120, height: 20, detailSession: &child, sessions: []domain.Session{child}}
	if listed {
		m.sessions = append(m.sessions, domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "parent", CWD: "/repo", Title: "where it came from"})
	}
	u, _ := m.Update(convMsg{c: &c, focusLeaf: "f2", reset: true})
	return u.(Model)
}

// A fork holds a copy of what was said before it, so the parent session is where
// that conversation carries on. The row leading there sits under turn #1 — the
// bottom of a list that runs newest first.
func TestForkParentRowSitsUnderTheOldestTurn(t *testing.T) {
	m := forkDetail(true)
	if len(m.detailPathStack) < 2 || !m.detailPathStack[len(m.detailPathStack)-1].followFocus {
		t.Fatalf("test setup: opening a fork should focus its own frame, stack=%d", len(m.detailPathStack))
	}
	if len(m.detailRows) == 0 {
		t.Fatal("no rows built")
	}
	last := m.detailRows[len(m.detailRows)-1]
	if last.Kind != "forkparent" {
		t.Fatalf("last row kind=%q want forkparent (the row belongs under turn #1)", last.Kind)
	}
	for _, r := range m.detailRows[:len(m.detailRows)-1] {
		if r.Kind == "forkparent" {
			t.Fatal("the fork-parent row must appear once, at the bottom")
		}
	}
	// The turn above it is #1, so the row really is under the oldest turn.
	above := m.detailRows[len(m.detailRows)-2]
	if above.Kind != "turn" || above.TurnIndex != 0 {
		t.Fatalf("row above the fork-parent row is %q index=%d, want turn #1", above.Kind, above.TurnIndex)
	}
	view := stripANSI(m.detailView())
	if !strings.Contains(view, "↳"+shortID("parent")+" forked from where it came from") {
		t.Fatalf("the row does not name the parent:\n%s", view)
	}
}

// The focused fork path is the continuation being read, so its own assistant
// reply is not an abandoned one-node rewind merely because the synthesized
// conversation's root path belongs to the parent session.
func TestForkTurnMarkDoesNotTreatItsReplyAsRewind(t *testing.T) {
	m := forkDetail(true)
	if len(m.detailTurns) != 1 {
		t.Fatalf("fork turns=%d want 1", len(m.detailTurns))
	}
	if got := m.turnMarkParts(m.detailTurns[0], time.Time{})[5]; got != "" {
		t.Fatalf("fork's own reply got a rewind mark %q:\n%s", got, stripANSI(m.detailView()))
	}
}

// An ordinary session has no parent, and nothing extra is appended to its turns.
func TestOrdinarySessionHasNoForkParentRow(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Date(2026, 6, 23, 1, 0, 1, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "ask", Prompt: "ask"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Date(2026, 6, 23, 1, 0, 2, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s", CWD: "/repo"}
	m := Model{width: 120, height: 20, detailSession: &s, sessions: []domain.Session{s}}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)
	for _, r := range m.detailRows {
		if r.Kind == "forkparent" {
			t.Fatal("a session with no parent should not offer a row to one")
		}
	}
}

// Descending into another line by hand changes what the list is showing, and a
// row about this session's origin no longer belongs to it.
func TestForkParentRowGoesAwayWhenDescendingByHand(t *testing.T) {
	m := forkDetail(true)
	m.detailPathStack = append(m.detailPathStack, detailFrame{path: []string{"p2"}, label: "rewind p2"})
	m.setDetailPath(m.currentDetailPath())
	for _, r := range m.detailRows {
		if r.Kind == "forkparent" {
			t.Fatal("a hand-entered frame should not carry the fork-parent row")
		}
	}
	if got := stripANSI(m.detailView()); strings.Contains(got, "(p)") {
		t.Fatalf("the heading offers (p) where the row is gone:\n%s", got)
	}
}

// The heading's (p) and the row are the same offer, so they appear together.
// They did not once: opening a fork focuses a frame of its own, which the
// heading read as descent and left the key unannounced.
func TestForkParentRowAndHeadingAgree(t *testing.T) {
	m := forkDetail(true)
	row := false
	for _, r := range m.detailRows {
		if r.Kind == "forkparent" {
			row = true
		}
	}
	heading := strings.Contains(stripANSI(m.detailView()), "(p)")
	if !row || !heading {
		t.Fatalf("a directly opened fork should offer both: row=%v heading(p)=%v", row, heading)
	}
}

// Selecting the row goes to the parent, the same move the p key makes.
func TestForkParentRowOpensTheParent(t *testing.T) {
	m := forkDetail(true)
	m.detailCursor = len(m.detailRows) - 1
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = u.(Model)
	if m.detailSession == nil || m.detailSession.SessionID != "parent" {
		t.Fatalf("Enter on the fork-parent row did not open the parent: session=%v", m.detailSession)
	}
	if len(m.forkBack) != 1 || m.forkBack[0].SessionID != "child" {
		t.Fatalf("the fork was not remembered for going back: %v", m.forkBack)
	}
}

func TestForkBranchOpensOwningSessionWithParentNavigation(t *testing.T) {
	c := forkConversation()
	parent := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "parent", CWD: "/repo", Title: "where it came from"}
	child := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "child", CWD: "/repo", Title: "asked in the fork", ParentSessionID: "parent", ForkAt: "a1"}
	origins := map[string]app.NodeOrigin{
		"u1": {Session: parent, NodeID: "u1"},
		"a1": {Session: parent, NodeID: "a1"},
		"p2": {Session: parent, NodeID: "p2", ActiveLeaf: true},
		"f1": {Session: child, NodeID: "f1"},
		"f2": {Session: child, NodeID: "f2", ActiveLeaf: true},
	}
	m := Model{width: 140, height: 20, detailSession: &parent, sessions: []domain.Session{parent, child}}
	u, _ := m.Update(convMsg{c: &c, origins: origins, reset: true})
	m = u.(Model)
	for i, row := range m.detailRows {
		if row.Kind == "branch" && row.Root == "f1" {
			m.detailCursor = i
			break
		}
	}

	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = u.(Model)
	if cmd != nil || m.detailSession == nil || m.detailSession.SessionID != "child" {
		t.Fatalf("fork Enter reloaded instead of immediately opening the loaded child: session=%v cmd=%v", m.detailSession, cmd)
	}
	view := stripANSI(m.detailView())
	if !strings.Contains(view, "forked from: parent") || !strings.Contains(view, "(p)") {
		t.Fatalf("child opened from the turn list lacks lineage or parent navigation:\n%s", view)
	}
	if got := m.loadedSessionFocusLeaf(parent); got != "p2" {
		t.Fatalf("loaded parent focus leaf=%q want p2; origins=%+v", got, m.detailOrigins)
	}

	u, cmd = m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = u.(Model)
	if cmd != nil || m.detailSession == nil || m.detailSession.SessionID != "parent" {
		t.Fatalf("p reloaded instead of immediately returning to the loaded parent: session=%v cmd=%v", m.detailSession, cmd)
	}
}

// The row is a way to another session, not a branch of this one, so it must not
// be counted among the branches the header reports.
func TestForkParentRowIsNotCountedAsABranch(t *testing.T) {
	m := forkDetail(true)
	want := 0
	for _, r := range m.detailRows {
		if r.Kind == "branch" {
			want++
		}
	}
	lead := stripANSI(m.detailLead(m.detailSession))
	if want == 0 {
		if strings.Contains(lead, "branches:") {
			t.Fatalf("the fork-parent row was counted as a branch: %q", lead)
		}
		return
	}
	if !strings.Contains(lead, fmt.Sprintf("branches:%d", want)) {
		t.Fatalf("header should report the %d branch rows and nothing else: %q", want, lead)
	}
}

// A parent that is not in the list is still named: that a fork came from
// somewhere is worth saying, and selecting the row explains the rest.
func TestForkParentRowNamesAnAbsentParent(t *testing.T) {
	m := forkDetail(false)
	view := stripANSI(m.detailView())
	if !strings.Contains(view, "↳"+shortID("parent")+" forked from (not loaded)") {
		t.Fatalf("an absent parent should still be named:\n%s", view)
	}
}
