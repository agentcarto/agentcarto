package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
)

// relocate must not modify the search query or the list state (m.rows/m.filtered) at all.
// If this breaks, returning from relocate while filtered leaves the stale (small) filtered result in place,
// which does not revert to the full set until the next rescan (the "few entries -> full list a few seconds later" bug).
func TestRelocateDoesNotDisturbSearchState(t *testing.T) {
	m := Model{view: "time"}
	m.query = textinput.New()
	m.relocInput = textinput.New()
	m.query.SetValue("myquery") // simulate an active search filter
	m.filtered = []int{2, 5}
	m.rows = []listRow{{Kind: "session"}, {Kind: "session"}}

	// Start relocate (set up the state as the "m" key would) -> input -> finish.
	m.action = "relocate"
	m.relocOld = "/old"
	m.relocInput.SetValue("/old/path")
	m = m.endRelocate()

	if m.query.Value() != "myquery" {
		t.Fatalf("search query must survive relocate, got %q", m.query.Value())
	}
	if len(m.rows) != 2 || len(m.filtered) != 2 {
		t.Fatalf("list state must be untouched, rows=%d filtered=%d", len(m.rows), len(m.filtered))
	}
	if m.action != "" {
		t.Fatal("action must be cleared after relocate")
	}
	if m.relocInput.Value() != "" {
		t.Fatalf("relocInput must reset, got %q", m.relocInput.Value())
	}
}

func TestNormalizeRelocPath(t *testing.T) {
	cases := map[string]string{
		"  /a/b/  ": "/a/b",
		"/a/b/":     "/a/b",
		"/":         "/",
		" / ":       "/",
		"/a":        "/a",
		"":          "",
	}
	for in, want := range cases {
		if got := normalizeRelocPath(in); got != want {
			t.Errorf("normalizeRelocPath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestValidateRelocPath(t *testing.T) {
	d := t.TempDir()
	// not an absolute path
	if _, ok, _ := validateRelocPath("relative", "/old"); ok {
		t.Error("relative path should be rejected")
	}
	if _, ok, _ := validateRelocPath("", "/old"); ok {
		t.Error("empty should be rejected")
	}
	// same as the current cwd -> cancel
	if _, ok, cancel := validateRelocPath(d, d); ok || !cancel {
		t.Error("same path should cancel")
	}
	// does not exist
	if _, ok, _ := validateRelocPath(filepath.Join(d, "nope"), "/old"); ok {
		t.Error("missing dir should be rejected")
	}
	// existing directory -> OK
	if _, ok, _ := validateRelocPath(d, "/old"); !ok {
		t.Error("existing dir should pass")
	}
	// a file (not a directory) -> rejected
	f := filepath.Join(d, "file")
	_ = os.WriteFile(f, []byte("x"), 0600)
	if _, ok, _ := validateRelocPath(f, "/old"); ok {
		t.Error("file should be rejected")
	}
}

func TestCommonPrefix(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"daily", "dairy"}, "dai"},
		{[]string{"abc"}, "abc"},
		{[]string{"abc", "xyz"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := commonPrefix(c.in); got != c.want {
			t.Errorf("commonPrefix(%v)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestCandidateGrid(t *testing.T) {
	// Each is width 2 -> cellW=4. width=8 gives 2 columns; 5 candidates -> 3 rows. Filled column-major.
	names := []string{"aa", "bb", "cc", "dd", "ee"}
	rows, cellW := candidateGrid(names, 8)
	if cellW != 4 {
		t.Fatalf("cellW=%d want 4", cellW)
	}
	want := [][]int{{0, 3}, {1, 4}, {2, -1}}
	if len(rows) != len(want) {
		t.Fatalf("rows=%v want %v", rows, want)
	}
	for r := range want {
		for c := range want[r] {
			if rows[r][c] != want[r][c] {
				t.Fatalf("rows=%v want %v", rows, want)
			}
		}
	}
	// Even with a narrow width, at least 1 column is guaranteed.
	if r, _ := candidateGrid(names, 1); len(r) != len(names) || len(r[0]) != 1 {
		t.Fatalf("narrow width should give single column, got %v", r)
	}
	// If there are fewer candidates than columns, the column count is clamped to the candidate count (1 row).
	if r, _ := candidateGrid([]string{"x", "y"}, 200); len(r) != 1 || len(r[0]) != 2 {
		t.Fatalf("few candidates should fit one row, got %v", r)
	}
}

func TestDirCandidatesAndComplete(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"daily", "dairy", "docs"} {
		_ = os.MkdirAll(filepath.Join(d, n), 0700)
	}
	_ = os.WriteFile(filepath.Join(d, "dafile"), []byte("x"), 0600) // not a directory -> not a candidate

	// basename "da" -> directory-only candidates.
	target, base, names := dirCandidates(filepath.Join(d, "da"))
	if target != d || base != "da" {
		t.Fatalf("target=%q base=%q", target, base)
	}
	if len(names) != 2 || names[0] != "daily" || names[1] != "dairy" {
		t.Fatalf("names=%v (file should be excluded, sorted)", names)
	}

	// Multiple candidates -> extend to the common prefix.
	completed, ns := completePath(filepath.Join(d, "da"))
	if completed != filepath.Join(d, "dai") || len(ns) != 2 {
		t.Fatalf("completed=%q names=%v", completed, ns)
	}

	// Unique candidate -> append a trailing slash.
	completed1, ns1 := completePath(filepath.Join(d, "do"))
	if completed1 != filepath.Join(d, "docs")+"/" || len(ns1) != 1 {
		t.Fatalf("completed1=%q names=%v", completed1, ns1)
	}

	// No candidates -> input unchanged.
	completed0, ns0 := completePath(filepath.Join(d, "zzz"))
	if completed0 != filepath.Join(d, "zzz") || ns0 != nil {
		t.Fatalf("completed0=%q names=%v", completed0, ns0)
	}
}
