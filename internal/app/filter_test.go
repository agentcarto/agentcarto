package app

import (
	"github.com/agentcarto/core/domain"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func abs(slash string) string {
	p := filepath.FromSlash(slash)
	if runtime.GOOS == "windows" {
		p = `C:` + p
	}
	return p
}

func session(cwd, plugin, agent string, updated time.Time) domain.Session {
	return domain.Session{CWD: cwd, PluginID: plugin, AgentType: agent, UpdatedAt: updated}
}

func TestFilterSessionsByDirectory(t *testing.T) {
	now := time.Now()
	sessions := []domain.Session{
		session(abs("/repo/app"), "claude", "claude", now),
		session(abs("/repo/app/sub"), "claude", "claude", now),
		session(abs("/repo/app2"), "claude", "claude", now), // sibling sharing a name prefix
		session(abs("/elsewhere"), "claude", "claude", now),
		session("", "claude", "claude", now), // no recorded cwd
	}
	got := FilterSessions(sessions, SessionFilter{CWD: abs("/repo/app") + string(filepath.Separator)})
	if len(got) != 2 || got[0].CWD != abs("/repo/app") || got[1].CWD != abs("/repo/app/sub") {
		t.Fatalf("kept %d sessions: %+v", len(got), got)
	}
}

func TestFilterSessionsByAgentAndTime(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	sessions := []domain.Session{
		session(abs("/x"), "claude", "claude", recent),
		session(abs("/x"), "codex", "codex", recent),
		session(abs("/x"), "copilot-vc", "copilot-vc", old),
	}
	if got := FilterSessions(sessions, SessionFilter{Agent: "CODEX"}); len(got) != 1 || got[0].PluginID != "codex" {
		t.Fatalf("agent filter kept %+v", got)
	}
	// The agent type matches too, and so does the family two plugins share: a
	// copilot session is reachable by "copilot" without naming its editor.
	if got := FilterSessions(sessions, SessionFilter{Agent: "copilot"}); len(got) != 1 || got[0].PluginID != "copilot-vc" {
		t.Fatalf("agent family filter kept %+v", got)
	}
	both := append(sessions, session(abs("/x"), "copilot-jb", "copilot-jb", recent))
	if got := FilterSessions(both, SessionFilter{Agent: "copilot"}); len(got) != 2 {
		t.Fatalf("both copilot editors should answer to \"copilot\": %+v", got)
	}
	if got := FilterSessions(both, SessionFilter{Agent: "copilot-jb"}); len(got) != 1 || got[0].PluginID != "copilot-jb" {
		t.Fatalf("naming one editor should keep only it: %+v", got)
	}
	// A name that is merely a prefix of another is not a family.
	if got := FilterSessions(both, SessionFilter{Agent: "cop"}); len(got) != 0 {
		t.Fatalf("a partial name matched: %+v", got)
	}
	if got := FilterSessions(sessions, SessionFilter{Since: time.Now().Add(-24 * time.Hour)}); len(got) != 2 {
		t.Fatalf("since filter kept %d sessions", len(got))
	}
}

// An unstarted fork is a full copy of its parent: keeping it would report every
// hit twice.
func TestFilterSessionsDropsEmptyForks(t *testing.T) {
	s := session(abs("/x"), "claude", "claude", time.Now())
	fork := s
	fork.EmptyFork = true
	if got := FilterSessions([]domain.Session{s, fork}, SessionFilter{}); len(got) != 1 || got[0].EmptyFork {
		t.Fatalf("empty fork survived: %+v", got)
	}
	if got := FilterSessions([]domain.Session{s, fork}, SessionFilter{IncludeEmptyForks: true}); len(got) != 2 {
		t.Fatalf("empty fork was dropped even when asked for: %+v", got)
	}
}

// A log that was deleted took its session out of every listing, although the
// cache still held what it takes to read one. The cache is asked for what the
// scan could not find.
func TestMergeDeletedLogs(t *testing.T) {
	scanned := []domain.Session{
		{PluginID: "claude", SessionID: "alive", SourceRef: domain.SessionRef{Source: "/logs/alive.jsonl"}},
	}
	cached := []domain.Session{
		{PluginID: "claude", SessionID: "alive", SourceRef: domain.SessionRef{Source: "/logs/alive.jsonl"}},
		{PluginID: "claude", SessionID: "gone", SourceRef: domain.SessionRef{Source: "/logs/gone.jsonl"},
			Status: domain.StatusRunning, PermissionWait: true},
		{PluginID: "codex", SessionID: "unscanned", SourceRef: domain.SessionRef{Source: "/logs/codex.jsonl"}},
		{PluginID: "claude", SessionID: "nosource"},
	}
	// codex did not scan successfully, so nothing of its is called deleted.
	out := MergeDeletedLogs(scanned, cached, map[string]bool{"claude": true, "codex": false})
	if len(out) != 2 {
		t.Fatalf("sessions=%d want 2: %+v", len(out), out)
	}
	if out[0].SessionID != "alive" || out[0].LogDeleted {
		t.Errorf("a scanned session should come back unchanged: %+v", out[0])
	}
	gone := out[1]
	if gone.SessionID != "gone" || !gone.LogDeleted {
		t.Errorf("the deleted session should be marked: %+v", gone)
	}
	// What the cache remembers of a running session is stale: nothing is watching
	// that process any more.
	if gone.Status != "" || gone.PermissionWait {
		t.Errorf("a remembered status should not be presented as live: %+v", gone)
	}
}

// A plugin that fails to start finds nothing, which must not be read as "every
// session was deleted".
func TestMergeDeletedLogsIgnoresAFailedPlugin(t *testing.T) {
	cached := []domain.Session{
		{PluginID: "claude", SessionID: "a", SourceRef: domain.SessionRef{Source: "/logs/a.jsonl"}},
		{PluginID: "claude", SessionID: "b", SourceRef: domain.SessionRef{Source: "/logs/b.jsonl"}},
	}
	if out := MergeDeletedLogs(nil, cached, map[string]bool{"claude": false}); len(out) != 0 {
		t.Errorf("a failed scan should add nothing: %+v", out)
	}
	if out := MergeDeletedLogs(nil, cached, map[string]bool{"claude": true}); len(out) != 2 {
		t.Errorf("a successful scan that found nothing means both logs are gone: %+v", out)
	}
}
