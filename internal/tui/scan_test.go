package tui

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"github.com/agentcarto/plugin-claude"
)

type excludedLogScanner struct{ source string }

func (s excludedLogScanner) Scan(context.Context, plugin.ScanInput) (plugin.ScanOutput, error) {
	return plugin.ScanOutput{Dead: map[string]string{s.source: "fp"}}, nil
}

func TestScanDoesNotRestoreExcludedCachedLog(t *testing.T) {
	ctx := context.Background()
	db, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	review := domain.Session{PluginID: "codex", SessionID: "review", UpdatedAt: time.Now(),
		SourceRef: domain.SessionRef{Source: "/logs/review.jsonl"}}
	if err := db.Save(ctx, []domain.Session{review}); err != nil {
		t.Fatal(err)
	}
	a := app.Build(config.Config{}, []plugin.Instance{{ID: "codex",
		Impl: excludedLogScanner{source: review.SourceRef.Source}}})
	m := Model{app: a, cache: db, sessions: []domain.Session{review}}
	for _, incremental := range []bool{false, true} {
		m.scanned = incremental
		sm := m.scan()().(scanMsg)
		if len(sm.sessions) != 0 {
			t.Fatalf("incremental=%v restored excluded log: %+v", incremental, sm.sessions)
		}
		m.sessions, m.dead = sm.sessions, sm.snap.Dead
	}
}

// A command run after the TUI closes reads what the TUI's scans excluded, rather
// than reparsing those logs.
func TestScanStoresWhatItExcluded(t *testing.T) {
	db, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	excluded := map[string]string{"/logs/subagent.jsonl": "stamp"}
	m := Model{width: 120, height: 20, view: "time", cache: db, app: &app.App{}}

	m.Update(scanMsg{snap: domain.Snapshot{Dead: excluded}})

	got, err := db.LoadDead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got, excluded) {
		t.Errorf("LoadDead = %v, want %v", got, excluded)
	}
}

