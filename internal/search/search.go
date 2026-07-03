package search

import (
	"context"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"strings"
	"unicode"
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
	m        map[string]entry
	MaxChars int
}

func New(max int) *Index { return &Index{m: map[string]entry{}, MaxChars: max} }

func id(s domain.Session) string { return s.SourceRef.Source }
func fold(s string) string       { return strings.Map(unicode.ToLower, s) }

func (i *Index) Build(ctx context.Context, s domain.Session, l plugin.ConversationLoader) error {
	c, e := l.LoadConversation(ctx, s.SourceRef)
	if e != nil {
		return e
	}
	var b strings.Builder
	count := 0
	for _, nid := range c.ActivePath() {
		for _, ev := range c.Nodes[nid].Events {
			// EventTask is included because task notices were previously user
			// events; their summaries/results stay searchable. Index the
			// normalized body rather than the raw notification wrapper.
			if ev.Kind == domain.EventUser || ev.Kind == domain.EventQueued || ev.Kind == domain.EventAssistant || ev.Kind == domain.EventTask {
				count++
				if b.Len() >= i.MaxChars {
					break
				}
				text := ev.Text
				if ev.Kind == domain.EventTask && ev.ToolDetail != "" {
					text = ev.ToolDetail
				}
				b.WriteString(text)
				b.WriteByte('\n')
			}
		}
	}
	i.m[id(s)] = entry{fold(b.String()), count}
	return nil
}

func (i *Index) Match(s domain.Session, q string) bool {
	q = fold(strings.TrimSpace(q))
	if q == "" {
		return true
	}
	meta := fold(strings.Join([]string{s.Title, s.CWD, s.AgentType, s.PluginID, s.SessionID}, "\n"))
	return strings.Contains(meta, q) || strings.Contains(i.m[id(s)].text, q)
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
