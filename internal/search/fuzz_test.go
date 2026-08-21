package search

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentcarto/core/domain"
)

// FuzzHits drives the whole locate-and-cut path with arbitrary text and
// queries. What it asserts is what the callers rely on: a hit's snippet is text
// that was really there (once whitespace is folded), it is valid UTF-8, and the
// turn it names exists.
func FuzzHits(f *testing.F) {
	f.Add("handoff の順序は？", "handoff", 5)
	f.Add("", "", 0)
	f.Add("aaaa", "a", 1)
	f.Add("日本語のテキスト", "本語", 3)
	f.Add("\x00\x01 control", "control", 2)
	f.Add(strings.Repeat("あ", 500), "あ", 1000)
	f.Fuzz(func(t *testing.T, text, query string, ctx int) {
		if !utf8.ValidString(text) || !utf8.ValidString(query) {
			t.Skip() // the plugins hand us decoded JSON, which is valid UTF-8
		}
		c := domain.NewConversation([]domain.ConvNode{
			{ID: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: text, Prompt: text}}},
		})
		turns := turnsOf(c)
		hits, total := Hits(c, turns, NewQuery(query), HitOptions{Context: ctx})
		if total < len(hits) {
			t.Fatalf("total=%d is less than the %d hits returned", total, len(hits))
		}
		folded := strings.Join(strings.Fields(text), " ")
		for _, h := range hits {
			if h.Turn < 1 {
				t.Fatalf("turn=%d", h.Turn)
			}
			if !utf8.ValidString(h.Snippet) {
				t.Fatalf("snippet is not valid UTF-8: %q", h.Snippet)
			}
			core := strings.Trim(h.Snippet, "…")
			if core != "" && !strings.Contains(folded, core) {
				t.Fatalf("snippet %q is not in the text it was cut from", core)
			}
		}
		// A single-term query that Hits located must also be one Match accepts, or
		// the search would list a session it can find no hits in — or worse, hold
		// hits in a session it never reaches. (With several terms the two answer
		// different questions on purpose: a session has to hold every term, while
		// one event only has to hold one of them.)
		if len(hits) > 0 && len(Terms(query)) == 1 {
			i := New(1 << 20)
			i.Set(domain.Session{}, fold(text), 1)
			if !i.Match(domain.Session{}, NewQuery(query)) {
				t.Fatalf("Hits found %q but Match did not", query)
			}
		}
	})
}

// FuzzIndexText asserts the one rule the index depends on: what it returns is a
// substring of what the event holds (so a hit can always be located again) and
// no longer than the tool limit.
func FuzzIndexText(f *testing.F) {
	f.Add("Bash", "$ go test ./...", "go test ./...", "prompt")
	f.Add("", "", "", "")
	f.Add("apply_patch", strings.Repeat("x", 1000), "", "")
	f.Fuzz(func(t *testing.T, name, arg, detail, prompt string) {
		// Text reaches a plugin through a JSON decoder, so it is always valid
		// UTF-8 by the time it gets here; invalid bytes are folded to U+FFFD when
		// the index is built, and json.Marshal does the same on the way out.
		for _, s := range []string{name, arg, detail, prompt} {
			if !utf8.ValidString(s) {
				t.Skip()
			}
		}
		for _, kind := range []domain.EventKind{domain.EventUser, domain.EventAssistant, domain.EventQueued, domain.EventTask, domain.EventToolCall, domain.EventFileChange} {
			e := domain.Event{Kind: kind, Text: detail, ToolName: name, ToolArg: arg, ToolDetail: detail, Prompt: prompt}
			text, _ := IndexText(e)
			if !utf8.ValidString(text) {
				t.Fatalf("%s: indexed text is not valid UTF-8", kind)
			}
			if kind == domain.EventToolCall && len([]rune(text)) > toolTextLimit {
				t.Fatalf("tool call indexed %d runes", len([]rune(text)))
			}
		}
	})
}
