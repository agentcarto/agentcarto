package main

import (
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
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
	got := pendingTurns(turns, stored, false)
	if len(got) != 1 || !got[4] {
		t.Fatalf("pending=%v want only turn 4", got)
	}
}

func TestPendingTurnsOnAFreshSession(t *testing.T) {
	turns := []transcript.Turn{{Index: 0}, {Index: 1}}
	got := pendingTurns(turns, nil, false)
	if len(got) != 2 || !got[1] || !got[2] {
		t.Fatalf("pending=%v want both turns", got)
	}
}

func TestPendingTurnsOnAFullySummarizedSession(t *testing.T) {
	turns := []transcript.Turn{{Index: 0}, {Index: 1}}
	stored := map[int]cache.Summary{1: {Turn: 1, Text: "a"}, 2: {Turn: 2, Text: "b"}}
	if got := pendingTurns(turns, stored, false); len(got) != 0 {
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
	got := pendingTurns(turns, stored, false)
	if len(got) != 1 || !got[2] {
		t.Fatalf("pending=%v want only turn 2", got)
	}
}

// The session summary is not one of the turns: storing it must not make the
// last turn look done.
func TestPendingTurnsIgnoresTheSessionSummary(t *testing.T) {
	turns := []transcript.Turn{{Index: 0}}
	stored := map[int]cache.Summary{0: {Turn: 0, Text: "the session"}}
	got := pendingTurns(turns, stored, false)
	if len(got) != 1 || !got[1] {
		t.Fatalf("pending=%v want turn 1", got)
	}
}

// The turn an agent is still writing is not asked about. Its terminal node moves
// as the agent writes, and the store withholds a summary whose node moved, so
// the call would be paid for and the answer never shown. Waiting for the whole
// session to go quiet — which is what settleBefore did — held back the finished
// turns too.
func TestPendingTurnsLeavesOutTheTurnStillBeingWritten(t *testing.T) {
	turns := []transcript.Turn{{Index: 0}, {Index: 1}, {Index: 2}}
	got := pendingTurns(turns, nil, true)
	if len(got) != 2 || !got[1] || !got[2] {
		t.Fatalf("pending=%v want the two finished turns (1, 2), not the one in progress", got)
	}
	// The same session a moment later, its last turn finished.
	if got := pendingTurns(turns, nil, false); len(got) != 3 {
		t.Fatalf("pending=%v want all three once the turn is complete", got)
	}
}

// A session whose only turn is in progress has nothing that can be summarized
// yet. It must not read as "everything is done".
func TestPendingTurnsOnASessionThatIsOnlyOneOpenTurn(t *testing.T) {
	turns := []transcript.Turn{{Index: 0}}
	if got := pendingTurns(turns, nil, true); len(got) != 0 {
		t.Fatalf("pending=%v want none", got)
	}
	if got := summarizableTurns(turns, true); len(got) != 0 {
		t.Fatalf("summarizable=%v want none", got)
	}
}

func TestSummarizableTurnsOnNothing(t *testing.T) {
	if got := summarizableTurns(nil, true); len(got) != 0 {
		t.Fatalf("summarizable=%v want none", got)
	}
}

// Deciding a turn is open is what stops it being summarized, and nothing asks
// again — so anything the plugin does not tell us has to answer "not open".
//
// A plugin need not report what its log ends with: plugin-copilot reads exported
// VS Code chat files and leaves LastKind unset, and plugin-grok returns it empty
// for a session whose events file is missing. Read as "mid-turn", every one of
// those sessions would lose its newest turn permanently.
func TestHasOpenTurnAnswersNoWhenTheAgentDoesNotSay(t *testing.T) {
	turns := []transcript.Turn{{Index: 0, Nodes: []string{"a"}}, {Index: 1, Nodes: []string{"b"}}}
	path := []string{"a", "b"}
	if hasOpenTurn(turns, path, "", 0) {
		t.Error("a session whose plugin reports no last kind was read as mid-turn")
	}
	if hasOpenTurn(turns, path, domain.EventTurnComplete, 0) {
		t.Error("a finished turn was read as open")
	}
	if !hasOpenTurn(turns, path, domain.EventStream, 0) {
		t.Error("a turn being written was not recognized")
	}
}

// transcript.Turns drops a trailing turn that holds nothing but a compact
// summary, so its last entry is not always the last turn of the branch. When it
// is not, that entry is a finished turn and must be summarized: held back, it
// would never be asked about again, because the compact turn ahead of it is not
// going to grow.
func TestHasOpenTurnIgnoresATurnThatIsNotAtTheEndOfTheBranch(t *testing.T) {
	// The branch is a → b, but b was a compact summary and Turns left it out.
	turns := []transcript.Turn{{Index: 0, Nodes: []string{"a"}, Compact: true}}
	if hasOpenTurn(turns, []string{"a", "b"}, domain.EventStream, 0) {
		t.Error("a finished turn was called open because the open one had been dropped")
	}
	// Same session, nothing dropped: now the last entry really is the open turn.
	if !hasOpenTurn(turns, []string{"a"}, domain.EventStream, 0) {
		t.Error("the turn at the end of the branch was not recognized as open")
	}
}

// A turn nobody is writing any more is not "in progress" — it is abandoned, and
// waiting for it to finish means waiting forever. The agent was interrupted, or
// crashed: the log stops mid-turn and never moves again, so its fingerprint
// never changes and nothing opens the session a second time.
//
// This is what settleBefore used to do for the whole session. Here it is asked
// of the one turn it is about, so the finished turns never wait for it.
func TestHasOpenTurnTreatsAnAbandonedTurnAsFinished(t *testing.T) {
	turns := []transcript.Turn{{Index: 0, Nodes: []string{"a"}}}
	path := []string{"a"}
	if !hasOpenTurn(turns, path, domain.EventStream, time.Minute) {
		t.Error("a turn written a minute ago was not treated as in progress")
	}
	if hasOpenTurn(turns, path, domain.EventStream, abandonedAfter) {
		t.Error("a turn untouched for the whole window was still treated as in progress")
	}
	if hasOpenTurn(turns, path, domain.EventStream, 48*time.Hour) {
		t.Error("a turn abandoned two days ago would be held back forever")
	}
}

func TestHasOpenTurnOnNothing(t *testing.T) {
	if hasOpenTurn(nil, []string{"a"}, domain.EventStream, 0) {
		t.Error("a session with no turns reported an open one")
	}
	if hasOpenTurn([]transcript.Turn{{Index: 0, Nodes: []string{"a"}}}, nil, domain.EventStream, 0) {
		t.Error("a session with no path reported an open turn")
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
