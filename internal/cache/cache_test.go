package cache

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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

// A bump of the artifact kind leaves the previous generation behind: nothing
// else deletes artifacts of a session that still exists. What must survive the
// sweep is a kind of another name — an older agentcarto sharing this cache would
// otherwise delete the conversation, which is the one artifact that cannot be
// rebuilt.
func TestDropSupersededArtifacts(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	s := domain.Session{PluginID: "claude", SessionID: "s1", Fingerprint: "fp", ParserVersion: "1"}
	for _, kind := range []string{"search-v3", "search-v4", "conversation-v1", "unversioned"} {
		if err := db.PutArtifact(ctx, s, kind, map[string]string{"Text": kind}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DropSupersededArtifacts(ctx, "search-v4"); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if db.GetArtifact(ctx, s, "search-v3", &got) {
		t.Error("the superseded artifact survived")
	}
	if !db.GetArtifact(ctx, s, "search-v4", &got) || got["Text"] != "search-v4" {
		t.Errorf("the current artifact was dropped: %v", got)
	}
	// A kind this sweep was not told about is none of its business.
	for _, kind := range []string{"conversation-v1", "unversioned"} {
		if !db.GetArtifact(ctx, s, kind, &got) {
			t.Errorf("%s was deleted by a sweep of another kind", kind)
		}
	}
	// Sweeping nothing is a no-op rather than a table wipe.
	if err := db.DropSupersededArtifacts(ctx); err != nil {
		t.Fatal(err)
	}
	if !db.GetArtifact(ctx, s, "search-v4", &got) {
		t.Error("an empty list emptied the table")
	}
}

// A conversation is stored compressed, because it is the size of the log it was
// parsed from and the cache now keeps one for every session on the machine.
func TestBlobRoundTripAndCompression(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1", Fingerprint: "fp1", ParserVersion: "1"}

	// Text that compresses, which is what a conversation is: the same words over
	// and over, in two languages.
	long := strings.Repeat("handoff の順序はプラグインを先に落とすこと。 ", 200)
	c := domain.NewConversation([]domain.ConvNode{{ID: "n1", Events: []domain.Event{
		{Kind: domain.EventUser, Text: long, Prompt: long},
	}}})
	if d.HasArtifact(ctx, s, "conversation-v1") {
		t.Fatal("nothing has been stored yet")
	}
	if e := d.PutBlob(ctx, s, "conversation-v1", &c); e != nil {
		t.Fatal(e)
	}
	if !d.HasArtifact(ctx, s, "conversation-v1") {
		t.Error("HasArtifact should see what PutBlob wrote")
	}

	var got domain.Conversation
	if !d.GetBlob(ctx, s, "conversation-v1", &got) {
		t.Fatal("the conversation should read back")
	}
	if len(got.Nodes) != 1 || got.Nodes["n1"].Events[0].Text != long {
		t.Errorf("the conversation did not survive the round trip: %+v", got)
	}

	var stored int
	if e := d.db.QueryRow("SELECT length(data) FROM artifacts WHERE session_id='s1'").Scan(&stored); e != nil {
		t.Fatal(e)
	}
	if stored > len(long)/4 {
		t.Errorf("stored %d bytes for %d bytes of text: it is not being compressed", stored, len(long))
	}
}

// An artifact belongs to one version of a session. A session that changed
// underneath must not read back what was written for the old one — the whole
// point of keeping the fingerprint on the row.
func TestBlobIsTiedToTheSessionVersion(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1", Fingerprint: "fp1", ParserVersion: "1"}
	c := domain.NewConversation([]domain.ConvNode{{ID: "n1"}})
	if e := d.PutBlob(ctx, s, "conversation-v1", &c); e != nil {
		t.Fatal(e)
	}
	changed := s
	changed.Fingerprint = "fp2"
	if d.HasArtifact(ctx, changed, "conversation-v1") {
		t.Error("a changed session should not see the old artifact")
	}
	var got domain.Conversation
	if d.GetBlob(ctx, changed, "conversation-v1", &got) {
		t.Error("a changed session should not read the old artifact")
	}
	// What PutArtifact wrote is not a blob and must not be read as one.
	if e := d.PutArtifact(ctx, s, "plain", map[string]string{"a": "b"}); e != nil {
		t.Fatal(e)
	}
	var any map[string]string
	if d.GetBlob(ctx, s, "plain", &any) {
		t.Error("uncompressed bytes should not decode as a blob")
	}
}

// A session whose log was deleted is what the cache exists for once its
// conversation is stored: max_age must not collect it. One that was never
// stored is a title and a time, and that does expire.
func TestPruneKeepsWhatCanStillBeRead(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	readable := domain.Session{PluginID: "p", SessionID: "readable", Fingerprint: "fp", ParserVersion: "1"}
	bare := domain.Session{PluginID: "p", SessionID: "bare", Fingerprint: "fp", ParserVersion: "1"}
	if e := d.Save(ctx, []domain.Session{readable, bare}); e != nil {
		t.Fatal(e)
	}
	c := domain.NewConversation([]domain.ConvNode{{ID: "n1"}})
	if e := d.PutBlob(ctx, readable, "conversation-v1", &c); e != nil {
		t.Fatal(e)
	}
	// Neither is on disk any more, and both are older than the age allowed.
	if e := d.Prune(ctx, nil, map[string]bool{"p": true}, -time.Hour); e != nil {
		t.Fatal(e)
	}
	got, e := d.Load(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 1 || got[0].SessionID != "readable" {
		t.Fatalf("the readable session should have survived and the bare one gone: %+v", got)
	}
	var back domain.Conversation
	if !d.GetBlob(ctx, readable, "conversation-v1", &back) {
		t.Error("the conversation of the surviving session was collected with it")
	}
}
