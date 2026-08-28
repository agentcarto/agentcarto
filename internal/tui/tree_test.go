package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

// rowTree represents each row as "H(cwd)" / "[<prefix><sid>]" (for verifying tree prefixes).
func rowTree(m Model) string {
	var b strings.Builder
	for _, r := range m.rows {
		if r.Kind == "header" {
			b.WriteString("H(" + r.CWD + ")")
		} else {
			b.WriteString("[" + r.TreePrefix + m.sessions[r.SessIdx].SessionID + "]")
		}
	}
	return b.String()
}

// time view: fork children nest directly under their parent, and siblings are ordered by update time (newest->oldest). The last child uses └─.
func TestTreeTimeViewNestsForksNewestFirst(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "time", sessions: []domain.Session{
		{PluginID: "claude", SessionID: "P", UpdatedAt: t0.Add(2 * time.Hour)},
		{PluginID: "claude", SessionID: "fA", UpdatedAt: t0.Add(3 * time.Hour), ParentSessionID: "P"}, // newer
		{PluginID: "claude", SessionID: "fB", UpdatedAt: t0.Add(time.Hour), ParentSessionID: "P"},     // older
	}}
	m.filter()
	if got, want := rowTree(m), "[P][├─ fA][└─ fB]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestTreeUsesCanonicalConversationParent(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	root := domain.Session{PluginID: "codex", SessionID: "P", UpdatedAt: t0.Add(3 * time.Hour)}
	first := domain.Session{PluginID: "codex", SessionID: "A", UpdatedAt: t0.Add(2 * time.Hour), ParentSessionID: "P"}
	reedited := domain.Session{PluginID: "codex", SessionID: "B", UpdatedAt: t0.Add(time.Hour), ParentSessionID: "A"}
	m := Model{
		view:     "time",
		sessions: []domain.Session{root, first, reedited},
		sessionParents: map[domain.SessionKey]string{
			first.Key():    "P",
			reedited.Key(): "P",
		},
	}
	m.filter()
	if got, want := rowTree(m), "[P][├─ A][└─ B]"; got != want {
		t.Fatalf("logical siblings rendered as a creation chain: got %q want %q", got, want)
	}
}

// Multi-level forks: descendants of a non-last child get the ancestor continuation "│  ". Pre-order: P -> C1 -> G -> C2.
func TestTreeMultiLevelContinuation(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "time", sessions: []domain.Session{
		{PluginID: "claude", SessionID: "P", UpdatedAt: t0.Add(10 * time.Hour)},
		{PluginID: "claude", SessionID: "C1", UpdatedAt: t0.Add(9 * time.Hour), ParentSessionID: "P"}, // non-last child
		{PluginID: "claude", SessionID: "C2", UpdatedAt: t0.Add(8 * time.Hour), ParentSessionID: "P"}, // last child
		{PluginID: "claude", SessionID: "G", UpdatedAt: t0.Add(7 * time.Hour), ParentSessionID: "C1"},
	}}
	m.filter()
	if got, want := rowTree(m), "[P][├─ C1][│  └─ G][└─ C2]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Deep fork chains keep their logical hierarchy, but their visual indentation is
// capped at two levels. Rows beyond the cap identify their immediate parent.
func TestTreeDeepChainUsesBoundedLineage(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "time", sessions: []domain.Session{
		{PluginID: "codex", SessionID: "P", UpdatedAt: t0.Add(5 * time.Hour)},
		{PluginID: "codex", SessionID: "child-01", UpdatedAt: t0.Add(4 * time.Hour), ParentSessionID: "P"},
		{PluginID: "codex", SessionID: "child-02", UpdatedAt: t0.Add(3 * time.Hour), ParentSessionID: "child-01"},
		{PluginID: "codex", SessionID: "child-03", UpdatedAt: t0.Add(2 * time.Hour), ParentSessionID: "child-02"},
		{PluginID: "codex", SessionID: "child-04", UpdatedAt: t0.Add(time.Hour), ParentSessionID: "child-03"},
	}}
	m.filter()
	if got, want := rowTree(m), "[P][└─ child-01][   └─ child-02][   ↳child-02 child-03][   ↳child-03 child-04]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Deep siblings share the bounded display depth while retaining their immediate
// parent and the continuation line of a shallower, non-last branch.
func TestTreeDeepBranchesKeepDFSOrderAndLineage(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "time", sessions: []domain.Session{
		{PluginID: "codex", SessionID: "P", UpdatedAt: t0.Add(10 * time.Hour)},
		{PluginID: "codex", SessionID: "A", UpdatedAt: t0.Add(9 * time.Hour), ParentSessionID: "P"},
		{PluginID: "codex", SessionID: "X", UpdatedAt: t0.Add(time.Hour), ParentSessionID: "P"},
		{PluginID: "codex", SessionID: "B", UpdatedAt: t0.Add(8 * time.Hour), ParentSessionID: "A"},
		{PluginID: "codex", SessionID: "C1", UpdatedAt: t0.Add(7 * time.Hour), ParentSessionID: "B"},
		{PluginID: "codex", SessionID: "C2", UpdatedAt: t0.Add(6 * time.Hour), ParentSessionID: "B"},
	}}
	m.filter()
	if got, want := rowTree(m), "[P][├─ A][│  └─ B][│  ↳B C1][│  ↳B C2][└─ X]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	wantDepths := []int{0, 1, 2, 3, 3, 1}
	for i, row := range m.rows {
		if row.Depth != wantDepths[i] {
			t.Fatalf("row %d depth = %d, want %d", i, row.Depth, wantDepths[i])
		}
	}
}

