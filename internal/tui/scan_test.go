package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"github.com/agentcarto/plugin-claude"
)

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
	shown := app.MergeDeletedLogs(snap.Sessions, []domain.Session{seen, gone}, map[string]bool{"claude": true})
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
