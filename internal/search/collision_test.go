package search

import (
	"github.com/agentcarto/core/domain"
	"testing"
)

// Two sessions whose Key()(plugin, session_id) collides (such as a fork child and
// its parent) must keep independent index entries as long as their Source differs.
// Previously, keying on map[SessionKey] meant one entry overwrote the other.
func TestIndexDistinguishesSameKeyDifferentSource(t *testing.T) {
	parent := domain.Session{PluginID: "claude", SessionID: "dup", SourceRef: domain.SessionRef{Source: "/p/parent.jsonl"}}
	fork := domain.Session{PluginID: "claude", SessionID: "dup", SourceRef: domain.SessionRef{Source: "/p/subagents/fork.jsonl"}}

	i := New(1 << 20)
	i.Set(parent, "alpha needle", 3)
	i.Set(fork, "beta haystack", 7)

	// Bodies do not bleed into each other.
	if !i.Match(parent, "alpha") || i.Match(parent, "beta") {
		t.Fatal("parent index leaked into/from fork")
	}
	if !i.Match(fork, "beta") || i.Match(fork, "alpha") {
		t.Fatal("fork index leaked into/from parent")
	}
	// Message counts are independent too.
	if n, _ := i.Count(parent); n != 3 {
		t.Fatalf("parent count=%d want 3", n)
	}
	if n, _ := i.Count(fork); n != 7 {
		t.Fatalf("fork count=%d want 7", n)
	}

	// CopyFrom reuses entries keyed by Source.
	j := New(1 << 20)
	if !j.CopyFrom(i, parent) || !j.CopyFrom(i, fork) {
		t.Fatal("CopyFrom should reuse both entries")
	}
	if n, _ := j.Count(fork); n != 7 {
		t.Fatalf("copied fork count=%d want 7", n)
	}
}
