package app

import (
	"context"
	"encoding/json"
	"github.com/agentcarto/agentcarto/internal/catalog"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/search"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"strings"
	"testing"
)

// loaderStub counts how often a session was parsed, which is what the index
// build is meant to avoid.
type loaderStub struct {
	text  string
	loads int
}

func (l *loaderStub) LoadConversation(_ context.Context, _ domain.SessionRef) (*domain.Conversation, error) {
	l.loads++
	c := domain.NewConversation([]domain.ConvNode{{
		ID: "n1",
		// Prompt as well as Text: an injected message (no prompt) is deliberately
		// not indexed, and this stub stands for something a person typed.
		Events: []domain.Event{{Kind: domain.EventUser, Text: l.text, Prompt: l.text}},
	}})
	return &c, nil
}

// storeStub is an in-memory ArtifactStore holding the JSON-ish payload by kind.
type storeStub struct {
	m          map[string]indexArtifact
	gets, puts int
}

func newStore() *storeStub { return &storeStub{m: map[string]indexArtifact{}} }

func (s *storeStub) key(x domain.Session, kind string) string {
	return strings.Join([]string{x.PluginID, x.SessionID, x.Fingerprint, kind}, "|")
}
func (s *storeStub) GetArtifact(_ context.Context, x domain.Session, kind string, dst any) bool {
	s.gets++
	v, ok := s.m[s.key(x, kind)]
	if !ok {
		return false
	}
	*(dst.(*indexArtifact)) = v
	return true
}
func (s *storeStub) PutArtifact(_ context.Context, x domain.Session, kind string, v any) error {
	s.puts++
	s.m[s.key(x, kind)] = v.(indexArtifact)
	return nil
}

func indexApp(impl any) *App {
	return &App{
		Config:  config.Config{Index: config.Index{MaxCharsPerSession: 1 << 16}},
		Catalog: catalog.Catalog{Plugins: []plugin.Instance{{ID: "p", Impl: impl}}},
	}
}

func indexSession() domain.Session {
	return domain.Session{PluginID: "p", SessionID: "s", Fingerprint: "fp1", SourceRef: domain.SessionRef{Source: "/tmp/s.jsonl"}}
}

func TestBuildIndexParsesOnceAndCaches(t *testing.T) {
	l := &loaderStub{text: "handoff order"}
	a := indexApp(l)
	store := newStore()
	s := indexSession()

	idx, fp := a.BuildIndex(context.Background(), []domain.Session{s}, store, nil, nil)
	if !idx.Match(s, search.NewQuery("handoff")) {
		t.Fatal("indexed text should match the query")
	}
	if l.loads != 1 || store.puts != 1 {
		t.Fatalf("first build: loads=%d puts=%d", l.loads, store.puts)
	}
	if fp[s.SourceRef.Source] != "fp1" {
		t.Fatalf("fingerprint not reported: %#v", fp)
	}

	// A fresh build with no previous index must come from the artifact cache.
	idx2, _ := a.BuildIndex(context.Background(), []domain.Session{s}, store, nil, nil)
	if l.loads != 1 {
		t.Fatalf("cached artifact should not be reparsed: loads=%d", l.loads)
	}
	if !idx2.Match(s, search.NewQuery("handoff")) {
		t.Fatal("cached entry should still match")
	}
}

func TestBuildIndexReusesPreviousEntryForUnchangedSession(t *testing.T) {
	l := &loaderStub{text: "handoff order"}
	a := indexApp(l)
	store := newStore()
	s := indexSession()

	prev, prevFP := a.BuildIndex(context.Background(), []domain.Session{s}, store, nil, nil)
	gets := store.gets
	idx, _ := a.BuildIndex(context.Background(), []domain.Session{s}, store, prev, prevFP)
	if store.gets != gets || l.loads != 1 {
		t.Fatalf("unchanged session should touch neither cache nor parser: gets=%d loads=%d", store.gets, l.loads)
	}
	if !idx.Match(s, search.NewQuery("handoff")) {
		t.Fatal("reused entry should still match")
	}

	// A changed fingerprint invalidates both the previous entry and the cached one.
	changed := s
	changed.Fingerprint = "fp2"
	l.text = "relocate plan"
	idx, _ = a.BuildIndex(context.Background(), []domain.Session{changed}, store, prev, prevFP)
	if l.loads != 2 {
		t.Fatalf("changed session should be reparsed: loads=%d", l.loads)
	}
	if idx.Match(changed, search.NewQuery("handoff")) || !idx.Match(changed, search.NewQuery("relocate")) {
		t.Fatal("index should hold the new text only")
	}
}

// The artifact is stored as JSON under a kind that is part of the on-disk
// format: renaming either silently invalidates every cache entry a user already
// has, and the tests above would stay green because they write and read through
// the same struct.
func TestIndexArtifactFormatIsFixed(t *testing.T) {
	if SearchArtifactKind != "search-v6" {
		t.Fatalf("kind=%q: changing it discards every cached index entry", SearchArtifactKind)
	}
	b, err := json.Marshal(indexArtifact{Text: "folded text", Count: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"Text":"folded text","Count":3}`; got != want {
		t.Fatalf("artifact JSON=%s want %s", got, want)
	}
}

func TestBuildIndexWithoutStoreOrLoader(t *testing.T) {
	l := &loaderStub{text: "handoff"}
	a := indexApp(l)
	s := indexSession()
	if idx, _ := a.BuildIndex(context.Background(), []domain.Session{s}, nil, nil, nil); !idx.Match(s, search.NewQuery("handoff")) {
		t.Fatal("a nil store must still index by parsing")
	}

	// A plugin that cannot load conversations is skipped: no entry, no fingerprint.
	a = indexApp(struct{}{})
	idx, fp := a.BuildIndex(context.Background(), []domain.Session{s}, nil, nil, nil)
	if _, ok := idx.Count(s); ok {
		t.Fatal("a session without a conversation loader should not be indexed")
	}
	if len(fp) != 0 {
		t.Fatalf("no fingerprint should be reported: %#v", fp)
	}
}
