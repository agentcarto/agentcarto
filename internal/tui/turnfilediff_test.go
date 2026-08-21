package tui

import (
	"testing"

	"github.com/agentcarto/core/domain"
)

func editEvent(kind domain.EventKind, changes ...domain.FileChange) domain.Event {
	return domain.Event{Kind: kind, ToolName: "Edit", Changes: changes}
}

func TestEditStatsNoDoubleCountWithFileChange(t *testing.T) {
	fc := domain.FileChange{Path: "a.go", Added: 1, Removed: 1, Diff: "+new\n-old"}
	events := []domain.Event{editEvent(domain.EventToolCall, fc), editEvent(domain.EventFileChange, fc)}
	files, added, removed := editStats(events)
	if files != 1 || added != 1 || removed != 1 {
		t.Fatalf("editStats = files%d +%d -%d, want 1 +1 -1", files, added, removed)
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
