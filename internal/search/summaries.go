package search

import "sort"

// SummaryHits locates a query inside a session's generated summaries.
//
// Summaries are searched apart from the conversation, and only ever as a last
// resort: they are a paraphrase, and a session that says a thing outright is a
// better answer than one a model said it about. What they are for is the
// session the conversation can no longer answer for — one whose log an agent
// rotated away and whose cached conversation the store has since evicted. A
// kilobyte of summary is what outlives both, and without this it is searchable
// by its title alone.
//
// total is how many summaries hold the query, of which Max are returned — the
// same split Hits reports, so a caller can say what it is not showing.
//
// texts is keyed the way a document wants summaries: turn number, with 0 for the
// session's own. The caller decides which turns it can vouch for — a summary is
// held against the id of the turn it was made from, and a caller with no
// conversation to check that against passes the session summary alone.
func SummaryHits(texts map[int]string, q Query, o HitOptions) (hits []Hit, total int) {
	if q.Empty() || len(texts) == 0 {
		return nil, 0
	}
	ctx := o.Context
	if ctx <= 0 {
		ctx = DefaultContext
	}
	turns := make([]int, 0, len(texts))
	for n := range texts {
		turns = append(turns, n)
	}
	sort.Ints(turns)

	for _, n := range turns {
		text := texts[n]
		if text == "" {
			continue
		}
		folded := fold(text)
		from, to, ok := q.find(folded)
		if !ok {
			continue
		}
		hits = append(hits, Hit{
			Turn:    n,
			Snippet: snippet(text, runesBefore(folded, from), runeLen(folded[from:to]), ctx),
		})
	}
	// The session summary describes the whole session and the turn summaries
	// describe parts of it, so the general one goes first — but a listing is cut
	// from the end, and the turns are what a reader follows. Keeping the newest
	// matches the rule Hits uses for a conversation.
	total = len(hits)
	if o.Max > 0 && len(hits) > o.Max {
		hits = hits[len(hits)-o.Max:]
	}
	return hits, total
}