// The first scan right after startup does a full reparse without using the warm (cache), while later
// periodic scans reuse the warm differentially. Both behaviors are confirmed by a stale warm title
// surviving in a differential scan (reused) but disappearing in the first full scan (reparsed).
func TestFirstScanIsFullThenDifferential(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(proj, "s.jsonl")
	if err := os.WriteFile(src, []byte(`{"uuid":"u1","cwd":"/work","timestamp":"2026-06-23T00:00:00Z","message":{"role":"user","content":"real question"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var opts yaml.Node
	if err := opts.Encode(map[string]string{"projects_dir": proj}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Index:   config.Index{MaxCharsPerSession: 100000},
		Plugins: []config.Plugin{{ID: "claude", Type: "claude", Enabled: true, Options: opts}},
	}
	// Although plugins are normally run as subprocesses, this test uses the claude parser in-process
	// to verify the TUI scan flow (first full / later differential). The Instance is assembled directly.
	impl, err := claude.Factory{}.New("claude", &opts)
	if err != nil {
		t.Fatal(err)
	}
	a := app.Build(cfg, []plugin.Instance{{ID: "claude", Descriptor: claude.Factory{}.Descriptor(), Impl: impl}})

	run := func(m Model) scanMsg {
		msg := m.scan()()
		sm, ok := msg.(scanMsg)
		if !ok {
			t.Fatalf("scan() did not return scanMsg: %T", msg)
		}
		return sm
	}

	// 1) A full scan (equivalent to the first scan) obtains the real title.
	m := Model{app: a, scanning: true}
	sm := run(m)
	if len(sm.snap.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sm.snap.Sessions))
	}
	realTitle := sm.snap.Sessions[0].Title
	if realTitle == "" || realTitle == "STALE" {
		t.Fatalf("unexpected parsed title %q", realTitle)
	}

	// Keep it as warm (with the correct Fingerprint/ParserVersion that a differential scan reuses). Corrupt the title.
	m.sessions = sm.snap.Sessions
	m.dead = sm.snap.Dead
	m.index = sm.index
	m.indexFP = sm.indexFP
	m.sessions[0].Title = "STALE"

	// 2) Differential scan (scanned=true): fingerprint matches, so the warm is reused -> the stale title remains.
	m.scanned = true
	sm2 := run(m)
	if got := sm2.snap.Sessions[0].Title; got != "STALE" {
		t.Fatalf("differential scan should reuse warm (stale title), got %q", got)
	}

	// 3) First full scan (scanned=false): ignore the warm and reparse -> back to the real title.
	m.scanned = false
	sm3 := run(m)
	if got := sm3.snap.Sessions[0].Title; got != realTitle {
		t.Fatalf("first scan should be full (reparse, fresh title %q), got %q", realTitle, got)
	}
}

// scanRecorder reports what each scan was handed and always finds the same thing.
type scanRecorder struct {
	handed  *[]plugin.ScanInput
	session domain.Session
}

// Scan implements plugin.Scanner.
func (s scanRecorder) Scan(_ context.Context, in plugin.ScanInput) (plugin.ScanOutput, error) {
	*s.handed = append(*s.handed, in)
	return plugin.ScanOutput{Sessions: []domain.Session{s.session}, Dead: map[string]string{"/logs/subagent.jsonl": "stamp"}}, nil
}

// reloadFixture is a TUI past its startup scan, holding results a later scan
// could reuse, and the record of what its scans are handed from here on.
func reloadFixture() (Model, *[]plugin.ScanInput) {
	s := domain.Session{PluginID: "claude", SessionID: "s", UpdatedAt: time.Now(),
		SourceRef: domain.SessionRef{Source: "/logs/s.jsonl"}}
	var handed []plugin.ScanInput
	a := app.Build(config.Config{}, []plugin.Instance{{ID: "claude", Impl: scanRecorder{handed: &handed, session: s}}})
	return Model{width: 120, height: 20, view: "time", app: a, scanned: true,
		sessions: []domain.Session{s}, dead: map[string]string{"/logs/subagent.jsonl": "stamp"}}, &handed
}

// reparsedEverything reports whether a scan was handed nothing to reuse.
func reparsedEverything(in plugin.ScanInput) bool { return len(in.Warm) == 0 && len(in.Dead) == 0 }

// land delivers a finished scan to the model.
func land(m Model, x scanMsg) Model {
	u, _ := m.Update(x)
	return u.(Model)
}

func TestReloadReparsesEverythingThenScansGoBackToBeingDifferential(t *testing.T) {
	m, handed := reloadFixture()

	u, cmd := m.Update(keyRunes("r"))
	m = u.(Model)
	if cmd == nil || !m.scanning {
		t.Fatalf("reload started nothing: cmd=%v scanning=%v", cmd, m.scanning)
	}
	m = land(m, cmd().(scanMsg))
	if m.scanning {
		t.Error("the scan landed but the TUI still says it is scanning")
	}
	if len(*handed) != 1 || !reparsedEverything((*handed)[0]) {
		t.Fatalf("reload should reparse everything, was handed %+v", *handed)
	}

	m.scan()()
	if reparsedEverything((*handed)[1]) {
		t.Errorf("the scan after a reload should reuse its results, was handed %+v", (*handed)[1])
	}
}

// A reload pressed while the periodic scan is running starts nothing of its own,
// but it must not be lost when that scan lands.
func TestReloadDuringAPeriodicScanMakesTheNextScanFull(t *testing.T) {
	m, handed := reloadFixture()
	m.scanning = true

	u, cmd := m.Update(keyRunes("r"))
	m = u.(Model)
	if cmd != nil {
		t.Fatal("reload started a second scan alongside the running one")
	}
	m = land(m, scanMsg{sessions: m.sessions})

	m.scan()()
	if len(*handed) != 1 || !reparsedEverything((*handed)[0]) {
		t.Errorf("the scan after that one should reparse everything, was handed %+v", *handed)
	}
}

// The running scan is already the full one, so there is nothing left to ask for.
func TestReloadDuringAFullScanDoesNotRepeatIt(t *testing.T) {
	m, handed := reloadFixture()
	m.scanned, m.scanning = false, true

	u, cmd := m.Update(keyRunes("r"))
	m = u.(Model)
	if cmd != nil {
		t.Fatal("reload started a second scan alongside the running one")
	}
	m = land(m, scanMsg{full: true, sessions: m.sessions, snap: domain.Snapshot{Dead: m.dead}})

	m.scan()()
	if len(*handed) != 1 || reparsedEverything((*handed)[0]) {
		t.Errorf("the scan after that one should reuse its results, was handed %+v", *handed)
	}
}

// A log that could not be read at the moment of a scan looks like one with nothing
// in it, and stays excluded for as long as it does not change, which for a finished
// session is forever. A reload is the way back.
func TestReloadRecoversALogThatCouldNotBeRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs file modes that stop this process from reading")
	}
	ctx := context.Background()
	proj := t.TempDir()
	src := filepath.Join(proj, "s.jsonl")
	if err := os.WriteFile(src, []byte(`{"uuid":"u1","cwd":"/work","timestamp":"2026-06-23T00:00:00Z","message":{"role":"user","content":"real question"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var opts yaml.Node
	if err := opts.Encode(map[string]string{"projects_dir": proj}); err != nil {
		t.Fatal(err)
	}
	impl, err := claude.Factory{}.New("claude", &opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Index: config.Index{MaxCharsPerSession: 100000},
		Plugins: []config.Plugin{{ID: "claude", Type: "claude", Enabled: true, Options: opts}}}
	a := app.Build(cfg, []plugin.Instance{{ID: "claude", Descriptor: claude.Factory{}.Descriptor(), Impl: impl}})
	db, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	stored := func() map[string]string {
		dead, err := db.LoadDead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return dead
	}
	m := Model{width: 120, height: 20, view: "time", app: a, cache: db}

	// Changing the mode leaves the size and mtime alone, so the fingerprint stays
	// the same, as it does for a read that failed once and then works.
	if err := os.Chmod(src, 0); err != nil {
		t.Fatal(err)
	}
	m = land(m, m.scan()().(scanMsg))
	if err := os.Chmod(src, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, excluded := stored()[src]; !excluded || len(m.sessions) != 0 {
		t.Fatalf("the unreadable log should be excluded: stored=%v sessions=%+v", stored(), m.sessions)
	}

	m = land(m, m.scan()().(scanMsg))
	if len(m.sessions) != 0 {
		t.Fatalf("a periodic scan should not look at it again: %+v", m.sessions)
	}

	u, cmd := m.Update(keyRunes("r"))
	m = land(u.(Model), cmd().(scanMsg))
	if len(m.sessions) != 1 {
		t.Fatalf("reload should bring the session back, got %+v", m.sessions)
	}
	if _, excluded := stored()[src]; excluded {
		t.Errorf("reload should clear the stored exclusion: %v", stored())
	}
}

func TestSessionParentTopologyIgnoresConversationGrowth(t *testing.T) {
	before := []domain.Session{
		{PluginID: "codex", SessionID: "root", Title: "old"},
		{PluginID: "codex", SessionID: "child", ParentSessionID: "root", Fingerprint: "old"},
	}
	after := append([]domain.Session(nil), before...)
	after[0].Title = "new"
	after[1].Fingerprint = "new"
	after[1].UpdatedAt = time.Now()
	if !sameSessionParentTopology(before, after) {
		t.Fatal("conversation growth should not invalidate logical parents")
	}
}

func TestSessionParentTopologyDetectsRelationshipChanges(t *testing.T) {
	base := []domain.Session{
		{PluginID: "codex", SessionID: "root"},
		{PluginID: "codex", SessionID: "child", ParentSessionID: "root"},
	}
	tests := map[string][]domain.Session{
		"new session": append(append([]domain.Session(nil), base...), domain.Session{PluginID: "codex", SessionID: "grandchild", ParentSessionID: "child"}),
		"new parent": {
			{PluginID: "codex", SessionID: "root"},
			{PluginID: "codex", SessionID: "child", ParentSessionID: "other"},
		},
		"empty fork started": {
			{PluginID: "codex", SessionID: "root"},
			{PluginID: "codex", SessionID: "child", ParentSessionID: "root", EmptyFork: true},
		},
	}
	for name, changed := range tests {
		t.Run(name, func(t *testing.T) {
			if sameSessionParentTopology(base, changed) {
				t.Fatal("relationship change should invalidate logical parents")
			}
		})
	}
}

// The list is what the scan found plus what the cache remembers and the scan
// could not: a session whose log was deleted is still there to read. What the
// cache is told it has seen stays the scan's own answer, so a deleted session
// does not look like it was there a moment ago.
func TestScanMsgSeparatesWhatWasSeenFromWhatIsShown(t *testing.T) {
	now := time.Now()
	seen := domain.Session{PluginID: "claude", SessionID: "alive", UpdatedAt: now,
		SourceRef: domain.SessionRef{Source: "/logs/alive.jsonl"}}
	gone := domain.Session{PluginID: "claude", SessionID: "gone", UpdatedAt: now.Add(-time.Hour),
		SourceRef: domain.SessionRef{Source: "/logs/gone.jsonl"}}

	snap := domain.Snapshot{Sessions: []domain.Session{seen}}
	shown := app.MergeDeletedLogs(snap.Sessions, []domain.Session{seen, gone}, map[string]bool{"claude": true}, snap.Dead)
	msg := scanMsg{snap: snap, sessions: shown}

	if len(msg.snap.Sessions) != 1 || msg.snap.Sessions[0].SessionID != "alive" {
		t.Errorf("the snapshot should stay what the scan returned: %+v", msg.snap.Sessions)
	}
	if len(msg.sessions) != 2 {
		t.Fatalf("the list should hold both: %+v", msg.sessions)
	}
	if !msg.sessions[1].LogDeleted {
		t.Errorf("the session read from the cache should be marked: %+v", msg.sessions[1])
	}

	m := Model{app: &app.App{}, width: 120}
	if row := m.sessionRow(msg.sessions[1], false, 1, ""); !strings.Contains(row, "⌫") {
		t.Errorf("the row should mark a deleted log: %q", row)
	}
	if row := m.sessionRow(msg.sessions[0], false, 1, ""); strings.Contains(row, "⌫") {
		t.Errorf("a session still on disk should carry no mark: %q", row)
	}
}
