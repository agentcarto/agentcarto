package catalog

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"github.com/agentcarto/core/scan"
)

// fakePlugin scans the files in dir. "real*" entries produce a session; everything
// else is recorded as dead. parses counts how many files were actually parsed (cache
// misses). pv is the plugin's own ParserVersion (used for the reuse decision).
type fakePlugin struct {
	dir    string
	pv     string
	parses int
}

func (f *fakePlugin) Scan(_ context.Context, in plugin.ScanInput) (plugin.ScanOutput, error) {
	cache := scan.New(in.Warm, in.Dead, f.pv)
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return plugin.ScanOutput{}, err
	}
	var out []domain.Session
	for _, e := range entries {
		path := filepath.Join(f.dir, e.Name())
		if s, ok := cache.Reuse(path); ok {
			out = append(out, s)
			continue
		}
		if cache.Skip(path) {
			continue
		}
		f.parses++
		if strings.HasPrefix(e.Name(), "real") {
			s := domain.Session{PluginID: "fake", SessionID: e.Name(), SourceRef: domain.SessionRef{Source: path}}
			cache.Stamp(&s)
			out = append(out, s)
		} else {
			cache.Dead(path)
		}
	}
	return plugin.ScanOutput{Sessions: out, Dead: cache.DeadOut()}, nil
}

func TestScanReuseAndDeadCache(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real1.jsonl")
	empty := filepath.Join(dir, "empty1.jsonl")
	if err := os.WriteFile(real, []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, []byte("b"), 0600); err != nil {
		t.Fatal(err)
	}
	fp := &fakePlugin{dir: dir, pv: "1"}
	c := Catalog{Plugins: []plugin.Instance{{ID: "fake", Descriptor: plugin.Descriptor{ParserVersion: "1"}, Impl: fp}}}
	ctx := context.Background()

	// First run (cold): both files parsed. 1 session, 1 dead.
	snap1 := c.Scan(ctx, nil, nil)
	if fp.parses != 2 {
		t.Fatalf("cold parses=%d want 2", fp.parses)
	}
	if len(snap1.Sessions) != 1 {
		t.Fatalf("cold sessions=%d want 1", len(snap1.Sessions))
	}
	if len(snap1.Dead) != 1 {
		t.Fatalf("cold dead=%d want 1", len(snap1.Dead))
	}

	// Second run (warm, unchanged): nothing is parsed thanks to reuse and skip.
	fp.parses = 0
	snap2 := c.Scan(ctx, snap1.Sessions, snap1.Dead)
	if fp.parses != 0 {
		t.Fatalf("warm parses=%d want 0 (reuse+skip)", fp.parses)
	}
	if len(snap2.Sessions) != 1 || len(snap2.Dead) != 1 {
		t.Fatalf("warm sessions=%d dead=%d want 1/1 (carried forward)", len(snap2.Sessions), len(snap2.Dead))
	}

	// Change real (the size change alters its fingerprint): only that file is
	// re-parsed, while empty keeps being skipped.
	if err := os.WriteFile(real, []byte("aaa-changed"), 0600); err != nil {
		t.Fatal(err)
	}
	fp.parses = 0
	snap3 := c.Scan(ctx, snap2.Sessions, snap2.Dead)
	if fp.parses != 1 {
		t.Fatalf("after-change parses=%d want 1 (only changed file)", fp.parses)
	}
	if len(snap3.Sessions) != 1 {
		t.Fatalf("after-change sessions=%d want 1", len(snap3.Sessions))
	}
}

func TestBackfillInferredCWDFromNearbySession(t *testing.T) {
	t0 := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	sessions := []domain.Session{
		{PluginID: "copilot-jb", AgentType: "copilot-jb", SessionID: "copilot-inline", CWD: "(unknown)", InferCWD: true, StartedAt: t0},
		{PluginID: "codex", SessionID: "nearby", CWD: "/repo", StartedAt: t0.Add(5 * time.Minute)},
		{PluginID: "codex", SessionID: "far", CWD: "/other", StartedAt: t0.Add(24 * time.Hour)},
	}

	backfillInferredCWD(sessions, 6*time.Hour)

	if sessions[0].CWD != "/repo" {
		t.Fatalf("cwd=%q want /repo", sessions[0].CWD)
	}
}

