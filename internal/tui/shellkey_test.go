package tui

import (
	"runtime"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/core/domain"
)

// setShellEnv points the platform's shell env var (SHELL, COMSPEC on Windows)
// at a fake path so tests can assert it is what gets launched.
func setShellEnv(t *testing.T, path string) {
	env := "SHELL"
	if runtime.GOOS == "windows" {
		env = "COMSPEC"
	}
	t.Setenv(env, path)
}

func shellModel(cwd string) Model {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "folder", app: app.Build(config.Config{}, nil), sessions: []domain.Session{
		{PluginID: "codex", SessionID: "s1", CWD: cwd, UpdatedAt: t0},
	}}
	m.filter()
	return m
}

// Pressing c on a header row hands off a shell in the group's cwd.
func TestShellKeyOnHeader(t *testing.T) {
	setShellEnv(t, "/opt/fancy/sh")
	dir := t.TempDir()
	m := shellModel(dir)
	updated, cmd := m.updateList(key("c")) // cursor 0 = header row
	m = updated.(Model)
	if m.launch == nil || m.launch.WorkingDirectory != dir || m.launch.Executable != "/opt/fancy/sh" {
		t.Fatalf("launch not set from header: %+v", m.launch)
	}
	if cmd == nil {
		t.Fatal("c must quit the TUI to hand off")
	}
}

// Pressing c on a session row hands off a shell in the session's cwd.
func TestShellKeyOnSession(t *testing.T) {
	setShellEnv(t, "/opt/fancy/sh")
	dir := t.TempDir()
	m := shellModel(dir)
	m.cursor = 1 // session row under the header
	updated, cmd := m.updateList(key("c"))
	m = updated.(Model)
	if m.launch == nil || m.launch.WorkingDirectory != dir {
		t.Fatalf("launch not set from session row: %+v", m.launch)
	}
	if cmd == nil {
		t.Fatal("c must quit the TUI to hand off")
	}
}

// A missing directory flashes an error instead of quitting.
func TestShellKeyMissingDirFlashes(t *testing.T) {
	m := shellModel("/nonexistent-agentcarto-test-dir")
	updated, cmd := m.updateList(key("c"))
	m = updated.(Model)
	if m.launch != nil || cmd != nil {
		t.Fatal("missing dir must not hand off")
	}
	if m.flash == "" {
		t.Fatal("missing dir should flash an error")
	}
}

// The detail view's c key hands off a shell in the open session's cwd.
func TestShellKeyInDetail(t *testing.T) {
	setShellEnv(t, "/opt/fancy/sh")
	dir := t.TempDir()
	s := domain.Session{PluginID: "codex", SessionID: "s1", CWD: dir}
	m := Model{app: app.Build(config.Config{}, nil), detailSession: &s}
	updated, cmd := m.updateDetail(key("c"))
	m = updated.(Model)
	if m.launch == nil || m.launch.WorkingDirectory != dir {
		t.Fatalf("launch not set from detail: %+v", m.launch)
	}
	if cmd == nil {
		t.Fatal("c must quit the TUI to hand off")
	}
}

// A session without a cwd (e.g. Copilot before inference) flashes instead of quitting.
func TestShellKeyEmptyCWDFlashes(t *testing.T) {
	s := domain.Session{PluginID: "copilot", SessionID: "s1"}
	m := Model{app: app.Build(config.Config{}, nil), detailSession: &s}
	updated, cmd := m.updateDetail(key("c"))
	m = updated.(Model)
	if m.launch != nil || cmd != nil {
		t.Fatal("empty cwd must not hand off")
	}
	if m.flash == "" {
		t.Fatal("empty cwd should flash an error")
	}
}
