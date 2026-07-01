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

func TestUnifiedHunksChange(t *testing.T) {
	lines, added, removed := unifiedHunks("a\nb\nc", "a\nB\nc")
	if added != 1 || removed != 1 {
		t.Fatalf("counts = +%d -%d, want +1 -1", added, removed)
	}
	want := []string{"@@", " a", "-b", "+B", " c"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("hunk =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestUnifiedHunksAddOnly(t *testing.T) {
	lines, added, removed := unifiedHunks("", "x\ny")
	if added != 2 || removed != 0 {
		t.Fatalf("counts = +%d -%d, want +2 -0", added, removed)
	}
	if !hasLine(lines, "+x") || !hasLine(lines, "+y") || hasLine(lines, "-") {
		t.Fatalf("add-only hunk unexpected: %v", lines)
	}
}

func TestUnifiedHunksDeleteOnly(t *testing.T) {
	lines, added, removed := unifiedHunks("x\ny\nz", "x\nz")
	if added != 0 || removed != 1 {
		t.Fatalf("counts = +%d -%d, want +0 -1", added, removed)
	}
	if !hasLine(lines, "-y") {
		t.Fatalf("delete hunk missing -y: %v", lines)
	}
}

func TestUnifiedHunksNoChange(t *testing.T) {
	lines, added, removed := unifiedHunks("a\nb", "a\nb")
	if lines != nil || added != 0 || removed != 0 {
		t.Fatalf("no-change should be empty, got %v +%d -%d", lines, added, removed)
	}
}

func toolCall(name, text string) domain.Event {
	return domain.Event{Kind: domain.EventToolCall, ToolName: name, Text: text}
}

func TestTurnFileEditsClaudeEdit(t *testing.T) {
	e := toolCall("Edit", `{"file_path":"/x/foo.go","old_string":"a\nb","new_string":"a\nc"}`)
	fes := turnFileEdits([]domain.Event{e})
	if len(fes) != 1 {
		t.Fatalf("want 1 file, got %d", len(fes))
	}
	fe := fes[0]
	if fe.Path != "/x/foo.go" || fe.Added != 1 || fe.Removed != 1 {
		t.Fatalf("fileEdit = %+v", fe)
	}
	if fe.Diff[0] != "*** Update File: /x/foo.go" {
		t.Fatalf("header = %q", fe.Diff[0])
	}
	if !hasLine(fe.Diff, "-b") || !hasLine(fe.Diff, "+c") {
		t.Fatalf("diff body missing -b/+c: %v", fe.Diff)
	}
}

func TestTurnFileEditsWriteIsAddFile(t *testing.T) {
	e := toolCall("Write", `{"file_path":"/x/new.go","content":"one\ntwo"}`)
	fes := turnFileEdits([]domain.Event{e})
	if len(fes) != 1 || fes[0].Diff[0] != "*** Add File: /x/new.go" {
		t.Fatalf("Write should be Add File, got %v", fes)
	}
	if fes[0].Added != 2 || fes[0].Removed != 0 {
		t.Fatalf("Write counts = +%d -%d, want +2 -0", fes[0].Added, fes[0].Removed)
	}
}

func TestTurnFileEditsSameFileMerged(t *testing.T) {
	e1 := toolCall("Edit", `{"file_path":"/x/foo.go","old_string":"a","new_string":"b"}`)
	e2 := toolCall("Edit", `{"file_path":"/x/foo.go","old_string":"c","new_string":"d"}`)
	fes := turnFileEdits([]domain.Event{e1, e2})
	if len(fes) != 1 {
		t.Fatalf("same file should merge into 1, got %d", len(fes))
	}
	// one header, two @@ hunks
	if strings.Count(strings.Join(fes[0].Diff, "\n"), "*** ") != 1 {
		t.Fatalf("want single header: %v", fes[0].Diff)
	}
	if strings.Count(strings.Join(fes[0].Diff, "\n"), "@@") != 2 {
		t.Fatalf("want 2 hunks: %v", fes[0].Diff)
	}
}

func TestTurnFileEditsCodexApplyPatch(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: pkg/a.go\n@@\n-old\n+new\n*** End Patch"
	e := toolCall("apply_patch", patch)
	fes := turnFileEdits([]domain.Event{e})
	if len(fes) != 1 || fes[0].Path != "pkg/a.go" {
		t.Fatalf("apply_patch parse = %v", fes)
	}
	if fes[0].Added != 1 || fes[0].Removed != 1 {
		t.Fatalf("codex counts = +%d -%d, want +1 -1", fes[0].Added, fes[0].Removed)
	}
	if fes[0].Diff[0] != "*** Update File: pkg/a.go" || !hasLine(fes[0].Diff, "+new") {
		t.Fatalf("codex diff body = %v", fes[0].Diff)
	}
}

func TestTurnFileEditsFileChangeNoBody(t *testing.T) {
	e := domain.Event{Kind: domain.EventFileChange, Text: `{"files":["/y/bar.go"],"added":3,"removed":1}`}
	fes := turnFileEdits([]domain.Event{e})
	if len(fes) != 1 || !fes[0].noBody {
		t.Fatalf("file_change should be body-less, got %v", fes)
	}
	if !hasLine(fes[0].Diff, "(no diff body)") {
		t.Fatalf("want no-body note: %v", fes[0].Diff)
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

func countLine(lines []string, want string) int {
	n := 0
	for _, l := range lines {
		if l == want {
			n++
		}
	}
	return n
}

// Codex patch_apply_end now carries the real diff as an apply_patch document.
func TestTurnFileEditsCodexFileChangeDiff(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: a.go\n--- a/a.go\n+++ b/a.go\n+new\n-old\n*** End Patch"
	e := domain.Event{Kind: domain.EventFileChange, Text: patch}
	fes := turnFileEdits([]domain.Event{e})
	if len(fes) != 1 || fes[0].Path != "a.go" || fes[0].noBody {
		t.Fatalf("file_change diff = %+v", fes)
	}
	if fes[0].Added != 1 || fes[0].Removed != 1 {
		t.Fatalf("counts +%d -%d, want +1 -1", fes[0].Added, fes[0].Removed)
	}
	if !hasLine(fes[0].Diff, "+new") || !hasLine(fes[0].Diff, "-old") {
		t.Fatalf("missing diff body: %v", fes[0].Diff)
	}
}

// An apply_patch tool_call and its patch_apply_end file_change describe the same
// change; they must not produce two entries or doubled hunks/counts.
func TestTurnFileEditsDedupsToolCallAndFileChange(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: a.go\n+new\n-old\n*** End Patch"
	events := []domain.Event{toolCall("apply_patch", patch), {Kind: domain.EventFileChange, Text: patch}}
	fes := turnFileEdits(events)
	if len(fes) != 1 {
		t.Fatalf("want 1 deduped file, got %d: %+v", len(fes), fes)
	}
	if fes[0].Added != 1 || fes[0].Removed != 1 {
		t.Fatalf("dedup counts +%d -%d, want +1 -1", fes[0].Added, fes[0].Removed)
	}
	if n := countLine(fes[0].Diff, "+new"); n != 1 {
		t.Fatalf("diff duplicated (+new x%d): %v", n, fes[0].Diff)
	}
}

func TestEditStatsNoDoubleCountWithFileChange(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: a.go\n+new\n-old\n*** End Patch"
	events := []domain.Event{toolCall("apply_patch", patch), {Kind: domain.EventFileChange, Text: patch}}
	files, added, removed := editStats(events)
	if files != 1 || added != 1 || removed != 1 {
		t.Fatalf("editStats = files%d +%d -%d, want 1 +1 -1", files, added, removed)
	}
}
