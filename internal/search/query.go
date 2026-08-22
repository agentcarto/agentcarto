package search

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/agentcarto/core/domain"
)

// Query is what a search looks for. Two shapes, because two questions get
// asked: a handful of words, all of which a session has to hold, and a pattern
// for everything the words cannot say.
//
// The words are the default because they need no syntax. The pattern exists
// because a corpus written in two languages needs alternation more than
// anything else: in these logs, seven sessions in ten that talk about a cache
// say either "cache" or "キャッシュ" — not both — so a single word finds well
// under half of them.
type Query struct {
	terms []string
	re    *regexp.Regexp
}

// NewQuery reads a query as words, all of which have to be present.
func NewQuery(s string) Query { return Query{terms: Terms(s)} }

// NewRegexpQuery reads a query as a pattern. It is RE2, so there is no
// backtracking to blow up on whatever a caller passes in, and it is compiled
// case-insensitive and multi-line:
//
//   - case-insensitive because the text it runs against has been folded to
//     lower case, so a pattern with capitals would match nothing;
//   - multi-line so that ^ and $ mean the start and end of a line. Sessions are
//     narrowed down against one run of text holding the whole session and hits
//     are located in one message at a time, and only line anchors mean the same
//     thing in both — \A and \z would narrow a session down by its first line
//     and then look for a match at the start of every message.
func NewRegexpQuery(pattern string) (Query, error) {
	re, err := regexp.Compile("(?im)" + pattern)
	if err != nil {
		return Query{}, fmt.Errorf("--regex %q: %w", pattern, err)
	}
	return Query{re: re}, nil
}

// Empty reports whether the query asks for nothing, which every session answers.
func (q Query) Empty() bool { return q.re == nil && len(q.terms) == 0 }

// IsRegexp reports whether the query is a pattern rather than words.
func (q Query) IsRegexp() bool { return q.re != nil }

// matches reports whether the query is satisfied by this text alone.
func (q Query) matches(folded string) bool {
	if q.re != nil {
		return q.re.MatchString(folded)
	}
	for _, t := range q.terms {
		if !strings.Contains(folded, t) {
			return false
		}
	}
	return len(q.terms) > 0
}

// matchesEither is the rule a session is judged by: a pattern has to be found
// in one of the two texts, while every word may be satisfied by either of them
// (the title says one thing, the conversation another).
func (q Query) matchesEither(meta, text string) bool {
	if q.re != nil {
		return q.re.MatchString(meta) || q.re.MatchString(text)
	}
	for _, t := range q.terms {
		if !strings.Contains(meta, t) && !strings.Contains(text, t) {
			return false
		}
	}
	return len(q.terms) > 0
}

// count returns how often the query occurs in the folded text: for several
// words, the occurrences of each of them added together. The sum is a coarse
// measure on purpose — a session that says a word twenty times is about it, and
// which of the words carried the count is not worth the arithmetic to find out.
func (q Query) count(folded string) int {
	if q.re != nil {
		return len(q.re.FindAllStringIndex(folded, -1))
	}
	n := 0
	for _, t := range q.terms {
		n += strings.Count(folded, t)
	}
	return n
}

// present returns how many of the query's terms the folded text holds at all. A
// pattern is one term, so it answers 0 or 1.
func (q Query) present(folded string) int {
	if q.re != nil {
		if q.re.MatchString(folded) {
			return 1
		}
		return 0
	}
	n := 0
	for _, t := range q.terms {
		if strings.Contains(folded, t) {
			n++
		}
	}
	return n
}

// find returns the byte range of the earliest match in the folded text.
func (q Query) find(folded string) (start, end int, ok bool) {
	if q.re != nil {
		if loc := q.re.FindStringIndex(folded); loc != nil {
			return loc[0], loc[1], true
		}
		return 0, 0, false
	}
	start = -1
	for _, t := range q.terms {
		if i := strings.Index(folded, t); i >= 0 && (start < 0 || i < start) {
			start, end = i, i+len(t)
		}
	}
	return start, end, start >= 0
}

// metaText is the session's own searchable text: what it is called, where it
// ran, and what it is.
func metaText(s domain.Session) string {
	return fold(strings.Join([]string{s.Title, s.CWD, s.AgentType, s.PluginID, s.SessionID}, "\n"))
}

// MatchesMetadata reports whether the query is answered by the session's own
// fields rather than by anything that was said in it.
func MatchesMetadata(s domain.Session, q Query) bool { return q.matches(metaText(s)) }
