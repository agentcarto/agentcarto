package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func rowKinds(m Model) string {
	var b strings.Builder
	for _, r := range m.rows {
		if r.Kind == "header" {
			b.WriteString("H(" + r.CWD + ")")
		} else {
			b.WriteString("s(" + m.sessions[r.SessIdx].SessionID + ")")
		}
	}
	return b.String()
}

func folderModel() Model {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	return Model{view: "folder", sessions: []domain.Session{
		{PluginID: "codex", SessionID: "a-old", CWD: "/a", UpdatedAt: t0},
		{PluginID: "codex", SessionID: "b-new", CWD: "/b", UpdatedAt: t0.Add(3 * time.Hour)},
		{PluginID: "codex", SessionID: "a-new", CWD: "/a", UpdatedAt: t0.Add(time.Hour)},
	}}
}

// Rows in the folder view consist of a header plus the sessions of an expanded group;
// groups are ordered by most-recent time, and sessions within a group are ordered newest-first.
func TestFolderRowsHeaderThenSessions(t *testing.T) {
	m := folderModel()
	m.filter()
	got := rowKinds(m)
	want := "H(/b)s(b-new)H(/a)s(a-new)s(a-old)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Groups tied on most-recent time are ordered by first appearance, not alphabetically.
func TestFolderTieBreakIsFirstSeenNotAlphabetical(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "folder", sessions: []domain.Session{
		{PluginID: "codex", SessionID: "b", CWD: "/b", UpdatedAt: t0}, // /b appears first
		{PluginID: "codex", SessionID: "a", CWD: "/a", UpdatedAt: t0}, // tied
	}}
	m.filter()
	if got := rowKinds(m); got != "H(/b)s(b)H(/a)s(a)" {
		t.Fatalf("first-seen order broken: got %q", got)
	}
}

// Pressing Enter on a header row collapses that group, and its session rows disappear.
func TestFolderEnterTogglesCollapse(t *testing.T) {
	m := folderModel()
	m.filter()
	// The cursor starts at the top = header /b. Enter collapses /b.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if m.detailSession != nil {
		t.Fatal("header Enter must not open detail")
	}
	if got := rowKinds(m); got != "H(/b)H(/a)s(a-new)s(a-old)" {
		t.Fatalf("after collapse got %q", got)
	}
	if !m.rows[0].Collapsed {
		t.Fatal("header should report collapsed")
	}
	// Enter again expands it.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if got := rowKinds(m); got != "H(/b)s(b-new)H(/a)s(a-new)s(a-old)" {
		t.Fatalf("after expand got %q", got)
	}
}

// H collapses all, L expands all.
func TestFolderCollapseAllExpandAll(t *testing.T) {
	m := folderModel()
	m.filter()
	updated, _ := m.Update(key("H"))
	m = updated.(Model)
	if got := rowKinds(m); got != "H(/b)H(/a)" {
		t.Fatalf("collapse-all got %q", got)
	}
	updated, _ = m.Update(key("L"))
	m = updated.(Model)
	if got := rowKinds(m); got != "H(/b)s(b-new)H(/a)s(a-new)s(a-old)" {
		t.Fatalf("expand-all got %q", got)
	}
}

// The cursor moves across both headers and sessions.
func TestFolderCursorCrossesHeaders(t *testing.T) {
	m := folderModel()
	m.filter()
	// 0:H(/b) 1:s(b-new) 2:H(/a) 3:s(a-new) 4:s(a-old)
	for i := 0; i < 2; i++ {
		updated, _ := m.Update(key("j"))
		m = updated.(Model)
	}
	if m.cursor != 2 || m.rows[m.cursor].Kind != "header" {
		t.Fatalf("cursor=%d kind=%s, want header at 2", m.cursor, m.rows[m.cursor].Kind)
	}
	// On a header, selected() (a session) is unavailable, but selectedCWD is available.
	if _, ok := m.selected(); ok {
		t.Fatal("selected() must be false on header")
	}
	if cwd, ok := m.selectedCWD(); !ok || cwd != "/a" {
		t.Fatalf("selectedCWD=%q ok=%v, want /a", cwd, ok)
	}
	updated, _ := m.Update(key("j"))
	m = updated.(Model)
	s, ok := m.selected()
	if !ok || s.SessionID != "a-new" {
		t.Fatalf("after j on session: ok=%v id=%s", ok, s.SessionID)
	}
}

// A selected folder header fills the whole row with the selection background (same as a session row).
// Without this, even when the cursor is on it the selection is not visible, making folder rows look unselectable.
func TestFolderHeaderSelectionSpansFullWidth(t *testing.T) {
	m := folderModel()
	m.width, m.height = 60, 20
	m.filter() // cursor=0 = top header (selected)
	out := m.View()
	// line 0=head, line 1=search/status line (always reserved), line 2=first body line = selected header.
	hdr := strings.Split(out, "\n")[2]
	if w := lipgloss.Width(hdr); w != m.width-1 {
		t.Fatalf("selected header width=%d, want %d (full-width highlight)", w, m.width-1)
	}
	// An unselected header (cursor moved to another row) is not full-width filled.
	updated, _ := m.Update(key("j")) // to a session
	m = updated.(Model)
	updated, _ = m.Update(key("j")) // to the next header
	m = updated.(Model)
	out = m.View()
	first := strings.Split(out, "\n")[2] // the top header (line 2) is now unselected
	if w := lipgloss.Width(first); w == m.width-1 {
		t.Fatalf("unselected header should not be padded full-width, got %d", w)
	}
}

// Collapse state is preserved across view switches and is reflected when returning to folder.
func TestFolderCollapsePersistsAcrossViewSwitch(t *testing.T) {
	m := folderModel()
	m.filter()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // collapse /b
	m = updated.(Model)
	updated, _ = m.Update(key("v")) // to time
	m = updated.(Model)
	if m.view != "time" {
		t.Fatalf("view=%s", m.view)
	}
	updated, _ = m.Update(key("v")) // back to folder
	m = updated.(Model)
	if got := rowKinds(m); got != "H(/b)H(/a)s(a-new)s(a-old)" {
		t.Fatalf("collapse not persisted: got %q", got)
	}
}