func TestBackfillInferredCWDIgnoresDistantSession(t *testing.T) {
	t0 := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	sessions := []domain.Session{
		{PluginID: "copilot-jb", AgentType: "copilot-jb", SessionID: "copilot-inline", CWD: "(unknown)", InferCWD: true, StartedAt: t0},
		{PluginID: "codex", SessionID: "far", CWD: "/other", StartedAt: t0.Add(24 * time.Hour)},
	}

	backfillInferredCWD(sessions, 6*time.Hour)

	if sessions[0].CWD != "(unknown)" {
		t.Fatalf("cwd=%q want (unknown)", sessions[0].CWD)
	}
}

func TestBackfillSkipsUnflaggedUnknownCWD(t *testing.T) {
	t0 := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	sessions := []domain.Session{
		{PluginID: "claude", AgentType: "claude", SessionID: "s", CWD: "(unknown)", StartedAt: t0},
		{PluginID: "codex", SessionID: "nearby", CWD: "/repo", StartedAt: t0.Add(5 * time.Minute)},
	}

	backfillInferredCWD(sessions, 6*time.Hour)

	if sessions[0].CWD != "(unknown)" {
		t.Fatalf("cwd=%q want (unknown): only InferCWD sessions may borrow", sessions[0].CWD)
	}
}

// When the parser version changes, reuse is invalidated and the file is re-parsed.
func TestScanInvalidatesOnParserVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real1.jsonl"), []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	fp := &fakePlugin{dir: dir, pv: "1"}
	ctx := context.Background()

	c1 := Catalog{Plugins: []plugin.Instance{{ID: "fake", Descriptor: plugin.Descriptor{ParserVersion: "1"}, Impl: fp}}}
	snap1 := c1.Scan(ctx, nil, nil)

	fp.parses = 0
	fp.pv = "2" // the plugin's parser version was bumped, so the old warm (pv "1") must be invalidated
	c2 := Catalog{Plugins: []plugin.Instance{{ID: "fake", Descriptor: plugin.Descriptor{ParserVersion: "2"}, Impl: fp}}}
	c2.Scan(ctx, snap1.Sessions, snap1.Dead)
	if fp.parses != 1 {
		t.Fatalf("parser-version-change parses=%d want 1 (reuse invalidated)", fp.parses)
	}
}

// The per-file Lstat path of fingerprint must return the same hash as the older
// WalkDir traversal. If this breaks, the persistent cache (artifacts.fingerprint
// in cache.db) misses entirely, so we pin the behavior.
func TestFingerprintFileMatchesWalkDir(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(f, []byte("hello world"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(f)
	if err != nil {
		t.Fatal(err)
	}
	h := fnv.New64a()
	_, _ = io.WriteString(h, f)
	_, _ = io.WriteString(h, fmt.Sprintf(":%d:%d", st.Size(), st.ModTime().UnixNano()))
	want := fmt.Sprintf("%x", h.Sum64())
	if got := scan.Fingerprint(f); got != want {
		t.Fatalf("fingerprint(file)=%s want %s (WalkDir compatibility is broken)", got, want)
	}
}

// Passing a directory still walks the whole tree (grok's directory-level sessions).
func TestFingerprintDirWalksTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	before := scan.Fingerprint(dir)
	if err := os.WriteFile(filepath.Join(dir, "b"), []byte("y"), 0600); err != nil {
		t.Fatal(err)
	}
	if after := scan.Fingerprint(dir); after == before {
		t.Fatal("fingerprint did not change after adding a file to the directory")
	}
}
