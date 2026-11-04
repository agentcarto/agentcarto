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
// that conversation carries on. The row leading there sits under the fork's
// oldest turn — the bottom of a list that runs newest first.
func TestForkParentRowSitsUnderTheOldestTurn(t *testing.T) {
	m := forkDetail(true)
	if len(m.detailPathStack) < 2 || !m.detailPathStack[len(m.detailPathStack)-1].followFocus {
		t.Fatalf("test setup: opening a fork should focus its own frame, stack=%d", len(m.detailPathStack))
	}
	if len(m.detailRows) == 0 {
		t.Fatal("no rows built")
	}
	last := m.detailRows[len(m.detailRows)-1]
	if last.Kind != "origin" {
		t.Fatalf("last row kind=%q want origin (the row belongs under the oldest turn)", last.Kind)
	}
	for _, r := range m.detailRows[:len(m.detailRows)-1] {
		if r.Kind == "origin" {
			t.Fatal("the fork-parent row must appear once, at the bottom")
		}
	}
	// The turn above it is the fork's own first, numbered on from the #1 it was
	// forked after — the number its own log gives it.
	above := m.detailRows[len(m.detailRows)-2]
	if above.Kind != "turn" || above.TurnIndex != 1 {
		t.Fatalf("row above the fork-parent row is %q index=%d, want turn #2", above.Kind, above.TurnIndex)
	}
	view := stripANSI(m.detailView())
	if !strings.Contains(view, "↳"+shortID("parent")+" forked from ask") {
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
		if r.Kind == "origin" {
			t.Fatal("a session with no parent should not offer a row to one")
		}
	}
}

// Descending into another line by hand changes what the list is showing, so the
// row under turn #1 and the p key lead back to the line it was entered from, not
// to this session's parent.
func TestHandEnteredFrameLeadsBackToItsLineNotTheParentSession(t *testing.T) {
	m := forkDetail(true)
	m.detailPathStack = append(m.detailPathStack, detailFrame{path: []string{"p2"}, label: "rewind p2"})
	m.setDetailPath(m.currentDetailPath())
	if got := stripANSI(m.detailView()); strings.Contains(got, "↳"+shortID("parent")+" forked from") {
		t.Fatalf("a hand-entered frame still offers the parent session:\n%s", got)
	}
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = u.(Model)
	if m.detailSession == nil || m.detailSession.SessionID != "child" || len(m.detailPathStack) != 2 {
		t.Fatalf("p left the session instead of the hand-entered frame: session=%v frames=%d", m.detailSession, len(m.detailPathStack))
	}
}

