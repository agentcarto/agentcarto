package summary

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

// session builds a conversation of n turns, each carrying an assistant reply of
// the given size, so that batching can be driven by real rendered sizes.
func session(t *testing.T, n, replyChars int) (domain.Session, domain.Conversation, []transcript.Turn) {
	t.Helper()
	var nodes []domain.ConvNode
	prev := ""
	for i := range n {
		u := fmt.Sprintf("u%d", i)
		a := fmt.Sprintf("a%d", i)
		nodes = append(nodes,
			domain.ConvNode{ID: u, Parent: prev, Timestamp: time.Unix(int64(i*2+1), 0),
				Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
			domain.ConvNode{ID: a, Parent: u, Timestamp: time.Unix(int64(i*2+2), 0),
				Events: []domain.Event{{Kind: domain.EventAssistant, Text: strings.Repeat("x", replyChars)}}},
		)
		prev = a
	}
	c := domain.NewConversation(nodes)
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s1", Title: "t"}
	return s, c, transcript.Turns(c, c.ActivePath())
}

func allTurns(turns []transcript.Turn) map[int]bool {
	want := map[int]bool{}
	for _, t := range turns {
		want[t.Index+1] = true
	}
	return want
}

// A short session is one call: splitting when nothing needs it would multiply
// the per-call overhead for no reason.
func TestBatchKeepsAShortSessionWhole(t *testing.T) {
	s, c, turns := session(t, 8, 200)
	got := Batch(c, turns, allTurns(turns), s.CWD)
	if len(got) != 1 || len(got[0]) != 8 {
		t.Fatalf("batches=%v want one batch of 8", got)
	}
	for i, n := range got[0] {
		if n != i+1 {
			t.Errorf("batch is not in turn order: %v", got[0])
		}
	}
}

// The output limit binds first on a long session: a summary runs 300-500
// characters, so asking about hundreds of turns at once produces an answer that
// is cut at the limit — losing every turn after the cut, at full price.
func TestBatchSplitsOnTurnCount(t *testing.T) {
	s, c, turns := session(t, maxTurnsPerCall*2+5, 100)
	got := Batch(c, turns, allTurns(turns), s.CWD)
	if len(got) != 3 {
		t.Fatalf("%d turns produced %d batches, want 3", len(turns), len(got))
	}
	total := 0
	for _, b := range got {
		if len(b) > maxTurnsPerCall {
			t.Errorf("a batch holds %d turns, over the %d limit", len(b), maxTurnsPerCall)
		}
		total += len(b)
	}
	if total != len(turns) {
		t.Errorf("batches hold %d turns, want all %d", total, len(turns))
	}
}

// Turn sizes vary by orders of magnitude, so a fixed number of turns per call
// is either wasteful or too big. Long turns split sooner.
func TestBatchSplitsOnSize(t *testing.T) {
	// Each turn renders to more than a third of the limit, so no more than two
	// fit in a batch however few turns that is.
	s, c, turns := session(t, 6, maxCharsPerCall/2)
	got := Batch(c, turns, allTurns(turns), s.CWD)
	if len(got) < 3 {
		t.Fatalf("6 oversized turns produced %d batches, want at least 3", len(got))
	}
	sizes := transcript.TurnSizes(c, turns, s.CWD, transcript.Options{Tools: transcript.ToolsBrief})
	for _, b := range got {
		n := 0
		for _, turn := range b {
			n += sizes[turn]
		}
		if len(b) > 1 && n > maxCharsPerCall {
			t.Errorf("a batch of %d turns renders to %d chars, over the %d limit", len(b), n, maxCharsPerCall)
		}
	}
}

// A turn too large for a limit still gets asked about. A call that may fail
// beats a turn that can never be summarized at all.
func TestBatchKeepsATurnLargerThanTheLimit(t *testing.T) {
	s, c, turns := session(t, 1, maxCharsPerCall*2)
	got := Batch(c, turns, allTurns(turns), s.CWD)
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != 1 {
		t.Fatalf("batches=%v want the oversized turn on its own", got)
	}
}

// Only the turns that were asked for are batched, and turns that render to
// nothing are left out — asking about one would wait forever for an answer that
// cannot come.
func TestBatchCoversOnlyWhatWasAskedFor(t *testing.T) {
	s, c, turns := session(t, 10, 100)
	got := Batch(c, turns, map[int]bool{3: true, 7: true}, s.CWD)
	if len(got) != 1 || len(got[0]) != 2 || got[0][0] != 3 || got[0][1] != 7 {
		t.Fatalf("batches=%v want one batch of [3 7]", got)
	}
	if got := Batch(c, turns, nil, s.CWD); len(got) != 0 {
		t.Errorf("asking about no turns produced %v", got)
	}
}

func TestTurnSet(t *testing.T) {
	got := TurnSet([]int{3, 7})
	if len(got) != 2 || !got[3] || !got[7] || got[1] {
		t.Fatalf("TurnSet=%v", got)
	}
	if got := TurnSet(nil); len(got) != 0 {
		t.Errorf("TurnSet(nil)=%v", got)
	}
}

// Batching decides what one call carries, so the prompt built from a batch has
// to stay under the limit it was batched for.
func TestBatchesProducePromptsWithinTheLimit(t *testing.T) {
	s, c, turns := session(t, 150, 1000)
	batches := Batch(c, turns, allTurns(turns), s.CWD)
	if len(batches) < 2 {
		t.Fatalf("150 turns produced %d batches", len(batches))
	}
	for i, b := range batches {
		doc, asked := Prompt(s, c, turns, Options{Turns: TurnSet(b)})
		if len(asked) != len(b) {
			t.Errorf("batch %d asked about %d turns, want %d", i, len(asked), len(b))
		}
		// The context blocks Prompt adds after the split are reserved for, and the
		// header fits in what the reserve leaves over: it is worked out at four
		// bytes to the rune, which no real log reaches.
		if len(doc) > maxCharsPerCall {
			t.Errorf("batch %d renders to %d chars, over the %d limit", i, len(doc), maxCharsPerCall)
		}
	}
}

// A batch that starts partway through a session opens on a turn whose
// predecessor is in the batch before it. What that turn closed with is carried
// over, and the first batch — which opens the session — has nothing to carry.
func TestLaterBatchesCarryWhatTheFirstDoesNot(t *testing.T) {
	s, c, turns := session(t, 150, 1000)
	batches := Batch(c, turns, allTurns(turns), s.CWD)
	if len(batches) < 2 {
		t.Fatalf("150 turns produced %d batches", len(batches))
	}
	for i, b := range batches {
		doc, _ := Prompt(s, c, turns, Options{Turns: TurnSet(b)})
		switch carries := strings.Contains(doc, transcript.ContextLabel); {
		case i == 0 && carries:
			t.Error("the first batch opens the session; nothing precedes it to carry over")
		case i > 0 && !carries:
			t.Errorf("batch %d does not carry what the turn before it closed with", i)
		}
	}
}

// A scattered set of turns — what a model that skipped some leaves for the next
// run — needs a block above every heading, which would carry half a call's
// worth of text nobody asked for. The budget stops it, and the reserve Batch
// holds back keeps what does get carried inside the limit the split worked to.
func TestContextStaysWithinItsBudget(t *testing.T) {
	s, c, turns := session(t, 200, 1000)
	want := map[int]bool{}
	for _, t := range turns {
		if n := t.Index + 1; n%2 == 1 {
			want[n] = true
		}
	}
	batches := Batch(c, turns, want, s.CWD)
	if len(batches) < 2 {
		t.Fatalf("100 scattered turns produced %d batches", len(batches))
	}
	for i, b := range batches {
		doc, _ := Prompt(s, c, turns, Options{Turns: TurnSet(b)})
		if n, max := strings.Count(doc, transcript.ContextLabel), maxContextRunesPerDoc/contextRunes; n > max {
			t.Errorf("batch %d carries %d context blocks, over the %d the budget allows", i, n, max)
		}
		if len(doc) > maxCharsPerCall {
			t.Errorf("batch %d renders to %d chars, over the %d limit the split reserved for", i, len(doc), maxCharsPerCall)
		}
	}
}
