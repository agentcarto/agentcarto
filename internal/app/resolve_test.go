package app

import (
	"github.com/agentcarto/core/domain"
	"strings"
	"testing"
)

func withID(plugin, id, source string) domain.Session {
	return domain.Session{PluginID: plugin, SessionID: id, SourceRef: domain.SessionRef{Source: source}}
}

func TestResolveSessionByPrefix(t *testing.T) {
	sessions := []domain.Session{
		withID("claude", "8f3a2b1c-4d5e", "/logs/a.jsonl"),
		withID("codex", "91b0ffee-0000", "/logs/b.jsonl"),
	}
	got, err := ResolveSession(sessions, "8f3a")
	if err != nil || got.SessionID != "8f3a2b1c-4d5e" {
		t.Fatalf("prefix lookup=%v %v", got.SessionID, err)
	}
	if _, err := ResolveSession(sessions, "nope"); err == nil {
		t.Fatal("an unknown id was resolved")
	}
	if _, err := ResolveSession(sessions, "  "); err == nil {
		t.Fatal("a blank reference was resolved")
	}
}

// A full id wins over a prefix: two sessions can share a prefix, and naming one
// exactly must not be called ambiguous.
func TestResolveSessionPrefersTheExactID(t *testing.T) {
	sessions := []domain.Session{
		withID("claude", "8f3a", "/logs/a.jsonl"),
		withID("claude", "8f3a2b1c", "/logs/b.jsonl"),
	}
	got, err := ResolveSession(sessions, "8f3a")
	if err != nil || got.SourceRef.Source != "/logs/a.jsonl" {
		t.Fatalf("exact match lost to the prefix: %v %v", got, err)
	}
}

// An ambiguous reference names the candidates instead of picking one: the same
// id can appear twice (a fork keeps its parent's).
func TestResolveSessionAmbiguityNamesTheCandidates(t *testing.T) {
	sessions := []domain.Session{
		withID("claude", "8f3a2b1c", "/logs/a.jsonl"),
		withID("claude", "8f3a9999", "/logs/b.jsonl"),
	}
	_, err := ResolveSession(sessions, "8f3a")
	if err == nil {
		t.Fatal("an ambiguous prefix was resolved")
	}
	for _, want := range []string{"matches 2 sessions", "/logs/a.jsonl", "/logs/b.jsonl", "--source"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestResolveSource(t *testing.T) {
	sessions := []domain.Session{
		withID("claude", "8f3a2b1c", "/logs/a.jsonl"),
		withID("claude", "8f3a2b1c", "/logs/b.jsonl"), // a fork keeping its parent's id
	}
	got, err := ResolveSource(sessions, "/logs/./b.jsonl")
	if err != nil || got.SourceRef.Source != "/logs/b.jsonl" {
		t.Fatalf("source lookup=%v %v", got, err)
	}
	if _, err := ResolveSource(sessions, "/logs/c.jsonl"); err == nil {
		t.Fatal("an unknown path was resolved")
	}
}

func TestResolveSessionFoldsCase(t *testing.T) {
	sessions := []domain.Session{withID("copilot-vc", "AB12CD34", "/logs/a.json")}
	if got, err := ResolveSession(sessions, "ab12"); err != nil || got.SessionID != "AB12CD34" {
		t.Fatalf("case-folded lookup=%v %v", got, err)
	}
}

// Sessions really can share an id — codex writes a rollout per resume, and a
// fork keeps its parent's — and then "name it in full" is no advice at all.
func TestResolveSessionSaysWhenTheIDItselfIsShared(t *testing.T) {
	sessions := []domain.Session{
		withID("codex", "019ee0a5", "/logs/a.jsonl"),
		withID("codex", "019ee0a5", "/logs/b.jsonl"),
	}
	_, err := ResolveSession(sessions, "019ee0a5")
	if err == nil {
		t.Fatal("a shared id was resolved")
	}
	if strings.Contains(err.Error(), "in full") {
		t.Errorf("the advice cannot be to name it in full: %v", err)
	}
	if !strings.Contains(err.Error(), "share this id") || !strings.Contains(err.Error(), "--source") {
		t.Errorf("the advice should point at --source: %v", err)
	}
}
