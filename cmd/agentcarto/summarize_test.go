package main

import (
	"strings"
	"testing"

	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/transcript"
)

// A session that grew since it was summarized costs only its new turns. Turn
// numbers skip (a compact-summary turn takes a number without being one), so
// the selection has to work on numbers rather than positions.
func TestPendingTurnsAsksOnlyForWhatIsMissing(t *testing.T) {
	turns := []transcript.Turn{{Index: 0}, {Index: 2}, {Index: 3}}
	stored := map[int]cache.Summary{
		1: {Turn: 1, Text: "done"},
		3: {Turn: 3, Text: "done"},
	}
	got := pendingTurns(turns, stored)
	if len(got) != 1 || !got[4] {
		t.Fatalf("pending=%v want only turn 4", got)
	}
}

func TestPendingTurnsOnAFreshSession(t *testing.T) {
	turns := []transcript.Turn{{Index: 0}, {Index: 1}}
	got := pendingTurns(turns, nil)
	if len(got) != 2 || !got[1] || !got[2] {
		t.Fatalf("pending=%v want both turns", got)
	}
}

func TestPendingTurnsOnAFullySummarizedSession(t *testing.T) {
	turns := []transcript.Turn{{Index: 0}, {Index: 1}}
	stored := map[int]cache.Summary{1: {Turn: 1, Text: "a"}, 2: {Turn: 2, Text: "b"}}
	if got := pendingTurns(turns, stored); len(got) != 0 {
		t.Fatalf("pending=%v want none", got)
	}
}

// The store withholds a summary made against a different node, which is what a
// rewind produces. Those turns have to be asked about again — otherwise the
// session keeps showing summaries of content that is no longer there.
func TestPendingTurnsIncludesTurnsWhoseContentMoved(t *testing.T) {
	turns := []transcript.Turn{{Index: 0}, {Index: 1}, {Index: 2}}
	// The store returned nothing for turn 2: its node moved.
	stored := map[int]cache.Summary{1: {Turn: 1}, 3: {Turn: 3}}
	got := pendingTurns(turns, stored)
	if len(got) != 1 || !got[2] {
		t.Fatalf("pending=%v want only turn 2", got)
	}
}

// The session summary is not one of the turns: storing it must not make the
// last turn look done.
func TestPendingTurnsIgnoresTheSessionSummary(t *testing.T) {
	turns := []transcript.Turn{{Index: 0}}
	stored := map[int]cache.Summary{0: {Turn: 0, Text: "the session"}}
	got := pendingTurns(turns, stored)
	if len(got) != 1 || !got[1] {
		t.Fatalf("pending=%v want turn 1", got)
	}
}

func TestTurnList(t *testing.T) {
	cases := []struct {
		in   []int
		want string
	}{
		{[]int{4}, "turn 4"},
		{[]int{4, 7}, "turns 4, 7"},
		{[]int{1, 2, 3}, "turns 1, 2, 3"},
	}
	for _, c := range cases {
		if got := turnList(c.in); got != c.want {
			t.Errorf("turnList(%v)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[int]string{7: "c", 1: "a", 4: "b"})
	if len(got) != 3 || got[0] != 1 || got[1] != 4 || got[2] != 7 {
		t.Fatalf("sortedKeys=%v want [1 4 7]", got)
	}
	if got := sortedKeys(nil); len(got) != 0 {
		t.Errorf("sortedKeys(nil)=%v", got)
	}
}

// summarize is the one command that spends money, so its help has to say so
// before anyone runs it to see what it does.
func TestUsageWarnsThatSummarizeCosts(t *testing.T) {
	var b strings.Builder
	usage(&b)
	line := ""
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.Contains(l, "summarize") {
			line = l
		}
	}
	if line == "" {
		t.Fatal("usage does not mention summarize")
	}
	if !strings.Contains(line, "costs money") {
		t.Errorf("the usage line does not warn about the cost: %q", line)
	}
}
