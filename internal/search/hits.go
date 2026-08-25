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

// TableContext is the same for a listing meant to be read on a terminal, where
// a hit has one line and a line that wraps three times costs more than the
// sentence it shows is worth.
const TableContext = 44

// Summary counts what the query was found in across a whole session, of which
// Hits returns only the newest few. It is what a caller needs to tell a session
// that worked on the subject from one that only looked it up.
type Summary struct {
	// Total is how many events hold the query.
	Total int
	// ToolCalls is how many of those events are tool calls — a command that was
	// run, a file that was read. A call that carries file changes is not counted
	// here: it is indexed as the paths it changed, which is work, not a lookup.
	ToolCalls int
	// SelfCalls is how many of those tool calls ran agentcarto itself.
	SelfCalls int
}

// OnlyRanAgentcarto reports that every tool call the query was found in was a
// call to agentcarto. Such a session looked the subject up rather than working
// on it: the search that found it, and the conversation about what it returned.
// A session with no matching tool call at all is not one of these — talking
// about a subject is not the same as searching for it.
func (s Summary) OnlyRanAgentcarto() bool { return s.ToolCalls > 0 && s.SelfCalls == s.ToolCalls }

// Hits locates a query inside a session's turns and returns the newest Max of
// them in chronological order, along with a Summary of everything it found (so
// a caller can say how many it is not showing, and what kind of session it is
// looking at). An event that holds the query more than once is one hit, cut
// around its first occurrence, and a query of several terms hits an event
// holding any one of them.
//
// What counts as searchable is IndexText, the same definition the index is built
// from: a message, a task's report, or a tool call's name and one-line argument.
// Injected system messages arrive as user events and are part of it, so they can
// be hit here too, even though a rendered transcript leaves them out. Searching more than the index does would
// produce hits in sessions the index cannot find in the first place, which reads
// as the search being unreliable rather than as the limitation it is.
func Hits(c domain.Conversation, turns []transcript.Turn, q Query, o HitOptions) (hits []Hit, sum Summary) {
	if q.Empty() {
		return nil, Summary{}
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
			sum.Total++
			// A call carrying file changes is indexed as the paths it changed, so it
			// is counted as the edit it is and not as a call that was made.
			if e.Kind == domain.EventToolCall && len(e.Changes) == 0 {
				sum.ToolCalls++
				if selfCall.MatchString(folded) {
					sum.SelfCalls++
				}
			}
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
	return hits, sum
}

// runesBefore and runeLen count runes in a folded string, which is what snippet
// takes: folding maps one rune to one rune, so a rune offset into the folded
// form is a rune offset into the original — which a byte offset is not.
func runesBefore(folded string, at int) int { return utf8.RuneCountInString(folded[:at]) }
func runeLen(s string) int                  { return utf8.RuneCountInString(s) }

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
