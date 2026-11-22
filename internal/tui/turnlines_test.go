package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
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

// A question and its answer are read as conversation: open from the start, the
// question in the agent's color and the answer in the user's, down to the last
// line. An option is a "- " line, and must not be colored as a line removed
// from a file the way such a line in a tool's body is.
func TestQuestionAndAnswerBlocksReadAsConversation(t *testing.T) {
	q := eventBlock(domain.Event{Kind: domain.EventQuestion, Text: "範囲: どこまで？\n- 全章\n+ 追記も\n    preview"})
	a := eventBlock(domain.Event{Kind: domain.EventAnswer, Text: "範囲: 全章\n- 年表は除く"})
	if q.Label != "ASSISTANT (question)" || a.Label != "USER (answer)" {
		t.Fatalf("labels=%q,%q", q.Label, a.Label)
	}
	if !q.Open || !a.Open {
		t.Fatalf("open=%v,%v: a question and its answer are shown without a keypress", q.Open, a.Open)
	}
	m := Model{width: 80, turnBlocks: []turnBlock{q, a}, turnExpanded: map[int]bool{0: true, 1: true}}
	seen := map[int]int{}
	for _, ln := range m.turnFullLines() {
		want := roleColor("assistant")
		if ln.block == 1 {
			want = roleColor("user")
		}
		if fg, _ := turnStyle(ln.style); fg != want {
			t.Errorf("%q is in color %q, want %q", ln.text, fg, want)
		}
		seen[ln.block]++
	}
	if seen[0] != 5 || seen[1] != 3 {
		t.Errorf("rendered %d and %d lines, want each label and every line of its body", seen[0], seen[1])
	}
}

func texts(lines []turnLine) []string {
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = ln.text
	}
	return out
}
