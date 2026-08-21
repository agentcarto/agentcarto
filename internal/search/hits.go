package search

import (
	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
	"strings"
	"time"
	"unicode/utf8"
)

// Hit is one place a query matched inside a session: which turn it was in, and
// enough text around the match to judge whether the turn is worth reading.
type Hit struct {
	// Turn is the number the TUI shows ("turn #N") and the one `show --turns`
	// takes, so a hit can be followed without a second search.
	Turn int              `json:"turn"`
	Kind domain.EventKind `json:"kind"`
	// omitzero, not omitempty: encoding/json never considers a struct empty, so
	// a session whose plugin records no times would print 0001-01-01 instead.
	Timestamp time.Time `json:"timestamp,omitzero"`
	Snippet   string    `json:"snippet"`
}

// HitOptions bound what a search returns. The defaults are the caller's to set;
// zero means "no limit" for Max and DefaultContext runes for Context.
type HitOptions struct {
	Max     int // most hits to return, newest kept (0: all of them)
	Context int // runes of context kept on either side of the match
}

// DefaultContext is the width of a snippet on either side of the match: enough
// to see the sentence the match sits in, short enough that a page of hits stays
// readable.
const DefaultContext = 120

// Hits locates a query inside a session's turns and returns the newest Max of
// them in chronological order, along with the number of matching events found
// (so a caller can say how many it is not showing). An event that holds the
// query more than once is one hit, cut around its first occurrence, and a query
// of several terms hits an event holding any one of them.
//
// What counts as searchable is IndexText, the same definition the index is built
// from: a message, a task's report, or a tool call's name and one-line argument.
// Injected system messages arrive as user events and are part of it, so they can
// be hit here too, even though a rendered transcript leaves them out. Searching more than the index does would
// produce hits in sessions the index cannot find in the first place, which reads
// as the search being unreliable rather than as the limitation it is.
func Hits(c domain.Conversation, turns []transcript.Turn, q Query, o HitOptions) (hits []Hit, total int) {
	if q.Empty() {
		return nil, 0
	}
	ctx := o.Context
	if ctx <= 0 {
		ctx = DefaultContext
	}
	for _, t := range turns {
		for _, e := range t.Events(c) {
			text, _ := IndexText(e)
			if text == "" {
				continue
			}
			// Folding maps one rune to one rune, so an index into the folded form is
			// an index into the original — which a byte offset is not.
			folded := fold(text)
			from, to, ok := q.find(folded)
			if !ok {
				continue
			}
			at := utf8.RuneCountInString(folded[:from])
			total++
			hits = append(hits, Hit{
				Turn:      t.Index + 1,
				Kind:      e.Kind,
				Timestamp: e.Timestamp,
				Snippet:   snippet(text, at, utf8.RuneCountInString(folded[from:to]), ctx),
			})
		}
	}
	if o.Max > 0 && len(hits) > o.Max {
		hits = hits[len(hits)-o.Max:] // the newest ones: a session's latest turns are what a reader is usually after
	}
	return hits, total
}

// snippet cuts the text around a match down to one line: the match plus ctx
// runes on either side, with the line breaks folded away so a hit occupies one
// line of output. An ellipsis marks each end that was cut.
func snippet(text string, at, length, ctx int) string {
	r := []rune(text)
	start, end := max(0, at-ctx), min(len(r), at+length+ctx)
	out := strings.Join(strings.Fields(string(r[start:end])), " ")
	if start > 0 {
		out = "…" + out
	}
	if end < len(r) {
		out += "…"
	}
	return out
}
