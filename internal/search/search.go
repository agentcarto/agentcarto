package search

import (
	"github.com/agentcarto/core/domain"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type entry struct {
	text  string
	count int
}

// Index holds the conversation text and message count per session. The key is
// SourceRef.Source (the file/directory path), which is unique per session.
// SessionKey(plugin, session_id) is deliberately not used because it can collide
// across a fork child and its parent, codex resume rollouts, grok subconversations,
// and similar cases.
type Index struct {
	m map[string]entry
	// MaxChars caps how much of a session is indexed. It is counted in bytes, so
	// Japanese text reaches it at roughly a third of the character count.
	MaxChars int
}

func New(max int) *Index { return &Index{m: map[string]entry{}, MaxChars: max} }

func id(s domain.Session) string { return s.SourceRef.Source }

// cutBytes shortens s to at most n bytes, at a rune boundary so the folded text
// stays valid UTF-8.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cut shortens s to at most n runes.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
func fold(s string) string { return strings.Map(unicode.ToLower, s) }

// Add indexes a conversation the caller has already loaded. Loading is the
// caller's business because one parse now serves two purposes — the index and
// the cached copy of the conversation — and a session is expensive enough to
// parse that doing it twice would be felt.
func (i *Index) Add(s domain.Session, c domain.Conversation) {
	var b strings.Builder
	count := 0
	for _, nid := range c.ActivePath() {
		for _, ev := range c.Nodes[nid].Events {
			text, message := IndexText(ev)
			if message {
				count++ // the message count the list shows, capped by nothing
			}
			room := i.MaxChars - b.Len()
			if text == "" || room <= 0 {
				continue
			}
			// Cut to what is left of the budget rather than skipping only whole
			// events: one 5 MB reply would otherwise be written in full, because
			// the room was checked before it and not against its size.
			b.WriteString(cutBytes(text, room))
			b.WriteByte('\n')
		}
	}
	i.m[id(s)] = entry{fold(b.String()), count}
}

// toolTextLimit is how much of a tool call is indexed, in runes.
const toolTextLimit = 300

// IndexText returns the text of an event that search covers, and whether the
// event is a message (the count the session list shows). It is the single
// definition of what "searchable" means: Build indexes exactly this, Hits looks
// for the query in exactly this, and a session can therefore never match the
// index in text that no hit can point at.
//
// Tool calls are covered by their name and one-line argument — the command that
// was run, the file that was read. Their expanded body (ToolDetail) is not: a
// heredoc writing a file would put the whole file in the index. Tool output,
// reasoning and file diffs are not covered either.
func IndexText(e domain.Event) (text string, message bool) {
	switch e.Kind {
	case domain.EventUser:
		// A user event without a normalized prompt is something the agent was
		// handed, not something a person said: a system reminder, an injected
		// file, a grok session's preamble. A transcript leaves those out, so
		// indexing them makes the search point at turns where the words cannot be
		// found — and, with a preamble repeated in every session of an agent,
		// matches everything. It still counts as a message.
		if e.Prompt == "" {
			return "", true
		}
		return e.Text, true
	case domain.EventQueued, domain.EventAssistant:
		return e.Text, true
	case domain.EventTask:
		// Task notices were once user events; their summaries and results stay
		// searchable. The normalized body is indexed, not the notification wrapper.
		if e.ToolDetail != "" {
			return e.ToolDetail, true
		}
		return e.Text, true
	case domain.EventToolCall:
		// A call that carries file changes is shown as the turn's edited-file
		// section and not as a call of its own, so that is what gets indexed:
		// searching "apply_patch" should not point at a turn that never says it,
		// and searching a path should find the sessions that changed the file.
		if len(e.Changes) > 0 {
			return changedPaths(e), false
		}
		// Cut short: a plugin folds a whole heredoc into ToolArg to fit it on one
		// terminal row, so an unbounded argument puts the file being written into
		// the index — a 14x growth on the sessions that write files this way, and
		// a "hit" that is a page of file content. What identifies a call (its name,
		// the command, the path) is at the front.
		return cut(strings.TrimSpace(e.ToolName+" "+e.ToolArg), toolTextLimit), false
	case domain.EventFileChange:
		return changedPaths(e), false
	}
	return "", false
}

// changedPaths lists the files an event changed, in the form the turn's
// edited-file section shows them, so that what is searchable is what is read.
func changedPaths(e domain.Event) string {
	var b strings.Builder
	for _, c := range e.Changes {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(c.Path)
	}
	return b.String()
}

// Terms splits a query into the words a session has to hold, folded for
// comparison. A query of several words means all of them: someone looking for
// "fork relocate" is naming two things they remember from one session, not a
// phrase that was typed that way. Nothing here is a phrase search — the terms
// may sit anywhere in the session.
func Terms(q string) []string { return strings.Fields(fold(q)) }

// Match reports whether the session answers the query, in what was said in it
// or in the session's own metadata (its title, working directory, agent or id).
func (i *Index) Match(s domain.Session, q Query) bool {
	if q.Empty() {
		return true
	}
	return q.matchesEither(metaText(s), i.m[id(s)].text)
}

// selfName is the command's own name. A session whose only matching tool calls
// are calls to agentcarto searched for the subject instead of working on it,
// and every use of the search leaves another such session behind — so the name
// has to be recognizable here.
const selfName = "agentcarto"

// selfCall matches a tool call that ran agentcarto: the name — however it was
// spelled on the command line, bare or as a path — followed by one of its
// subcommands, with any global flags in between.
//
// The bare name is not enough. This project's own files live under directories
// called agentcarto, so reading one would count as a lookup, and a search for
// one of its paths would then be told there is nothing: every hit it found was
// "a call to agentcarto". Requiring the shape of a command run is what separates
// looking something up from working on it.
var selfCall = regexp.MustCompile(`(^|[\s/\\])` + selfName + `(\.exe)?\s+(-{1,2}[a-z-]+\s+)*(search|show|list|active)\b`)

// IsSelfQuery reports whether the query is looking for agentcarto itself. Then
// the sessions that ran it are what is being asked for, and the rule that hides
// them has to stand down: applied to this query it would answer that there is
// nothing, having quietly discarded everything.
// One term naming it is enough: "agentcarto handoff" is looking for the
// sessions that worked on this command, and hiding the ones that ran it would
// leave the better half of the answer out.
func IsSelfQuery(q Query) bool { return q.present(selfName) > 0 }

// metaTermScore is what a term found in the session's own fields is worth: ten
// hits. A session whose title or working directory names what is being looked
// for is about it, which a handful of passing mentions elsewhere should not
// outrank — but a session that discusses it at length should.
const metaTermScore = 10

// MetaScore rewards the terms the session's own fields answer (its title,
// working directory, agent or id). It is the part of a session's relevance that
// does not depend on reading the conversation, so it is added both to the index
// score below and to the rank a caller computes from the hits it found.
func MetaScore(s domain.Session, q Query) int {
	if q.Empty() {
		return 0
	}
	return metaTermScore * q.present(metaText(s))
}

// Score rates how well the session answers the query, to decide which sessions
// are worth opening. It is the number of times the query occurs in the indexed
// text, plus MetaScore.
//
// It is a coarse measure: the index holds a session as one run of text, so a
// single message repeating a word outweighs ten turns that each mention it
// once. What it is good for is separating the sessions that are about the
// subject from the ones that mention it, without opening any of them.
//
// The count is not divided by the length of the session. A long session that
// keeps coming back to a subject is the one worth reading, and normalizing it
// away would put a passing mention in a short session on top.
//
// The score is read from the index, so it stops at MaxChars like everything
// else here: past that point a session cannot raise its score, which is the
// same limit that decides whether it is found at all.
func (i *Index) Score(s domain.Session, q Query) int {
	if q.Empty() {
		return 0
	}
	return q.count(i.m[id(s)].text) + MetaScore(s, q)
}

// Count returns the session's message count (ok=true if it has been indexed).
func (i *Index) Count(s domain.Session) (int, bool) {
	e, ok := i.m[id(s)]
	return e.count, ok
}

// Set injects the text and message count read from the cache (text is assumed
// to already be folded).
func (i *Index) Set(s domain.Session, text string, count int) {
	i.m[id(s)] = entry{text, count}
}

// Lookup returns the indexed text and message count (for persisting to the cache).
func (i *Index) Lookup(s domain.Session) (text string, count int, ok bool) {
	e, ok := i.m[id(s)]
	return e.text, e.count, ok
}

// CopyFrom imports the entry for the same session from other (reuse of an
// unchanged session).
func (i *Index) CopyFrom(other *Index, s domain.Session) bool {
	if other == nil {
		return false
	}
	if e, ok := other.m[id(s)]; ok {
		i.m[id(s)] = e
		return true
	}
	return false
}

// MaxCount returns the largest message count across all entries (used to compute
// the column width in the list view).
func (i *Index) MaxCount() int {
	mx := 0
	for _, e := range i.m {
		if e.count > mx {
			mx = e.count
		}
	}
	return mx
}
