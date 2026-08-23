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
// Blobs are kept apart because they are whole conversations rather than the
// index payload, and because what a test usually asks is whether one was
// written at all.
type storeStub struct {
	m          map[string]indexArtifact
	blobs      map[string]any
	gets, puts int
	// size is what the store claims to occupy; a test raises it to stand for a
	// cache that has reached its limit.
	size int64
}

func newStore() *storeStub {
	return &storeStub{m: map[string]indexArtifact{}, blobs: map[string]any{}}
}

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
func (s *storeStub) HasArtifact(_ context.Context, x domain.Session, kind string) bool {
	_, ok := s.blobs[s.key(x, kind)]
	return ok
}
func (s *storeStub) PutBlob(_ context.Context, x domain.Session, kind string, v any) error {
	s.blobs[s.key(x, kind)] = v
	return nil
}
func (s *storeStub) Size() int64 { return s.size }

func indexApp(impl any) *App {
	return &App{
		Config: config.Config{
			Index: config.Index{MaxCharsPerSession: 1 << 16},
			Cache: config.Cache{MaxSize: 1 << 20},
		},
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

// The conversation is kept so a session can outlive its log. The catch is that
// an already-indexed session is never parsed again, so storing it only "when a
// conversation happens to be loaded" would store nothing at all for every
// session already in the cache — and that would only be discovered the day a log
// was deleted and could not be read back.
func TestBuildIndexKeepsTheConversation(t *testing.T) {
	l := &loaderStub{text: "handoff order"}
	a := indexApp(l)
	a.Config.Cache.CacheConversations = true
	store := newStore()
	s := indexSession()

	a.BuildIndex(context.Background(), []domain.Session{s}, store, nil, nil)
	if len(store.blobs) != 1 {
		t.Fatalf("the conversation should have been stored: %d blobs", len(store.blobs))
	}

	// Indexed and stored: there is nothing left to parse it for.
	a.BuildIndex(context.Background(), []domain.Session{s}, store, nil, nil)
	if l.loads != 1 {
		t.Errorf("loads=%d want 1: neither the index nor the conversation was missing", l.loads)
	}

	// Indexed but not stored — which is what every existing session looks like the
	// first time this runs. The session is parsed again for the conversation alone.
	store.blobs = map[string]any{}
	a.BuildIndex(context.Background(), []domain.Session{s}, store, nil, nil)
	if l.loads != 2 || len(store.blobs) != 1 {
		t.Errorf("an indexed session with no stored conversation should be parsed for it: loads=%d blobs=%d", l.loads, len(store.blobs))
	}
}

// The setting has been in the configuration since the beginning without doing
// anything. Now that it does, off has to mean off.
func TestBuildIndexHonoursCacheConversations(t *testing.T) {
	l := &loaderStub{text: "handoff"}
	a := indexApp(l) // cache_conversations defaults to false in this fixture
	store := newStore()
	a.BuildIndex(context.Background(), []domain.Session{indexSession()}, store, nil, nil)
	if len(store.blobs) != 0 {
		t.Errorf("nothing should be stored when the setting is off: %d blobs", len(store.blobs))
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

// A session whose log is gone has nothing for the plugin to load, and asking
// anyway costs one failed read per deleted session on every scan. What the cache
// kept is used as it is.
func TestBuildIndexDoesNotAskThePluginForADeletedLog(t *testing.T) {
	l := &loaderStub{text: "handoff order"}
	a := indexApp(l)
	a.Config.Cache.CacheConversations = true
	store := newStore()
	s := indexSession()

	// Index it while the log is still there, then let the log disappear.
	a.BuildIndex(context.Background(), []domain.Session{s}, store, nil, nil)
	loads := l.loads
	gone := s
	gone.LogDeleted = true

	idx, fp := a.BuildIndex(context.Background(), []domain.Session{gone}, store, nil, nil)
	if l.loads != loads {
		t.Errorf("the plugin was asked for a log that is gone: loads=%d want %d", l.loads, loads)
	}
	if !idx.Match(gone, search.NewQuery("handoff")) {
		t.Error("the cached index should still answer for a deleted log")
	}
	if fp[gone.SourceRef.Source] != gone.Fingerprint {
		t.Errorf("the fingerprint should still be reported: %#v", fp)
	}
}

// A cache at its limit stops taking conversations. Storing into a full cache
// means the next run evicts one and reparses the session to store it again, and
// then the run after that — which is what made a search take five seconds every
// time instead of one.
func TestBuildIndexStopsStoringWhenTheCacheIsFull(t *testing.T) {
	l := &loaderStub{text: "handoff order"}
	a := indexApp(l)
	a.Config.Cache.CacheConversations = true
	store := newStore()
	store.size = int64(a.Config.Cache.MaxSize) // no room left
	s := indexSession()

	a.BuildIndex(context.Background(), []domain.Session{s}, store, nil, nil)
	if len(store.blobs) != 0 {
		t.Errorf("a full cache should not take the conversation: %d blobs", len(store.blobs))
	}
	// The index is still built: it is small, and without it nothing can be found.
	if _, ok := store.m[store.key(s, SearchArtifactKind)]; !ok {
		t.Error("the index should still have been written")
	}

	// Once indexed, a full cache gives the parser nothing more to do — which is
	// the loop this closes.
	loads := l.loads
	a.BuildIndex(context.Background(), []domain.Session{s}, store, nil, nil)
	if l.loads != loads {
		t.Errorf("a full cache should not keep reparsing: loads=%d want %d", l.loads, loads)
	}

	// Room again: the conversation is picked up on the next pass.
	store.size = 0
	a.BuildIndex(context.Background(), []domain.Session{s}, store, nil, nil)
	if len(store.blobs) != 1 {
		t.Errorf("the conversation should be stored once there is room: %d blobs", len(store.blobs))
	}
}
