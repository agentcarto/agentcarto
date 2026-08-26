// Package transcript turns a parsed conversation into what is read: the turns
// of one branch, the files a turn changed, and a Markdown rendering of both.
// It is agent-agnostic — everything it consumes was normalized by the plugin at
// parse time — and free of any UI, so the TUI and the CLI produce the same turn
// numbers and the same document.
package transcript

import (
	"github.com/agentcarto/core/conversation"
	"github.com/agentcarto/core/domain"
	"time"
)

// Turn is one turn of a branch, in the shape the reader sees it.
type Turn struct {
	// Index is the turn's position among the turns a reader can open. It is the number
	// the TUI shows as "turn #N" (Index+1) and the one an exported document
	// carries. Summary-only compact boundaries are left out before this index is
	// assigned, so public turn numbers are contiguous.
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
func Turns(c domain.Conversation, path []string) []Turn {
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

// Branches counts the lines of conversation that leave the given path: a node
// whose parent is on the path but which is not on it. Each is a rewind or an
// alternative the session took and abandoned, and a document that renders one
// path says nothing about them — a reader who is told "31 turns" would otherwise
// take that for everything that was said.
func Branches(c domain.Conversation, path []string) int {
	onPath := make(map[string]bool, len(path))
	for _, id := range path {
		onPath[id] = true
	}
	n := 0
	for _, id := range path {
		for _, child := range c.Children[id] {
			if !onPath[child] {
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

// TurnTime returns the smallest (earliest) timestamp among a turn's events. It does not
// depend on the events' order, guaranteeing chronological correctness (min of the times).
func TurnTime(events []domain.Event) time.Time {
	var t time.Time
	for _, e := range events {
		if e.Timestamp.IsZero() {
			continue
		}
		if t.IsZero() || e.Timestamp.Before(t) {
			t = e.Timestamp
		}
	}
	return t
}
