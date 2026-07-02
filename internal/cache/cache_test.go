package cache

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	d, e := Open(filepath.Join(t.TempDir(), "cache.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestSaveLoadRoundTrip(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	in := []domain.Session{
		{PluginID: "p", SessionID: "s1", Title: "one", Status: domain.StatusRunning, PermissionWait: true},
		{PluginID: "p", SessionID: "s2", Title: "two"},
	}
	if e := d.Save(ctx, in); e != nil {
		t.Fatal(e)
	}
	out, e := d.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if len(out) != 2 {
		t.Fatalf("loaded %d sessions, want 2", len(out))
	}
	for _, s := range out {
		// Volatile fields must not survive the cache round trip.
		if s.Status != "" || s.PermissionWait {
			t.Fatalf("volatile fields leaked through the cache: %+v", s)
		}
	}
}

func TestArtifactFingerprintAndParserVersionGate(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1", Fingerprint: "fp1", ParserVersion: "1"}
	if e := d.PutArtifact(ctx, s, "conversation", map[string]string{"k": "v"}); e != nil {
		t.Fatal(e)
	}
	var got map[string]string
	if !d.GetArtifact(ctx, s, "conversation", &got) || got["k"] != "v" {
		t.Fatalf("artifact did not round trip: %v", got)
	}
	stale := s
	stale.Fingerprint = "fp2"
	if d.GetArtifact(ctx, stale, "conversation", &got) {
		t.Fatal("stale fingerprint must miss")
	}
	stale = s
	stale.ParserVersion = "2"
	if d.GetArtifact(ctx, stale, "conversation", &got) {
		t.Fatal("stale parser version must miss")
	}
}

func TestPruneDropsOnlyOldUnseenSessionsOfSuccessfulPlugins(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	old := []domain.Session{
		{PluginID: "ok", SessionID: "gone"},
		{PluginID: "ok", SessionID: "kept"},
		{PluginID: "failed", SessionID: "unknown"},
	}
	if e := d.Save(ctx, old); e != nil {
		t.Fatal(e)
	}
	// Only "kept" is still present in the current scan; plugin "failed" did not
	// complete its scan, so its sessions must survive regardless. A negative
	// maxAge makes the just-written rows count as old (seen is second-granular).
	current := []domain.Session{{PluginID: "ok", SessionID: "kept"}}
	if e := d.Prune(ctx, current, map[string]bool{"ok": true}, -time.Hour); e != nil {
		t.Fatal(e)
	}
	out, e := d.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	ids := map[string]bool{}
	for _, s := range out {
		ids[s.PluginID+"|"+s.SessionID] = true
	}
	if ids["ok|gone"] || !ids["ok|kept"] || !ids["failed|unknown"] {
		t.Fatalf("prune selection wrong: %v", ids)
	}
	// maxAge in the future protects everything.
	if e := d.Prune(ctx, nil, map[string]bool{"ok": true}, time.Hour); e != nil {
		t.Fatal(e)
	}
	if out, _ = d.Load(ctx); len(out) != 2 {
		t.Fatalf("recent sessions must survive prune, got %d", len(out))
	}
}

// Enforce must actually shrink the file below max — not merely delete rows.
// With auto_vacuum unset, incremental_vacuum was a no-op, the file never
// shrank, and the loop wiped the entire artifacts table on every run.
func TestEnforceShrinksFileAndKeepsRecentArtifacts(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	blob := make([]byte, 32<<10)
	for i := 0; i < 128; i++ {
		s := domain.Session{PluginID: "p", SessionID: fmt.Sprintf("s%03d", i), Fingerprint: "fp", ParserVersion: "1"}
		if e := d.PutArtifact(ctx, s, "conversation", blob); e != nil {
			t.Fatal(e)
		}
	}
	before, e := d.sizeOnDisk()
	if e != nil {
		t.Fatal(e)
	}
	max := before / 2
	if e := d.Enforce(ctx, max); e != nil {
		t.Fatal(e)
	}
	after, e := d.sizeOnDisk()
	if e != nil {
		t.Fatal(e)
	}
	if after > max {
		t.Fatalf("Enforce left %d bytes on disk, want <= %d (deletes must reclaim space)", after, max)
	}
	var n int
	if e := d.db.QueryRowContext(ctx, "SELECT count(*) FROM artifacts").Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n == 0 {
		t.Fatal("Enforce wiped every artifact instead of stopping at the size target")
	}
}

func TestEnforceTerminatesWhenEmpty(t *testing.T) {
	d := openTemp(t)
	// Nothing to delete and (likely) a file below max: must return, not spin.
	if e := d.Enforce(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
}

// Reopening an existing database migrates it to auto_vacuum=incremental.
func TestOpenMigratesAutoVacuum(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.db")
	d, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	_ = d.Close()
	d, e = Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	var av int
	if e := d.db.QueryRow("PRAGMA auto_vacuum").Scan(&av); e != nil {
		t.Fatal(e)
	}
	if av != 2 {
		t.Fatalf("auto_vacuum=%d want 2 (incremental)", av)
	}
}
