package search

import (
	"github.com/agentcarto/core/domain"
	"testing"
)

func TestMatchMetadataUnicode(t *testing.T) {
	// The Japanese title and query are intentional test data: they exercise
	// matching over multibyte (non-ASCII) runes, so they are kept as-is.
	i := New(100)
	s := domain.Session{PluginID: "codex", Title: "日本語の題名", CWD: "/work"}
	if !i.Match(s, "日本語") {
		t.Fatal("expected Unicode match")
	}
	if i.Match(s, "claude") {
		t.Fatal("unexpected match")
	}
}

func TestMatchSessionID(t *testing.T) {
	i := New(100)
	s := domain.Session{PluginID: "codex", Title: "title", SessionID: "abc123-def456"}
	if !i.Match(s, "def456") {
		t.Fatal("expected SessionID match")
	}
}
