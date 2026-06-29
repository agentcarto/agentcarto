package app

import (
	"github.com/agentcarto/agentcarto/internal/catalog"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"testing"
)

// resumeStub satisfies only Resumer (it does not implement ExecutableProvider, so
// Availability's executable lookup is skipped and the test does not depend on PATH).
type resumeStub struct{ cmd domain.Command }

func (s *resumeStub) ResumeCommand(domain.Session) (domain.Command, error) { return s.cmd, nil }

func resumeApp(caps domain.Capabilities, cmd domain.Command) *App {
	return &App{Catalog: catalog.Catalog{Plugins: []plugin.Instance{{
		ID:         "p",
		Descriptor: plugin.Descriptor{DisplayName: "P", Capabilities: caps},
		Impl:       &resumeStub{cmd: cmd},
	}}}}
}

// ResumeCommand only builds the launch command; it does not start the process. To
// avoid breaking the terminal handoff, the real launch is deferred to after the TUI exits.
func TestResumeCommandReturnsPluginCommand(t *testing.T) {
	want := domain.Command{Executable: "claude", Args: []string{"--resume", "sid"}, WorkingDirectory: "/w"}
	a := resumeApp(domain.Capabilities{Resume: true}, want)
	got, err := a.ResumeCommand(domain.Session{PluginID: "p", SessionID: "sid", CWD: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Executable != want.Executable || got.WorkingDirectory != want.WorkingDirectory || len(got.Args) != 2 {
		t.Fatalf("got %#v", got)
	}
}

// Refuse a double-attach to a running session (resume is for non-running sessions only).
func TestResumeCommandRefusesActiveSession(t *testing.T) {
	a := resumeApp(domain.Capabilities{Resume: true}, domain.Command{})
	if _, err := a.ResumeCommand(domain.Session{PluginID: "p", Status: domain.StatusRunning}); err == nil {
		t.Fatal("expected refusal for active session")
	}
}

// A read-only plugin without the resume capability returns no launch command.
func TestResumeCommandRefusesReadOnly(t *testing.T) {
	a := resumeApp(domain.Capabilities{}, domain.Command{})
	if _, err := a.ResumeCommand(domain.Session{PluginID: "p"}); err == nil {
		t.Fatal("expected refusal for read-only plugin")
	}
}

// An Unresumable fork (claude's native subagent fork) is not offered for resume and
// refuses to launch. Its SessionID is a synthetic ID that is not a valid `--resume`
// target, so we don't let a dead-end command launch.
func TestResumeCommandRefusesUnresumableFork(t *testing.T) {
	a := resumeApp(domain.Capabilities{Resume: true}, domain.Command{})
	s := domain.Session{PluginID: "p", SessionID: "agent-x", Unresumable: true}
	if av := a.Availability(s, "resume"); av.Enabled {
		t.Fatal("expected resume to be unavailable for unresumable fork")
	}
	if _, err := a.ResumeCommand(s); err == nil {
		t.Fatal("expected refusal for unresumable fork")
	}
}
