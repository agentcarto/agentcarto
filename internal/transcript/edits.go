package transcript

import (
	"github.com/agentcarto/core/domain"
	"strings"
)

// Changes collects a turn's normalized file changes. Applied changes
// (EventFileChange, the result) supersede requested ones (EventToolCall) in
// the same turn, so the same edit is never counted twice.
func Changes(events []domain.Event) []domain.FileChange {
	applied := false
	for _, e := range events {
		if e.Kind == domain.EventFileChange && len(e.Changes) > 0 {
			applied = true
			break
		}
	}
	var out []domain.FileChange
	for _, e := range events {
		if len(e.Changes) == 0 {
			continue
		}
		if applied && e.Kind != domain.EventFileChange {
			continue
		}
		out = append(out, e.Changes...)
	}
	return out
}

// FileEdit is one file's consolidated change within a turn, carrying an
// apply_patch body.
type FileEdit struct {
	Path           string
	Op             string // "add" / "update" (default) / "delete"
	Diff           []string
	Added, Removed int
	// NoBody marks an edit whose source carried aggregate counts only, with no
	// diff body to show.
	NoBody bool
}

// Status returns the git-style status letter (A/M/D).
func (fe FileEdit) Status() string {
	switch fe.Op {
	case "add":
		return "A"
	case "delete":
		return "D"
	}
	return "M"
}

// Body returns the hunks to render. Bare "@@" markers carry no line numbers or
// context, so they render as a blank line between hunks (and not at all at the
// edges); "@@ <context>" lines are kept.
func (fe FileEdit) Body() []string {
	out := make([]string, 0, len(fe.Diff))
	for _, ln := range fe.Diff {
		if ln == "@@" {
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		}
		out = append(out, ln)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// FileEdits consolidates the turn's plugin-normalized Changes by file.
// Files keep first-seen order; repeated edits to one file append their hunks
// under a single entry. A change without a diff body (aggregate counts only)
// becomes a body-less entry.
func FileEdits(events []domain.Event) []FileEdit {
	var out []FileEdit
	idx := map[string]int{}
	for _, fc := range Changes(events) {
		i, ok := idx[fc.Path]
		if !ok {
			i = len(out)
			idx[fc.Path] = i
			out = append(out, FileEdit{Path: fc.Path, Op: fc.Op})
		}
		if fc.Diff != "" {
			out[i].Diff = append(out[i].Diff, strings.Split(fc.Diff, "\n")...)
		} else {
			out[i].NoBody = true
		}
		out[i].Added += fc.Added
		out[i].Removed += fc.Removed
	}
	for i := range out {
		if out[i].NoBody && len(out[i].Diff) == 0 {
			out[i].Diff = []string{"(no diff body)"}
		}
	}
	return out
}

// InFileSection reports whether an event's edits are already covered by
// FileEdits, so a caller listing the turn's events chronologically leaves it
// out instead of showing the same edit twice. An event without Changes stays
// visible in the timeline (a file_change lacking them would otherwise silently
// render nowhere).
func InFileSection(e domain.Event) bool {
	return len(e.Changes) > 0
}
