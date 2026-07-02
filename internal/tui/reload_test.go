package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentcarto/core/domain"
)

// On a reload while the conversation grows during processing, the new nodes must not be shown as a
// phantom rewind branch off the current branch (the current branch should follow the new active path).
func TestReloadFollowsGrowingActivePathNoPhantomBranch(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	// Conversation A at open time: user -> assistant
	nodesA := []domain.ConvNode{
		{ID: "u", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "request"}}},
		{ID: "a1", Parent: "u", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "work started"}}},
	}
	a := domain.NewConversation(nodesA)
	sess := domain.Session{PluginID: "claude", SessionID: "s"}
	m := Model{detail: &a, detailSession: &sess, detailPathStack: []detailFrame{{path: a.ActivePath(), label: "current"}}}
	m.setDetailPath(m.currentDetailPath())

	// The same turn grows during processing: 6 tool-call/result nodes are appended after a1 (making it substantial).
	nodesB := append([]domain.ConvNode(nil), nodesA...)
	prev := "a1"
	for i := 0; i < 6; i++ {
		id := "g" + string(rune('0'+i))
		k := domain.EventToolCall
		if i%2 == 1 {
			k = domain.EventToolResult
		}
		nodesB = append(nodesB, domain.ConvNode{ID: id, Parent: prev, Timestamp: ts(int64(3 + i)), Events: []domain.Event{{Kind: k}}})
		prev = id
	}
	b := domain.NewConversation(nodesB)

	// Reload via rescan (reset=false).
	nm, _ := m.Update(convMsg{c: &b, reset: false})
	m2 := nm.(Model)

	branches := 0
	for _, r := range m2.detailRows {
		if r.Kind == "branch" {
			branches++
		}
	}
	if branches != 0 {
		t.Fatalf("growth during processing was mis-rendered as a branch: branch rows=%d, active leaf=%s", branches, m2.detail.ActiveLeaf)
	}
	// The new nodes should follow onto the current branch.
	if got := m2.currentDetailPath(); len(got) != 8 {
		t.Fatalf("current branch did not follow the new active path: pathlen=%d want 8", len(got))
	}
}

// Drilling into another line shows a breadcrumb (current > branch) on the second header line.
func TestDetailHeaderShowsBreadcrumbWhenDrilledIntoBranch(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "question"}}},
		{ID: "a", Parent: "u", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "current"}}},
		{ID: "b", Parent: "u", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventUser, Text: "alternative"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s", CWD: "/repo", Title: "t"}
	m := Model{width: 120, height: 20, detailSession: &s}
	upd, _ := m.Update(convMsg{c: &c, reset: true})
	m = upd.(Model)
	out := stripANSI(m.detailView())
	if strings.Contains(out, "▸ current") {
		t.Fatalf("breadcrumb appears before drilling in:\n%s", out)
	}
	// Move to the branch row and drill in (Enter).
	for !rowIsBranch(m) && m.detailCursor < len(m.detailRows)-1 {
		u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = u.(Model)
	}
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = u.(Model)
	out = stripANSI(m.detailView())
	// The drilled segment shows the kind plus the branch root's short ID (an ID, not a fixed "branch").
	if !strings.Contains(out, "▸ current › rewind b") {
		t.Fatalf("ID-form breadcrumb not shown after drilling in:\n%s", out)
	}
}

// When a fork is opened directly, the header shows the lineage route from root -> ... -> nearest parent (multi-level forks are traced too).
func TestDetailHeaderShowsForkLineageRoute(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "question"}}},
		{ID: "a", Parent: "u", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "response"}}},
	})
	// Lineage: rootaaaa <- midbbbb <- selccccc (open selccccc).
	root := domain.Session{PluginID: "claude", SessionID: "rootaaaa11"}
	mid := domain.Session{PluginID: "claude", SessionID: "midbbbb22", ParentSessionID: "rootaaaa11"}
	sel := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "selccccc33", CWD: "/repo", Title: "t", ParentSessionID: "midbbbb22"}
	m := Model{width: 160, height: 20, detailSession: &sel, sessions: []domain.Session{root, mid, sel}}
	upd, _ := m.Update(convMsg{c: &c, reset: true})
	m = upd.(Model)
	out := stripANSI(m.detailView())
	// Ordered root -> nearest parent (self is excluded since it appears in the ID shown above).
	if !strings.Contains(out, "forked from: rootaaaa › midbbbb2 (p)") {
		t.Fatalf("fork lineage route not shown:\n%s", out)
	}
}

// Drilling into a fork branch uses the same "forked from: <lineage>" form as opening one directly
// (it must not become "current › fork …"). The drilled fork's parent is the current session, so the current session ID is shown.
func TestDetailHeaderUnifiesForkDrilldownToForkedFrom(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "question"}}},
		{ID: "a", Parent: "u", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "current"}}},
		{ID: "fk", Parent: "u", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventUser, Text: "fork side"}}},
	})
	c.ForkRoots = []string{"fk"}
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "sessabcd99", CWD: "/repo", Title: "t"}
	m := Model{width: 140, height: 20, detailSession: &s}
	upd, _ := m.Update(convMsg{c: &c, reset: true})
	m = upd.(Model)
	for !rowIsBranch(m) && m.detailCursor < len(m.detailRows)-1 {
		u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = u.(Model)
	}
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = u.(Model)
	out := stripANSI(m.detailView())
	if !strings.Contains(out, "forked from: sessabcd") {
		t.Fatalf("forked-from form not shown when drilling into a fork:\n%s", out)
	}
	if strings.Contains(out, "current › fork") {
		t.Fatalf("current › fork still remains after drilling into a fork:\n%s", out)
	}
}

func rowIsBranch(m Model) bool {
	r, ok := m.selectedDetailRow()
	return ok && r.Kind == "branch"
}

// While drilled into a branch (stack > 1), a reload must not silently rewrite the current branch.
func TestReloadKeepsBranchNavigationWhenDrilledIn(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	a := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "x"}}},
		{ID: "a1", Parent: "u", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventAssistant}}},
	})
	sess := domain.Session{PluginID: "claude", SessionID: "s"}
	drilled := []string{"u", "branchnode"}
	m := Model{detail: &a, detailSession: &sess, detailPathStack: []detailFrame{{path: a.ActivePath(), label: "current"}, {path: drilled, label: "branch"}}}
	nm, _ := m.Update(convMsg{c: &a, reset: false})
	m2 := nm.(Model)
	top := m2.detailPathStack[len(m2.detailPathStack)-1].path
	if len(top) != 2 || top[1] != "branchnode" {
		t.Fatalf("drilled-in navigation was lost: top=%v", top)
	}
}

// A load result tagged with a different session key is stale (the view moved
// to another session while the load ran) and must not be applied: quickly
// closing A and opening B used to show A's conversation under B's header.
func TestStaleConvMsgForAnotherSessionIsDropped(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	stale := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "A's content"}}},
	})
	b := domain.Session{PluginID: "claude", SessionID: "B"}
	m := Model{detailSession: &b}
	nm, _ := m.Update(convMsg{c: &stale, key: domain.SessionKey{PluginID: "claude", SessionID: "A"}, reset: true})
	if got := nm.(Model).detail; got != nil {
		t.Fatal("stale conversation for session A was applied while viewing B")
	}
	// The matching key is applied normally.
	nm, _ = m.Update(convMsg{c: &stale, key: b.Key(), reset: true})
	if nm.(Model).detail == nil {
		t.Fatal("matching conversation was not applied")
	}
}
