package transcript

import (
	"fmt"
	"github.com/agentcarto/core/conversation"
	"github.com/agentcarto/core/domain"
	"strings"
)

// headlineWidth is where a turn's headline is cut in an outline. It is there to
// tell turns apart, not to be read in place of the turn.
const headlineWidth = 100

// Outline renders a session's table of contents: the same header a document
// carries, then one line per turn with the number `Markdown` and the TUI both
// use, and the size that turn would print at under the given options. It is what a reader asks for first — a whole session can be hundreds of
// kilobytes, and this says which parts are worth opening.
func Outline(s domain.Session, c domain.Conversation, turns []Turn, o Options) string {
	lines := header(s, len(turns), o)
	for _, t := range turns {
		entry := fmt.Sprintf("- Turn %d", t.Index+1)
		if ts := TurnTime(t.Events(c)); !ts.IsZero() {
			entry += " (" + ts.Format(TimeFormat) + ")"
		}
		// The size the turn would print at, under the same options: one turn can
		// be a paragraph or forty kilobytes, and whoever is choosing which to open
		// (an agent paying for its context above all) cannot tell from the
		// headline alone.
		entry += " " + size(turnLines(c, t, s.CWD, o))
		if t.Compact {
			entry += " »" // the context was compacted at this turn's boundary
		}
		if h := headline(c, t); h != "" {
			entry += " — " + h
		}
		lines = append(lines, entry)
	}
	return strings.Join(lines, "\n")
}

// size renders how much text the turn would print, rounded to something a
// reader can weigh at a glance.
func size(lines []string) string {
	n := 0
	for _, ln := range lines {
		n += len(ln) + 1
	}
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("[%.1f MB]", float64(n)/(1<<20))
	case n >= 1024:
		return fmt.Sprintf("[%.1f KB]", float64(n)/1024)
	}
	return fmt.Sprintf("[%d B]", n)
}

// headline is the turn's first prompt (or, failing that, what the agent said),
// folded onto one line and cut to a width that keeps the outline scannable.
func headline(c domain.Conversation, t Turn) string {
	h := strings.Join(strings.Fields(conversation.TurnHeadline(c, t.Nodes)), " ")
	r := []rune(h)
	if len(r) > headlineWidth {
		return string(r[:headlineWidth]) + "…"
	}
	return h
}
