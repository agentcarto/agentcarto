package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

// Opening a fork drills initially into the opened fork branch (focusLeaf) within the tree rooted at
// the ancestor, and the breadcrumb points to that fork branch. This is the core of conversation-view
// canonicalization: no matter how it is opened, the same tree of parent -> ... -> current is shown.
func TestConvMsgFocusesForkBranch(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "question", Prompt: "question"}}},
		{ID: "a", Parent: "u", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "main-line continuation"}}},
		{ID: "b", Parent: "u", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventUser, Text: "fork-side continuation", Prompt: "fork-side continuation"}}},
	})
	c.ForkRoots = []string{"b"} // make b a fork branch (the active main line is the newest u->a)
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "sessabcd99", CWD: "/repo", Title: "t", ParentSessionID: "parent01"}
	m := Model{width: 140, height: 20, detailSession: &s}
	upd, _ := m.Update(convMsg{c: &c, focusLeaf: "b", reset: true})
	m = upd.(Model)

	// focusLeaf=b drills initially into the fork branch (two levels: current + fork branch).
	if len(m.detailPathStack) != 2 {
		t.Fatalf("did not drill into fork branch initially: stack depth=%d", len(m.detailPathStack))
	}
	out := stripANSI(m.detailView())
	// Having drilled into the fork branch, the top label shows the lineage from the parent (forked from: parent > ... > self).
	if !strings.Contains(out, "forked from: parent01") {
		t.Fatalf("parent-rooted lineage label not shown:\n%s", out)
	}
	// "main" was removed, so it must not appear.
	if strings.Contains(out, " main ") || strings.Contains(out, "› main") {
		t.Fatalf("removed \"main\" still appears in the breadcrumb:\n%s", out)
	}
}
