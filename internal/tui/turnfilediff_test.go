package tui

import (
	"strings"
	"testing"

	"github.com/agentcarto/core/domain"
)

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func editEvent(kind domain.EventKind, changes ...domain.FileChange) domain.Event {
	return domain.Event{Kind: kind, ToolName: "Edit", Changes: changes}
}

func TestTurnFileEditsFromChanges(t *testing.T) {
	e := editEvent(domain.EventToolCall, domain.FileChange{Path: "/x/foo.go", Op: "update", Added: 1, Removed: 1, Diff: "@@\n a\n-b\n+c"})
	fes := turnFileEdits([]domain.Event{e})
	if len(fes) != 1 {
		t.Fatalf("want 1 file, got %d", len(fes))
	}
	fe := fes[0]
	if fe.Path != "/x/foo.go" || fe.Added != 1 || fe.Removed != 1 || fe.op() != "M" {
		t.Fatalf("fileEdit = %+v", fe)
	}
	if !hasLine(fe.Diff, "-b") || !hasLine(fe.Diff, "+c") {
		t.Fatalf("diff body missing -b/+c: %v", fe.Diff)
	}
}

func TestTurnFileEditsSameFileMerged(t *testing.T) {
	e1 := editEvent(domain.EventToolCall, domain.FileChange{Path: "/x/foo.go", Added: 1, Removed: 1, Diff: "@@\n-a\n+b"})
	e2 := editEvent(domain.EventToolCall, domain.FileChange{Path: "/x/foo.go", Added: 1, Removed: 1, Diff: "@@\n-c\n+d"})
	fes := turnFileEdits([]domain.Event{e1, e2})
	if len(fes) != 1 {
		t.Fatalf("same file should merge into 1, got %d", len(fes))
	}
	if fes[0].Added != 2 || fes[0].Removed != 2 {
		t.Fatalf("merged counts +%d -%d, want +2 -2", fes[0].Added, fes[0].Removed)
	}
	if strings.Count(strings.Join(fes[0].Diff, "\n"), "@@") != 2 {
		t.Fatalf("want 2 hunks: %v", fes[0].Diff)
	}
}

func TestTurnFileEditsDiffLessChangeIsBodyLess(t *testing.T) {
	e := editEvent(domain.EventFileChange, domain.FileChange{Path: "/y/bar.go", Added: 3, Removed: 1})
	fes := turnFileEdits([]domain.Event{e})
	if len(fes) != 1 || !fes[0].noBody || fes[0].Added != 3 || fes[0].Removed != 1 {
		t.Fatalf("diff-less change = %+v", fes)
	}
	if !hasLine(fes[0].Diff, "(no diff body)") {
		t.Fatalf("want no-body note: %v", fes[0].Diff)
	}
}

// An apply_patch tool_call and its patch_apply_end file_change describe the same
// change; the applied result supersedes the request, so no doubled entries,
// hunks or counts.
func TestTurnFileEditsAppliedSupersedesRequested(t *testing.T) {
	fc := domain.FileChange{Path: "a.go", Added: 1, Removed: 1, Diff: "+new\n-old"}
	events := []domain.Event{editEvent(domain.EventToolCall, fc), editEvent(domain.EventFileChange, fc)}
	fes := turnFileEdits(events)
	if len(fes) != 1 {
		t.Fatalf("want 1 deduped file, got %d: %+v", len(fes), fes)
	}
	if fes[0].Added != 1 || fes[0].Removed != 1 {
		t.Fatalf("dedup counts +%d -%d, want +1 -1", fes[0].Added, fes[0].Removed)
	}
	if n := strings.Count(strings.Join(fes[0].Diff, "\n"), "+new"); n != 1 {
		t.Fatalf("diff duplicated (+new x%d): %v", n, fes[0].Diff)
	}
}

func TestEditStatsNoDoubleCountWithFileChange(t *testing.T) {
	fc := domain.FileChange{Path: "a.go", Added: 1, Removed: 1, Diff: "+new\n-old"}
	events := []domain.Event{editEvent(domain.EventToolCall, fc), editEvent(domain.EventFileChange, fc)}
	files, added, removed := editStats(events)
	if files != 1 || added != 1 || removed != 1 {
		t.Fatalf("editStats = files%d +%d -%d, want 1 +1 -1", files, added, removed)
	}
}

// The block label shows the op letter and path. Bare "@@" markers carry no
// information and become blank-line hunk separators (none at the edges);
// "@@ <context>" markers are kept.
func TestFileEditOpAndBody(t *testing.T) {
	cases := []struct {
		fe   fileEdit
		op   string
		body []string
	}{
		{fileEdit{Op: "update", Diff: []string{"@@", "+x"}}, "M", []string{"+x"}},
		{fileEdit{Op: "add", Diff: []string{"+y"}}, "A", []string{"+y"}},
		{fileEdit{Op: "delete"}, "D", nil},
		{fileEdit{}, "M", nil},
		{
			fileEdit{Diff: []string{"@@", " a", "-b", "+c", "@@", " d", "+e", "@@"}},
			"M",
			[]string{" a", "-b", "+c", "", " d", "+e"},
		},
		{
			fileEdit{Diff: []string{"@@ func main", "+x"}},
			"M",
			[]string{"@@ func main", "+x"},
		},
	}
	for _, c := range cases {
		if got := c.fe.op(); got != c.op {
			t.Errorf("op(%+v) = %q, want %q", c.fe, got, c.op)
		}
		if got := c.fe.body(); strings.Join(got, "\n") != strings.Join(c.body, "\n") {
			t.Errorf("body(%+v) = %v, want %v", c.fe, got, c.body)
		}
	}
}

// File rows in the "Edited files" section are colored by op letter.
func TestTurnStyleDiffOps(t *testing.T) {
	for style, role := range map[string]string{"diff-add": "add", "diff-del": "del", "diff-mod": "meta"} {
		if fg, _ := turnStyle(style); fg != roleColor(role) {
			t.Errorf("turnStyle(%q) = %v, want roleColor(%q) = %v", style, fg, role, roleColor(role))
		}
	}
}

// renderSpans must clip the joined text at the given width across segment
// boundaries, since clip is not ANSI-aware and runs per segment. A selected
// line keeps its segment colors and pads the cursor background to the width.
func TestRenderSpansClipsAcrossSegments(t *testing.T) {
	spans := []labelSpan{{"abc", "add"}, {"def", "del"}}
	if got := stripANSI(renderSpans(spans, 4, false)); got != "abcd" {
		t.Fatalf("renderSpans(w=4) = %q, want %q", got, "abcd")
	}
	if got := stripANSI(renderSpans(spans, 10, false)); got != "abcdef" {
		t.Fatalf("renderSpans(w=10) = %q, want %q", got, "abcdef")
	}
	if got := stripANSI(renderSpans(spans, 10, true)); got != "abcdef    " {
		t.Fatalf("selected renderSpans(w=10) = %q, want %q", got, "abcdef    ")
	}
}

func TestDiffLineStyle(t *testing.T) {
	cases := map[string]string{
		"+added":             "add",
		"-removed":           "del",
		" context":           "plain",
		"@@":                 "meta",
		"*** Update File: x": "meta",
	}
	for in, want := range cases {
		if got := diffLineStyle(in); got != want {
			t.Errorf("diffLineStyle(%q) = %q, want %q", in, got, want)
		}
	}
}