// The heading's (p) and the row are the same offer, so they appear together.
// They did not once: opening a fork focuses a frame of its own, which the
// heading read as descent and left the key unannounced.
func TestForkParentRowAndHeadingAgree(t *testing.T) {
	m := forkDetail(true)
	row := false
	for _, r := range m.detailRows {
		if r.Kind == "origin" {
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

func TestForkNavigationUsesCanonicalConversationParent(t *testing.T) {
	c := forkConversation()
	root := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "root", CWD: "/repo", Title: "shared history"}
	creationParent := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "middle", CWD: "/repo", Title: "first edit", ParentSessionID: "root"}
	reedited := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "child", CWD: "/repo", Title: "second edit", ParentSessionID: "middle"}
	origins := map[string]app.NodeOrigin{
		"u1": {Session: root, NodeID: "u1"},
		"a1": {Session: root, NodeID: "a1"},
		"p2": {Session: root, NodeID: "p2", ActiveLeaf: true},
		"f1": {Session: reedited, NodeID: "f1"},
		"f2": {Session: reedited, NodeID: "f2", ActiveLeaf: true},
	}
	m := Model{
		width:         140,
		height:        20,
		detailSession: &root,
		sessions:      []domain.Session{root, creationParent, reedited},
		sessionParents: map[domain.SessionKey]string{
			creationParent.Key(): "root",
			reedited.Key():       "root",
		},
	}
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
		t.Fatalf("fork Enter did not synchronously open reedited session: session=%v cmd=%v", m.detailSession, cmd)
	}
	view := stripANSI(m.detailView())
	if !strings.Contains(view, "forked from: root") || strings.Contains(view, "forked from: root › middle") {
		t.Fatalf("breadcrumb followed creation ancestry instead of conversation ancestry:\n%s", view)
	}
	if !strings.Contains(view, "↳root forked from ask") {
		t.Fatalf("parent row did not name the conversation parent:\n%s", view)
	}

	u, cmd = m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = u.(Model)
	if cmd != nil || m.detailSession == nil || m.detailSession.SessionID != "root" {
		t.Fatalf("p opened creation parent instead of conversation parent: session=%v cmd=%v", m.detailSession, cmd)
	}
	u, cmd = m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = u.(Model)
	if cmd != nil || m.detailSession == nil || m.detailSession.SessionID != "child" {
		t.Fatalf("q did not synchronously return to reedited session: session=%v cmd=%v", m.detailSession, cmd)
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

func TestBackFromFocusedForkDoesNotExposeAncestorConversation(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyLeft},
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune{'h'}},
	} {
		t.Run(key.String(), func(t *testing.T) {
			m := forkDetail(true)
			u, cmd := m.Update(key)
			m = u.(Model)
			if cmd != nil || m.detailSession != nil || m.detail != nil {
				t.Fatalf("back from a session should return to the list, got session=%v path=%v", m.detailSession, m.currentDetailPath())
			}
		})
	}
}

func TestBackFromManualBranchKeepsFocusedFork(t *testing.T) {
	m := forkDetail(true)
	m.detailPathStack = append(m.detailPathStack, detailFrame{path: []string{"f2"}, label: "rewind"})
	m.setDetailPath(m.currentDetailPath())
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = u.(Model)
	if m.detailSession == nil || m.detailSession.SessionID != "child" || len(m.detailPathStack) != 2 {
		t.Fatalf("manual branch back lost the focused session: session=%v frames=%v", m.detailSession, m.detailPathStack)
	}
	if len(m.detailTurns) != 1 || m.detailTurns[0][0] != "f1" {
		t.Fatalf("back showed another session's conversation: %v", m.detailTurns)
	}
}

func TestForkBranchOrderUsesDescendantsAndKeepsRewindsInPlace(t *testing.T) {
	parent := domain.Session{PluginID: "codex", SessionID: "parent"}
	older := domain.Session{PluginID: "codex", SessionID: "older", ParentSessionID: "parent", UpdatedAt: time.Unix(1, 0)}
	newer := domain.Session{PluginID: "codex", SessionID: "newer", ParentSessionID: "parent", UpdatedAt: time.Unix(2, 0)}
	grandchild := domain.Session{PluginID: "codex", SessionID: "grandchild", ParentSessionID: "older", UpdatedAt: time.Unix(3, 0)}
	c := domain.NewConversation([]domain.ConvNode{{ID: "older"}, {ID: "newer"}})
	c.ForkRoots = []string{"older", "newer"}
	m := Model{detail: &c, detailSession: &parent, sessions: []domain.Session{parent, newer, older, grandchild},
		detailOrigins: map[string]app.NodeOrigin{"older": {Session: older}, "newer": {Session: newer}}}
	roots := []string{"newer", "rewind", "older", "unlisted"}
	orderForkBranches(roots, m.forkBranchRanks())
	if got := strings.Join(roots, ","); got != "older,rewind,newer,unlisted" {
		t.Fatalf("subtree order or rewind positions changed: %s", got)
	}
	// A logical parent can differ from the session that created the fork.
	m.sessionParents = map[domain.SessionKey]string{grandchild.Key(): "newer"}
	orderForkBranches(roots, m.forkBranchRanks())
	if got := strings.Join(roots, ","); got != "newer,rewind,older,unlisted" {
		t.Fatalf("logical session tree was ignored: %s", got)
	}
}

