package search

import (
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

// linear builds a single-chain conversation, one event per node.
func linear(events ...domain.Event) *domain.Conversation {
	nodes := make([]domain.ConvNode, 0, len(events))
	parent := ""
	base := time.Unix(0, 0)
	for i, e := range events {
		id := "n" + string(rune('0'+i))
		e.Timestamp = base.Add(time.Duration(i) * time.Second)
		nodes = append(nodes, domain.ConvNode{ID: id, Parent: parent, Timestamp: e.Timestamp, Events: []domain.Event{e}})
		parent = id
	}
	c := domain.NewConversation(nodes)
	return &c
}

func sessionAt(source string) domain.Session {
	s := domain.Session{}
	s.SourceRef.Source = source
	return s
}

func TestBuildCountsAllButTruncatesText(t *testing.T) {
	conv := linear(
		domain.Event{Kind: domain.EventUser, Text: "alpha", Prompt: "alpha"},
		domain.Event{Kind: domain.EventAssistant, Text: "beta"},
		domain.Event{Kind: domain.EventMeta, Text: "ignored"}, // not a counted kind
		domain.Event{Kind: domain.EventUser, Text: "gamma", Prompt: "gamma"},
	)
	// A budget with room for the first message and nothing after it. (It used to
	// be 1 byte, back when the first event was written whole however large it
	// was; the budget is a real bound now, so the fixture has to leave room for
	// what it expects to find.)
	i := New(len("alpha\n"))
	s := sessionAt("/s/1")
	i.Add(s, *conv)
	// Count reflects every qualifying (user/assistant) event, even those past the
	// text budget; meta is not counted.
	if n, ok := i.Count(s); !ok || n != 3 {
		t.Errorf("count = %d (ok=%v), want 3", n, ok)
	}
	if !i.Match(s, NewQuery("alpha")) {
		t.Error("first message should be searchable")
	}
	if i.Match(s, NewQuery("gamma")) {
		t.Error("text beyond MaxChars must not be indexed")
	}
}

func TestMatch(t *testing.T) {
	i := New(100)
	s := domain.Session{Title: "Fix bug", CWD: "/home/work", AgentType: "claude", PluginID: "claude-plugin", SessionID: "id-1"}
	i.Set(s, fold("the conversation body"), 2)

	cases := []struct {
		q    string
		want bool
	}{
		{"", true},           // empty query matches everything
		{"   ", true},        // whitespace-only too
		{"FIX", true},        // title, case-insensitive
		{"/home/work", true}, // cwd
		{"claude", true},     // agent type / plugin id
		{"body", true},       // conversation text
		{"missing", false},
	}
	for _, c := range cases {
		if got := i.Match(s, NewQuery(c.q)); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.q, got, c.want)
		}
	}
}

func TestSetLookupAndCount(t *testing.T) {
	i := New(100)
	s := sessionAt("/s/x")
	if _, ok := i.Count(s); ok {
		t.Error("unindexed session should report ok=false")
	}
	i.Set(s, "folded text", 7)
	text, count, ok := i.Lookup(s)
	if !ok || text != "folded text" || count != 7 {
		t.Errorf("Lookup = %q,%d,%v", text, count, ok)
	}
}

func TestCopyFrom(t *testing.T) {
	src := New(100)
	s := sessionAt("/s/c")
	src.Set(s, "x", 5)

	dst := New(100)
	if !dst.CopyFrom(src, s) {
		t.Fatal("CopyFrom should succeed for a present entry")
	}
	if _, count, _ := dst.Lookup(s); count != 5 {
		t.Errorf("copied count = %d, want 5", count)
	}
	if dst.CopyFrom(src, sessionAt("/s/absent")) {
		t.Error("CopyFrom should fail for an absent entry")
	}
	if dst.CopyFrom(nil, s) {
		t.Error("CopyFrom(nil) should be false")
	}
}

func TestMaxCount(t *testing.T) {
	i := New(100)
	if i.MaxCount() != 0 {
		t.Error("empty index MaxCount should be 0")
	}
	i.Set(sessionAt("/a"), "x", 3)
	i.Set(sessionAt("/b"), "y", 9)
	i.Set(sessionAt("/c"), "z", 1)
	if got := i.MaxCount(); got != 9 {
		t.Errorf("MaxCount = %d, want 9", got)
	}
}