// A child whose parent is present but filtered out becomes a root and uses the
// existing lineage marker instead of a tree connector.
func TestTreeFilteredParentUsesRootLineageMarker(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "time", activeOnly: true, width: 100, sessions: []domain.Session{
		{PluginID: "codex", AgentType: "codex", SessionID: "parent", UpdatedAt: t0.Add(time.Hour)},
		{PluginID: "codex", AgentType: "codex", SessionID: "child", UpdatedAt: t0, ParentSessionID: "parent", Status: domain.StatusRunning},
	}}
	m.filter()
	if got, want := rowTree(m), "[child]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	row := m.rows[0]
	rendered := m.sessionRow(m.sessions[row.SessIdx], false, 0, row.TreePrefix)
	if !strings.Contains(rendered, "↳parent ") || strings.Contains(rendered, "├─") || strings.Contains(rendered, "└─") {
		t.Fatalf("filtered-parent root should show lineage marker, not connector: %q", rendered)
	}
}

// folder view: within the same CWD group, fork children nest under their parent.
func TestTreeFolderNestsWithinGroup(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "folder", sessions: []domain.Session{
		{PluginID: "claude", SessionID: "P", CWD: "/a", UpdatedAt: t0.Add(2 * time.Hour)},
		{PluginID: "claude", SessionID: "f", CWD: "/a", UpdatedAt: t0.Add(time.Hour), ParentSessionID: "P"},
	}}
	m.filter()
	if got, want := rowTree(m), "H(/a)[P][└─ f]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// folder view: a fork whose parent is in a different CWD (a different worktree, etc.) is treated as a root within this group.
func TestTreeForkInDifferentCWDIsRoot(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "folder", sessions: []domain.Session{
		{PluginID: "claude", SessionID: "P", CWD: "/a", UpdatedAt: t0.Add(2 * time.Hour)},
		{PluginID: "claude", SessionID: "f", CWD: "/b", UpdatedAt: t0.Add(time.Hour), ParentSessionID: "P"},
	}}
	m.filter()
	// The /a group is most recent (P is newer) -> first. f is the root of the /b group (no prefix).
	if got, want := rowTree(m), "H(/a)[P]H(/b)[f]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// A fork whose parent is not present in the list is treated as a root, with no prefix.
func TestTreeParentNotInListIsRoot(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "time", sessions: []domain.Session{
		{PluginID: "claude", SessionID: "orphan", UpdatedAt: t0, ParentSessionID: "gone"},
	}}
	m.filter()
	if got, want := rowTree(m), "[orphan]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Sorting is keyed on the latest within a tree: even if the parent itself is old, a tree with a newer fork child comes first.
// Each row's displayed time stays its own session's time (only the ordering is determined by the subtree's latest).
func TestTreeSortBySubtreeLatest(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "time", sessions: []domain.Session{
		{PluginID: "claude", SessionID: "P", UpdatedAt: t0.Add(time.Hour)},                           // parent is old
		{PluginID: "claude", SessionID: "C", UpdatedAt: t0.Add(5 * time.Hour), ParentSessionID: "P"}, // child is newest
		{PluginID: "claude", SessionID: "X", UpdatedAt: t0.Add(3 * time.Hour)},                       // independent, middle
	}}
	m.filter()
	// P's subtree latest = C(t5) > X(t3), so the P tree is above X.
	if got, want := rowTree(m), "[P][└─ C][X]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Siblings are also ordered by subtree latest: a branch with a newer grandchild comes first even if it is itself old.
func TestTreeSiblingSortBySubtreeLatest(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "time", sessions: []domain.Session{
		{PluginID: "claude", SessionID: "P", UpdatedAt: t0.Add(10 * time.Hour)},
		{PluginID: "claude", SessionID: "C1", UpdatedAt: t0.Add(time.Hour), ParentSessionID: "P"}, // itself is old
		{PluginID: "claude", SessionID: "C2", UpdatedAt: t0.Add(5 * time.Hour), ParentSessionID: "P"},
		{PluginID: "claude", SessionID: "G", UpdatedAt: t0.Add(9 * time.Hour), ParentSessionID: "C1"}, // C1's grandchild is newer
	}}
	m.filter()
	// C1's subtree latest = G(t9) > C2(t5), so the C1 branch is above.
	if got, want := rowTree(m), "[P][├─ C1][│  └─ G][└─ C2]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Rendering: nested rows have a tree connector and do not show the ↳ lineage marker. A root row
// (whose parent is outside the list) shows the ↳ marker instead.
func TestTreeRowRenderMarkerGating(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "time", width: 100, sessions: []domain.Session{
		{PluginID: "claude", AgentType: "claude", SessionID: "P", UpdatedAt: t0.Add(2 * time.Hour), Title: "parent"},
		{PluginID: "claude", AgentType: "claude", SessionID: "child", UpdatedAt: t0.Add(time.Hour), ParentSessionID: "P", Title: "fork"},
		{PluginID: "claude", AgentType: "claude", SessionID: "orphan", UpdatedAt: t0, ParentSessionID: "gone", Title: "root-fork"},
	}}
	m.filter()
	var childRow, orphanRow string
	for _, r := range m.rows {
		switch m.sessions[r.SessIdx].SessionID {
		case "child":
			childRow = m.sessionRow(m.sessions[r.SessIdx], false, 0, r.TreePrefix)
		case "orphan":
			orphanRow = m.sessionRow(m.sessions[r.SessIdx], false, 0, r.TreePrefix)
		}
	}
	if !strings.Contains(childRow, "└─") || strings.Contains(childRow, "↳") {
		t.Fatalf("nested child should show connector, not ↳ marker: %q", childRow)
	}
	if !strings.Contains(orphanRow, "↳") || strings.Contains(orphanRow, "├─") || strings.Contains(orphanRow, "└─") {
		t.Fatalf("root fork should show ↳ marker, not connector: %q", orphanRow)
	}
}
