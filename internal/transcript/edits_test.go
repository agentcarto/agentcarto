package transcript

import (
	"github.com/agentcarto/core/domain"
	"strings"
	"testing"
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
	fes := FileEdits([]domain.Event{e})
	if len(fes) != 1 {
		t.Fatalf("want 1 file, got %d", len(fes))
	}
	fe := fes[0]
	if fe.Path != "/x/foo.go" || fe.Added != 1 || fe.Removed != 1 || fe.Status() != "M" {
		t.Fatalf("FileEdit = %+v", fe)
	}
	if !hasLine(fe.Diff, "-b") || !hasLine(fe.Diff, "+c") {
		t.Fatalf("diff body missing -b/+c: %v", fe.Diff)
	}
}

func TestTurnFileEditsSameFileMerged(t *testing.T) {
	e1 := editEvent(domain.EventToolCall, domain.FileChange{Path: "/x/foo.go", Added: 1, Removed: 1, Diff: "@@\n-a\n+b"})
	e2 := editEvent(domain.EventToolCall, domain.FileChange{Path: "/x/foo.go", Added: 1, Removed: 1, Diff: "@@\n-c\n+d"})
	fes := FileEdits([]domain.Event{e1, e2})
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
	fes := FileEdits([]domain.Event{e})
	if len(fes) != 1 || !fes[0].NoBody || fes[0].Added != 3 || fes[0].Removed != 1 {
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
	fes := FileEdits(events)
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

// The block label shows the op letter and path. Bare "@@" markers carry no
// information and become blank-line hunk separators (none at the edges);
// "@@ <context>" markers are kept.
func TestFileEditStatusAndBody(t *testing.T) {
	cases := []struct {
		fe     FileEdit
		status string
		body   []string
	}{
		{FileEdit{Op: "update", Diff: []string{"@@", "+x"}}, "M", []string{"+x"}},
		{FileEdit{Op: "add", Diff: []string{"+y"}}, "A", []string{"+y"}},
		{FileEdit{Op: "delete"}, "D", nil},
		{FileEdit{}, "M", nil},
		{
			FileEdit{Diff: []string{"@@", " a", "-b", "+c", "@@", " d", "+e", "@@"}},
			"M",
			[]string{" a", "-b", "+c", "", " d", "+e"},
		},
		{
			FileEdit{Diff: []string{"@@ func main", "+x"}},
			"M",
			[]string{"@@ func main", "+x"},
		},
	}
	for _, c := range cases {
		if got := c.fe.Status(); got != c.status {
			t.Errorf("Status(%+v) = %q, want %q", c.fe, got, c.status)
		}
		if got := c.fe.Body(); strings.Join(got, "\n") != strings.Join(c.body, "\n") {
			t.Errorf("Body(%+v) = %v, want %v", c.fe, got, c.body)
		}
	}
}

// Changes-bearing events (edit tool calls, file changes) are surfaced by the
// consolidated file section, not the chronological block list. A file_change
// WITHOUT Changes stays visible in the timeline instead of vanishing.
func TestInFileSectionCoversChangesBearingEvents(t *testing.T) {
	edit := domain.Event{Kind: domain.EventToolCall, ToolName: "Edit", Changes: []domain.FileChange{{Path: "a.go"}}}
	fc := domain.Event{Kind: domain.EventFileChange, Changes: []domain.FileChange{{Path: "a.go"}}}
	plain := domain.Event{Kind: domain.EventToolCall, ToolName: "Read", ToolArg: "/x/c.py"}
	bare := domain.Event{Kind: domain.EventFileChange, Text: "raw"}
	if !InFileSection(edit) || !InFileSection(fc) || InFileSection(plain) || InFileSection(bare) {
		t.Fatalf("InFileSection: edit=%v fc=%v plain=%v bare=%v", InFileSection(edit), InFileSection(fc), InFileSection(plain), InFileSection(bare))
	}
}
