package transcript

import (
	"github.com/agentcarto/core/domain"
	"testing"
)

func userNode(id, parent, prompt string) domain.ConvNode {
	return domain.ConvNode{ID: id, Parent: parent, Events: []domain.Event{
		{Kind: domain.EventUser, Text: prompt, Prompt: prompt},
		{Kind: domain.EventAssistant, Text: "reply to " + prompt},
	}}
}

// compactNode is the summary a /compact leaves behind: a turn boundary with
// nothing in it to read.
func compactNode(id, parent string) domain.ConvNode {
	return domain.ConvNode{ID: id, Parent: parent, Events: []domain.Event{
		{Kind: domain.EventSystem, RawType: domain.RawCompactSummary, Text: "summary"},
	}}
}

func conv(nodes ...domain.ConvNode) domain.Conversation { return domain.NewConversation(nodes) }

func indexes(ts []Turn) []int {
	out := make([]int, len(ts))
	for i, t := range ts {
		out[i] = t.Index
	}
	return out
}

func TestTurnsNumbersEveryTurnOnThePath(t *testing.T) {
	c := conv(userNode("a", "", "first"), userNode("b", "a", "second"), userNode("c", "b", "third"))
	ts := Turns(c, c.ActivePath())
	if len(ts) != 3 {
		t.Fatalf("turns=%d", len(ts))
	}
	for i, turn := range ts {
		if turn.Index != i {
			t.Fatalf("turn %d has index %d", i, turn.Index)
		}
		if turn.Compact {
			t.Fatalf("turn %d should not be marked compact", i)
		}
	}
}

func TestTurnsCarriesTheCompactMarkForward(t *testing.T) {
	c := conv(userNode("a", "", "first"), compactNode("b", "a"), userNode("c", "b", "second"))
	ts := Turns(c, c.ActivePath())
	// The summary-only turn is not one of the turns, so the public indexes stay
	// contiguous. The compact boundary is carried separately by the badge.
	if got := indexes(ts); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("indexes=%v", got)
	}
	if ts[0].Compact {
		t.Fatal("the turn before the compact should not be marked")
	}
	if !ts[1].Compact {
		t.Fatal("the turn after the compact should carry the mark")
	}
}

func TestTurnsMarksThePreviousTurnForATrailingCompact(t *testing.T) {
	c := conv(userNode("a", "", "first"), compactNode("b", "a"))
	ts := Turns(c, c.ActivePath())
	if len(ts) != 1 || !ts[0].Compact {
		t.Fatalf("a trailing summary should mark the turn before it: %#v", ts)
	}
}

func TestTurnsKeepsACompactTurnThatAlsoHasContent(t *testing.T) {
	n := compactNode("b", "a")
	n.Events = append(n.Events, domain.Event{Kind: domain.EventUser, Text: "after", Prompt: "after"})
	c := conv(userNode("a", "", "first"), n)
	ts := Turns(c, c.ActivePath())
	if len(ts) != 2 {
		t.Fatalf("turns=%d, want the compacted turn kept for its content", len(ts))
	}
	if !ts[1].Compact {
		t.Fatal("the turn holding the compact boundary should be marked")
	}
}

func TestTurnEventsFollowNodeOrder(t *testing.T) {
	c := conv(userNode("a", "", "first"))
	ts := Turns(c, c.ActivePath())
	ev := ts[0].Events(c)
	if len(ev) != 2 || ev[0].Kind != domain.EventUser || ev[1].Kind != domain.EventAssistant {
		t.Fatalf("events=%#v", ev)
	}
}

// A session that was rewound holds lines of conversation the rendered path
// leaves out. Counting them is what keeps "31 turns" from reading as everything
// that was ever said.
func TestBranchesCountsWhatLeavesThePath(t *testing.T) {
	c := conv(
		userNode("a", "", "first"),
		userNode("b", "a", "second"),   // on the path
		userNode("b2", "a", "another"), // a rewind: same parent, abandoned
		userNode("c", "b", "third"),
		userNode("c2", "b", "yet another"),
	)
	path := c.ActivePath()
	if got := Branches(c, path); got != 2 {
		t.Fatalf("branches=%d want 2", got)
	}
	// A straight conversation has none.
	straight := conv(userNode("a", "", "first"), userNode("b", "a", "second"))
	if got := Branches(straight, straight.ActivePath()); got != 0 {
		t.Fatalf("branches=%d want 0 for a single line of conversation", got)
	}
}
