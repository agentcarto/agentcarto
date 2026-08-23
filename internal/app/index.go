package app

import (
	"context"
	"github.com/agentcarto/agentcarto/internal/search"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
)

// SearchArtifactKind names the cached form of a session's index text. The
// version suffix is part of the key: an entry written by an older shape of the
// artifact is simply never read again. Bump it whenever search.IndexText changes
// what it covers, or stale entries would keep answering with the old coverage.
const SearchArtifactKind = "search-v6"

// ConversationArtifactKind names the cached copy of a session's conversation.
// Unlike the index it is not derived data that can be rebuilt: once the log it
// was parsed from is gone, this is the only copy left. The version belongs to
// the shape of domain.Conversation, not to what search covers, so it moves on
// its own.
const ConversationArtifactKind = "conversation-v1"

// ArtifactStore is the part of the cache the index build uses. Taking it as an
// interface keeps internal/app free of a dependency on internal/cache, and lets
// a caller without a cache (the CLI with --no-cache) pass nil.
type ArtifactStore interface {
	GetArtifact(ctx context.Context, s domain.Session, kind string, dst any) bool
	PutArtifact(ctx context.Context, s domain.Session, kind string, v any) error
	// HasArtifact answers without reading the bytes, which is what deciding
	// whether to parse a session needs.
	HasArtifact(ctx context.Context, s domain.Session, kind string) bool
	// PutBlob stores a compressed artifact: the conversation, which is the size of
	// the log it was read from.
	PutBlob(ctx context.Context, s domain.Session, kind string, v any) error
	// Size is what the store occupies, so a caller can stop before filling it.
	Size() int64
}

// indexArtifact is the cached payload. The field names are part of the on-disk
// format (the cache stores artifacts as JSON), so renaming them invalidates
// every stored entry.
type indexArtifact struct {
	Text  string
	Count int
}

// BuildIndex indexes the conversation text of every session for search. Parsing
// a session is the expensive part, so it is avoided twice over: an unchanged
// session (same fingerprint) reuses the entry of the previous index, and one
// that was indexed in an earlier run is read back from the artifact cache.
// prev/prevFP may be nil, in which case every session goes through the cache or
// the parser.
//
// It returns the index and the fingerprint of each indexed session, keyed by
// source path, to be handed back as prevFP on the next call.
func (a *App) BuildIndex(ctx context.Context, sessions []domain.Session, store ArtifactStore, prev *search.Index, prevFP map[string]string) (*search.Index, map[string]string) {
	idx := search.New(a.Config.Index.MaxCharsPerSession)
	fp := make(map[string]string, len(sessions))
	for _, s := range sessions {
		src := s.SourceRef.Source
		if s.Fingerprint != "" && prevFP[src] == s.Fingerprint && idx.CopyFrom(prev, s) {
			fp[src] = s.Fingerprint
			continue
		}
		if s.LogDeleted {
			// The log is gone, so the plugin has nothing to give: what the cache holds
			// is all there is. Asking anyway would be one failed read per deleted
			// session on every scan.
			if store != nil {
				var cached indexArtifact
				if store.GetArtifact(ctx, s, SearchArtifactKind, &cached) {
					idx.Set(s, cached.Text, cached.Count)
				}
			}
			fp[src] = s.Fingerprint
			continue
		}
		p, ok := a.Catalog.Plugin(s.PluginID)
		if !ok {
			continue
		}
		l, ok := p.Impl.(plugin.ConversationLoader)
		if !ok {
			continue // the plugin cannot read conversations: nothing to index
		}
		var cached indexArtifact
		indexed := store != nil && store.GetArtifact(ctx, s, SearchArtifactKind, &cached)
		if indexed {
			idx.Set(s, cached.Text, cached.Count)
		}
		// The conversation is kept so the session survives its log: an agent
		// deletes its own logs, and what was said is then only here. It is written
		// once per version of a session, and asking whether it is already there
		// costs a row lookup against the parse it saves.
		// A full store stops taking conversations. Storing into a store that is over
		// its limit only means the next run evicts one and reparses the session to
		// store it again — for as long as it stays full, which is forever.
		keep := store != nil && a.Config.Cache.CacheConversations &&
			store.Size() < int64(a.Config.Cache.MaxSize) &&
			!store.HasArtifact(ctx, s, ConversationArtifactKind)
		if !indexed || keep {
			// One parse serves both. Without the second condition a session that is
			// already indexed would never be read again, and its conversation would
			// never be stored at all.
			if c, err := l.LoadConversation(ctx, s.SourceRef); err == nil && c != nil {
				if !indexed {
					idx.Add(s, *c)
					if t, n, ok := idx.Lookup(s); ok && store != nil {
						// A failed write costs a reparse next time and nothing else.
						_ = store.PutArtifact(ctx, s, SearchArtifactKind, indexArtifact{t, n})
					}
				}
				if keep {
					_ = store.PutBlob(ctx, s, ConversationArtifactKind, c)
				}
			}
		}
		// The fingerprint is recorded even when the parse failed: there is no entry
		// to copy from next time, so CopyFrom returns false and the session is
		// tried again.
		fp[src] = s.Fingerprint
	}
	return idx, fp
}
