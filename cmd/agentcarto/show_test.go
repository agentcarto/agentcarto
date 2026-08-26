package main

import (
	"strings"
	"testing"

	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

func showTurns(n int) []transcript.Turn {
	out := make([]transcript.Turn, 0, n)
	for i := range n {
		out = append(out, transcript.Turn{Index: i})
	}
	return out
}

func numbers(turns []transcript.Turn) []int {
	out := make([]int, len(turns))
	for i, t := range turns {
		out[i] = t.Index + 1
	}
	return out
}

func TestParseTurnSpec(t *testing.T) {
	got, err := parseTurnSpec(" 3, 7 , 11-13 ")
	if err != nil {
		t.Fatal(err)
	}
	if !got.explicit[3] || !got.explicit[7] || len(got.explicit) != 2 {
		t.Errorf("named numbers=%v", got.explicit)
	}
	if len(got.ranges) != 1 || got.ranges[0] != [2]int{11, 13} {
		t.Errorf("ranges=%v", got.ranges)
	}
	for _, bad := range []string{"", "abc", "0", "-3", "5-2", "3-", "3,,x"} {
		if _, err := parseTurnSpec(bad); err == nil {
			t.Errorf("--turns %q was accepted", bad)
		}
	}
}

func TestSelectTurns(t *testing.T) {
	turns := showTurns(5)

	got, err := selectTurns(turns, "2,4", 0, false)
	if err != nil || len(got) != 2 || numbers(got)[0] != 2 || numbers(got)[1] != 4 {
		t.Fatalf("selection=%v err=%v", numbers(got), err)
	}
	if got, err = selectTurns(turns, "2-4", 0, false); err != nil || len(got) != 3 {
		t.Fatalf("range selection=%v err=%v", numbers(got), err)
	}
	// A range that covers no turn at all is a mistake worth reporting.
	if _, err = selectTurns(turns, "20-30", 0, false); err == nil || !strings.Contains(err.Error(), "no turn between 20 and 30") {
		t.Fatalf("empty range err=%v", err)
	}
	if got, err = selectTurns(turns, "", 2, false); err != nil || numbers(got)[0] != 4 {
		t.Fatalf("--last 2 = %v err=%v", numbers(got), err)
	}
	// Asking for more than there is gives everything rather than an error: the
	// caller wanted the tail and there is nothing beyond it.
	if got, err = selectTurns(turns, "", 99, false); err != nil || len(got) != 5 {
		t.Fatalf("--last 99 = %v err=%v", numbers(got), err)
	}
	if got, err = selectTurns(turns, "", 0, true); err != nil || len(got) != 5 {
		t.Fatalf("--all = %v err=%v", numbers(got), err)
	}
	_, err = selectTurns(turns, "6", 0, false)
	if err == nil || !strings.Contains(err.Error(), "no such turn: 6") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(err.Error(), "turns 1 to 5") {
		t.Fatalf("the error should say what is there: %v", err)
	}
}

func TestToolOptions(t *testing.T) {
	for in, want := range map[string]transcript.ToolMode{
		"label": transcript.ToolsLabel, "full": transcript.ToolsFull, "none": transcript.ToolsNone,
	} {
		got, err := toolOptions(in)
		if err != nil || got.Tools != want {
			t.Errorf("toolOptions(%q)=%v %v", in, got.Tools, err)
		}
	}
	if _, err := toolOptions("verbose"); err == nil {
		t.Error("--tools verbose was accepted")
	}
}

// The outline is what `show` prints by default, so it has to carry everything
// needed to ask for more: the session's identity and every turn number.
func TestOutlineListsTheTurnNumbersShowTakes(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "最初の依頼", Prompt: "最初の依頼"}}},
		{ID: "c1", Parent: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "(summary)", RawType: domain.RawCompactSummary}}},
		{ID: "u2", Parent: "c1", Events: []domain.Event{{Kind: domain.EventUser, Text: "次の依頼", Prompt: "次の依頼"}}},
	})
	s := domain.Session{AgentType: "claude", SessionID: "8f3a2b1c", CWD: "/repo", Title: "t"}
	turns := transcript.Turns(c, c.ActivePath())
	got := transcript.Outline(s, c, turns, transcript.Options{Tools: transcript.ToolsLabel})
	for _, want := range []string{"- **Session**: 8f3a2b1c", "- **Turns**: 2",
		"- Turn 1", "最初の依頼", "- Turn 2", "次の依頼", "»", " B]"} {
		if !strings.Contains(got, want) {
			t.Errorf("outline is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "- Turn 3") {
		t.Errorf("the outline numbering is not contiguous:\n%s", got)
	}
	if _, err := selectTurns(turns, "1,2", 0, false); err != nil {
		t.Errorf("the outline's numbers were rejected: %v", err)
	}
}
