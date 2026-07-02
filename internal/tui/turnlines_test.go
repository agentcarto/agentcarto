package tui

import (
	"strings"
	"testing"
	"time"
)

// Expanding a block adds no blank separator line: the next block's header
// follows the body directly.
func TestTurnFullLinesNoBlankLineAfterExpandedBody(t *testing.T) {
	ts := time.Date(2026, 7, 3, 12, 0, 0, 0, time.Local)
	m := Model{width: 40, turnBlocks: []turnBlock{
		{Sym: "▪", Style: "plain", Label: "first", Time: ts, Body: []string{"body"}},
		{Sym: "▪", Style: "plain", Label: "second", Time: ts},
	}, turnExpanded: map[int]bool{0: true}}
	lines := m.turnFullLines()
	if len(lines) != 3 {
		t.Fatalf("line count=%d want 3 (header, body, next header): %q", len(lines), texts(lines))
	}
	for i, ln := range lines {
		if strings.TrimSpace(ln.text) == "" {
			t.Fatalf("blank separator line at %d: %q", i, texts(lines))
		}
	}
}

// NoGutter blocks (the edited-files section) render flush left, with no
// timestamp gutter on the header and a matching shallow body indent.
func TestTurnFullLinesNoGutterPacksLeft(t *testing.T) {
	m := Model{width: 40, turnBlocks: []turnBlock{{
		Sym: "*", Style: "tool", Label: "Edited files (1)", NoGutter: true,
		Body: []string{"hunk"},
	}}, turnExpanded: map[int]bool{0: true}}
	lines := m.turnFullLines()
	if strings.HasPrefix(lines[0].text, " ") {
		t.Fatalf("NoGutter header is not flush left: %q", lines[0].text)
	}
	if want := "    hunk"; lines[1].text != want {
		t.Fatalf("NoGutter body indent: %q want %q", lines[1].text, want)
	}
}

// Edited-file paths render relative to the session's working directory;
// paths outside it (or already relative) stay as-is.
func TestRelCWD(t *testing.T) {
	cases := []struct{ path, cwd, want string }{
		{"/repo/app/internal/x.go", "/repo/app", "internal/x.go"},
		{"/etc/hosts", "/repo/app", "/etc/hosts"},
		{"/repo/app2/x.go", "/repo/app", "/repo/app2/x.go"}, // sibling with a shared name prefix
		{"internal/x.go", "/repo/app", "internal/x.go"},     // already relative
		{"/repo/app/x.go", "", "/repo/app/x.go"},            // no cwd known
	}
	for _, c := range cases {
		if got := relCWD(c.path, c.cwd); got != c.want {
			t.Fatalf("relCWD(%q, %q)=%q want %q", c.path, c.cwd, got, c.want)
		}
	}
}

func texts(lines []turnLine) []string {
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = ln.text
	}
	return out
}
