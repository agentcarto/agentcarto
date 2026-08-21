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
		session(abs("/x"), "copilot-vc", "copilot", old),
	}
	if got := FilterSessions(sessions, SessionFilter{Agent: "CODEX"}); len(got) != 1 || got[0].PluginID != "codex" {
		t.Fatalf("agent filter kept %+v", got)
	}
	// The agent type matches too: a copilot session is reachable by either name.
	if got := FilterSessions(sessions, SessionFilter{Agent: "copilot"}); len(got) != 1 || got[0].PluginID != "copilot-vc" {
		t.Fatalf("agent type filter kept %+v", got)
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