func TestNestedForkOmitsInheritedTurns(t *testing.T) {
	c := forkConversation()
	root := domain.Session{PluginID: "codex", SessionID: "root"}
	child := domain.Session{PluginID: "codex", SessionID: "child", ParentSessionID: "root"}
	grandchild := domain.Session{PluginID: "codex", SessionID: "grandchild", ParentSessionID: "child"}
	c.Nodes["g1"] = domain.ConvNode{ID: "g1", Parent: "f2", Events: []domain.Event{{Kind: domain.EventUser, Prompt: "new question", Text: "new question"}}}
	c.Nodes["g2"] = domain.ConvNode{ID: "g2", Parent: "g1", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "new answer"}}}
	c.Children["f2"] = []string{"g1"}
	c.Children["g1"] = []string{"g2"}
	c.ForkRoots = append(c.ForkRoots, "g1")
	origins := map[string]app.NodeOrigin{
		"u1": {Session: root, NodeID: "u1"},
		"a1": {Session: root, NodeID: "a1"},
		"p2": {Session: root, NodeID: "p2", ActiveLeaf: true},
		"f1": {Session: child, NodeID: "f1"},
		"f2": {Session: child, NodeID: "f2", ActiveLeaf: true},
		"g1": {Session: grandchild, NodeID: "g1"},
		"g2": {Session: grandchild, NodeID: "g2", ActiveLeaf: true},
	}
	m := Model{width: 160, height: 30, detailSession: &grandchild, sessions: []domain.Session{root, child, grandchild}}
	u, _ := m.Update(convMsg{c: &c, origins: origins, focusLeaf: "g2", reset: true})
	m = u.(Model)
	if len(m.detailTurns) != 1 || strings.Join(m.detailTurns[0], ",") != "g1,g2" {
		t.Fatalf("inherited turns were not omitted: %v", m.detailTurns)
	}
	if !m.forkParentRowVisible() {
		t.Fatal("parent history must remain accessible")
	}

	c.Nodes["g3"] = domain.ConvNode{ID: "g3", Parent: "g2", Events: []domain.Event{{Kind: domain.EventUser, Prompt: "follow up", Text: "follow up"}}}
	c.Children["g2"] = []string{"g3"}
	origins["g2"] = app.NodeOrigin{Session: grandchild, NodeID: "g2"}
	origins["g3"] = app.NodeOrigin{Session: grandchild, NodeID: "g3", ActiveLeaf: true}
	u, _ = m.Update(convMsg{c: &c, origins: origins, focusLeaf: "g3"})
	m = u.(Model)
	if len(m.detailTurns) != 2 || m.detailTurns[1][0] != "g1" {
		t.Fatalf("reload restored inherited turns: %v", m.detailTurns)
	}
	u, cmd := m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = u.(Model)
	if cmd != nil || m.detailSession.Key() != child.Key() || len(m.detailTurns) != 1 || m.detailTurns[0][0] != "f1" {
		t.Fatalf("parent navigation lost its own history: session=%v turns=%v", m.detailSession, m.detailTurns)
	}
	u, cmd = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = u.(Model)
	if cmd != nil || m.detailSession.Key() != grandchild.Key() || len(m.detailTurns) != 2 || m.detailTurns[1][0] != "g1" {
		t.Fatalf("return to fork restored inherited turns: session=%v turns=%v", m.detailSession, m.detailTurns)
	}
}

