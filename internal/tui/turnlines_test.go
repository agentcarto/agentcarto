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

func texts(lines []turnLine) []string {
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = ln.text
	}
	return out
}
