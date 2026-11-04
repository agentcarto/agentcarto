// Package transcript turns a parsed conversation into what is read: the turns
// of one branch, the files a turn changed, and a Markdown rendering of both.
// It is agent-agnostic — everything it consumes was normalized by the plugin at
// parse time — and free of any UI, so the TUI and the CLI produce the same turn
// numbers and the same document.
package transcript

import (
	"github.com/agentcarto/core/conversation"
	"github.com/agentcarto/core/domain"
	"sort"
	"time"
)

// Turn is one turn of a branch, in the shape the reader sees it.
type Turn struct {
	// Index is the turn's position among the turns a reader can open, counted
	// along the whole line from the conversation's root. It is the number the TUI
	// shows as "turn #N" (Index+1) and the one an exported document carries.
	// Summary-only compact boundaries are left out before this index is assigned,
	// so public turn numbers are contiguous.
	Index int
	Nodes []string
	// Compact marks a turn whose context was compacted at its boundary (the TUI's
	// » badge). Where the agent stopped seeing what came before matters to whoever
	// reads the turn.
	Compact bool
}

// Turns splits a branch into the turns a reader is shown. A turn holding nothing
// but a compact summary gets no entry of its own — there is nothing in it to
// read — and passes its Compact mark to the turn that follows it (or, at the end
// of the branch, to the one before).
//
// A path that starts below the root — a rewound line, or a fork's own turns —
// is numbered on from the turn it branched after, not from 1. That is the
// number a fork's own log gives the same turn, since the log carries a copy of
// what came before it, and the one its summaries are stored under.
func Turns(c domain.Conversation, path []string) []Turn {
	out := turnsOf(c, path)
	if len(path) == 0 || c.Nodes[path[0]].Parent == "" {
		return out
	}
	line := append(ancestors(c, path[0]), path...)
	pos := make(map[string]int, len(line))
	for i, id := range line {
		pos[id] = i
	}
	// Where each turn of the whole line ends, in line order. A turn of path is
	// numbered by how many of them end at or before it does, rather than by
	// looking up the line's turn holding its last node: the two group path's
	// first turn differently when the branch starts inside a turn, and a turn
	// the line leaves out as compact-only can hold one that path shows.
	var ends []int
	for _, t := range turnsOf(c, line) {
		ends = append(ends, pos[t.Nodes[len(t.Nodes)-1]])
	}
	for i := range out {
		last := pos[out[i].Nodes[len(out[i].Nodes)-1]]
		out[i].Index = max(0, sort.SearchInts(ends, last+1)-1)
	}
	return out
}

// Count is how many turns Turns returns for path, without numbering them along
// the line from the root, which a caller that only shows the count would be
// paying for on every redraw.
func Count(c domain.Conversation, path []string) int { return len(turnsOf(c, path)) }

// ancestors returns the nodes above id, from the root down to id's parent.
func ancestors(c domain.Conversation, id string) []string {
	var up []string
	seen := map[string]bool{id: true}
	for cur := c.Nodes[id].Parent; cur != "" && !seen[cur]; {
		n, ok := c.Nodes[cur]
		if !ok {
			break
		}
		seen[cur] = true
		up = append(up, cur)
		cur = n.Parent
	}
	for i, j := 0, len(up)-1; i < j; i, j = i+1, j-1 {
		up[i], up[j] = up[j], up[i]
	}
	return up
}

// turnsOf is Turns numbered from path's own first turn.
func turnsOf(c domain.Conversation, path []string) []Turn {
	var out []Turn
	carry := false
	for _, nodes := range conversation.TurnsOfPath(c, path) {
		compact := conversation.TurnIsCompact(c, nodes)
		if compact && !conversation.TurnHasRealContent(c, nodes) {
			carry = true
			continue
		}
		out = append(out, Turn{Index: len(out), Nodes: nodes, Compact: carry || compact})
		carry = false
	}
	if carry && len(out) > 0 {
		out[len(out)-1].Compact = true
	}
	return out
}

// Branches counts the lines of conversation that leave the given path and are
// worth opening: a node whose parent is on the path but which is not on it, and
// which holds a conversation rather than a leftover. Each is a rewind or an
// alternative the session took and abandoned, and a document that renders one
// path says nothing about them — a reader who is told "31 turns" would otherwise
// take that for everything that was said.
//
// Counting every departing node instead would report leftovers an agent wrote
// but nobody can read, and would disagree with the TUI, which lists exactly the
// substantial ones.
func Branches(c domain.Conversation, path []string) int {
	onPath := make(map[string]bool, len(path))
	for _, id := range path {
		onPath[id] = true
	}
	n := 0
	for _, id := range path {
		for _, child := range c.Children[id] {
			if !onPath[child] && conversation.IsSubstantial(c, child) {
				n++
			}
		}
	}
	return n
}

// Events returns the events of a turn in node order.
func (t Turn) Events(c domain.Conversation) []domain.Event { return EventsOf(c, t.Nodes) }

// EventsOf concatenates the events of the given nodes, in the order the nodes
// are given. Callers that hold node ids without a Turn (the turn list, which
// works row by row) use it directly.
func EventsOf(c domain.Conversation, ids []string) []domain.Event {
	var out []domain.Event
	for _, id := range ids {
		out = append(out, c.Nodes[id].Events...)
	}
	return out
}

// TimedEvents returns the events that make up a turn's time: all of them but those written
// between turns (domain.Event.BetweenTurns), such as an idle compaction the turn is filed
// under. A turn that holds nothing else keeps them all, so it still has a time.
func TimedEvents(events []domain.Event) []domain.Event {
	own := make([]domain.Event, 0, len(events))
	for _, e := range events {
		if !e.BetweenTurns && !e.Timestamp.IsZero() {
			own = append(own, e)
		}
	}
	if len(own) == 0 {
		return events
	}
	return own
}

// TurnTime returns the smallest (earliest) timestamp among a turn's timed events (TimedEvents).
// It does not depend on the events' order, guaranteeing chronological correctness (min of the
// times).
func TurnTime(events []domain.Event) time.Time {
	var t time.Time
	for _, e := range TimedEvents(events) {
		if e.Timestamp.IsZero() {
			continue
		}
		if t.IsZero() || e.Timestamp.Before(t) {
			t = e.Timestamp
		}
	}
	return t
}