func TestParentJumpHighlightsForkPoint(t *testing.T) {
	c := forkConversation()
	parent := domain.Session{PluginID: "codex", SessionID: "parent"}
	child := domain.Session{PluginID: "codex", SessionID: "child", ParentSessionID: "parent"}
	origins := map[string]app.NodeOrigin{
		"u1": {Session: parent}, "a1": {Session: parent},
		"p2": {Session: parent, ActiveLeaf: true},
		"f1": {Session: child}, "f2": {Session: child, ActiveLeaf: true},
	}
	// Move the fork point far enough back to require scrolling when selected.
	last := "p2"
	for i := 0; i < 15; i++ {
		id := fmt.Sprintf("later%d", i)
		c.Nodes[id] = domain.ConvNode{ID: id, Parent: last, Events: []domain.Event{{Kind: domain.EventUser, Text: id, Prompt: id}}}
		c.Children[last] = []string{id}
		last = id
	}
	c.ActiveLeaf = last
	origins["p2"] = app.NodeOrigin{Session: parent}
	origins[last] = app.NodeOrigin{Session: parent, ActiveLeaf: true}
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async=%v", async), func(t *testing.T) {
			m := Model{width: 140, height: 10, detailSession: &child, sessions: []domain.Session{parent, child}}
			u, _ := m.Update(convMsg{c: &c, origins: origins, focusLeaf: "f2", reset: true})
			m = u.(Model)
			if async {
				m.detailSession = &parent
				m.detail = nil
				m.forkBack = []domain.Session{child}
				u, _ = m.Update(convMsg{c: &c, origins: origins, focusLeaf: last, key: parent.Key(), reset: true})
			} else {
				u, _ = m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
			}
			m = u.(Model)
			row, ok := m.selectedDetailRow()
			if !ok || row.Kind != "branch" || row.Root != "f1" {
				t.Fatalf("parent did not select fork point: %+v", row)
			}
			if m.detailOffset == 0 || m.detailCursor < m.detailOffset || m.detailCursor >= m.detailOffset+m.detailBodyRows() {
				t.Fatalf("fork point is outside viewport: cursor=%d offset=%d", m.detailCursor, m.detailOffset)
			}
			if view := stripANSI(m.detailView()); !strings.Contains(view, "↳child fork [fork point]") {
				t.Fatalf("fork point is not marked: %s", view)
			}
			m.detailCursor--
			if view := stripANSI(m.detailView()); !strings.Contains(view, "↳child fork [fork point]") {
				t.Fatalf("moving cursor removed fork point: %s", view)
			}
			u, _ = m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
			m = u.(Model)
			if m.detailSession == nil || m.detailSession.Key() != child.Key() {
				t.Fatal("back did not return to originating fork")
			}
			if strings.Contains(stripANSI(m.detailView()), "[fork point]") {
				t.Fatal("fork point marker leaked into child")
			}
		})
	}
}

func TestForkParentLabelUsesTurnBeforeFork(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%v", compact), func(t *testing.T) {
			c := forkConversation()
			c.Nodes["u2"] = domain.ConvNode{ID: "u2", Parent: "a1", Events: []domain.Event{{Kind: domain.EventUser, Text: "question immediately before fork", Prompt: "question immediately before fork"}}}
			c.Nodes["a2"] = domain.ConvNode{ID: "a2", Parent: "u2", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer immediately before fork"}}}
			attach := "a2"
			if compact {
				c.Nodes["compact"] = domain.ConvNode{ID: "compact", Parent: attach, Events: []domain.Event{{Kind: domain.EventUser, Text: "internal summary", RawType: domain.RawCompactSummary}}}
				attach = "compact"
			}
			node := c.Nodes["f1"]
			node.Parent = attach
			c.Nodes["f1"] = node
			parent := domain.Session{PluginID: "codex", SessionID: "parent", Title: "first question"}
			child := domain.Session{PluginID: "codex", SessionID: "child", ParentSessionID: "parent"}
			m := Model{detail: &c, detailSession: &child, sessions: []domain.Session{parent, child},
				detailPathStack: []detailFrame{{path: c.ActivePath()}, {path: []string{"f1", "f2"}, followFocus: true}}}
			want := "↳parent forked from question immediately before fork"
			if got := m.forkParentLabel(); got != want {
				t.Fatalf("label=%q want %q", got, want)
			}
		})
	}
}

func TestForkParentLabelDoesNotGuessWithoutForkBoundary(t *testing.T) {
	m := forkDetail(true)
	m.detailPathStack = m.detailPathStack[:1]
	if got := m.forkParentLabel(); got != "↳parent forked from" {
		t.Fatalf("unresolved fork point should not show the session's first prompt: %q", got)
	}
}
