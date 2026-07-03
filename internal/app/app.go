package app

import (
	"context"
	"fmt"
	"github.com/agentcarto/agentcarto/internal/catalog"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/platform"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"github.com/agentcarto/core/transaction"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type App struct {
	Config      config.Config
	Catalog     catalog.Catalog
	mu          sync.Mutex
	lastRunning map[domain.SessionKey]time.Time
}

// Build assembles an App from a set of already-started plugin Instances. The
// caller (pluginhost) launches the plugin processes and passes in their
// Instances (whose Impl is the RPC client).
func Build(c config.Config, instances []plugin.Instance) *App {
	return &App{Config: c, Catalog: catalog.Catalog{Plugins: instances}, lastRunning: map[domain.SessionKey]time.Time{}}
}
func (a *App) Scan(ctx context.Context, warm []domain.Session, dead map[string]string) domain.Snapshot {
	return a.Catalog.Scan(ctx, warm, dead)
}
func (a *App) Conversation(ctx context.Context, s domain.Session) (*domain.Conversation, error) {
	p, ok := a.Catalog.Plugin(s.PluginID)
	if !ok {
		return nil, fmt.Errorf("plugin %q unavailable", s.PluginID)
	}
	l, ok := p.Impl.(plugin.ConversationLoader)
	if !ok {
		return nil, fmt.Errorf("%s sessions are read-only", p.Descriptor.DisplayName)
	}
	return l.LoadConversation(ctx, s.SourceRef)
}
func (a *App) Availability(s domain.Session, action string) domain.ActionAvailability {
	p, ok := a.Catalog.Plugin(s.PluginID)
	if !ok {
		return domain.ActionAvailability{Reason: "plugin unavailable"}
	}
	caps := p.Descriptor.Capabilities
	enabled := map[string]bool{"open": caps.Conversation, "resume": caps.Resume, "fork": caps.Rewind, "relocate": caps.Relocate}[action]
	if !enabled {
		return domain.ActionAvailability{Reason: fmt.Sprintf("%s sessions are read-only; %s is unavailable", p.Descriptor.DisplayName, action)}
	}
	if action == "resume" && s.Unresumable {
		return domain.ActionAvailability{Reason: "fork has no resumable session id"}
	}
	if s.Status != "" && (action == "resume" || action == "relocate") {
		return domain.ActionAvailability{Reason: "session is active"}
	}
	if action == "resume" || action == "fork" {
		if exe := p.Descriptor.Executable; exe != "" {
			if _, e := exec.LookPath(exe); e != nil {
				return domain.ActionAvailability{Reason: fmt.Sprintf("%s executable unavailable: %v", p.Descriptor.DisplayName, e)}
			}
		}
	}
	return domain.ActionAvailability{Enabled: true}
}

// ResumeCommand builds the launch command for a resume operation (it does not
// start the process). Launching an interactive child process from inside the TUI
// while it still holds the alt-screen and raw mode would corrupt the terminal
// handoff, racing with bubbletea's teardown. The actual launch happens after the
// TUI has fully exited, when the caller replaces the current process via
// syscall.Exec.
func (a *App) ResumeCommand(s domain.Session) (domain.Command, error) {
	if x := a.Availability(s, "resume"); !x.Enabled {
		return domain.Command{}, fmt.Errorf("%s", x.Reason)
	}
	p, _ := a.Catalog.Plugin(s.PluginID)
	return p.Impl.(plugin.Resumer).ResumeCommand(s)
}

// ShellCommand builds the launch command that opens the user's shell in the
// session's working directory, handed off after the TUI exits like a resume.
// It is plugin-independent and deliberately skips the resume Availability
// guards: a running or unresumable session's directory is still reachable.
func (a *App) ShellCommand(cwd string) (domain.Command, error) {
	return platform.ShellCommand(cwd)
}
func (a *App) Fork(ctx context.Context, s domain.Session, t domain.ForkTarget) (domain.MutationPlan, domain.Command, error) {
	if x := a.Availability(s, "fork"); !x.Enabled {
		return domain.MutationPlan{}, domain.Command{}, fmt.Errorf("%s", x.Reason)
	}
	p, _ := a.Catalog.Plugin(s.PluginID)
	return p.Impl.(plugin.Rewinder).PlanFork(ctx, s, t)
}

// ApplyForkPlan applies only the file changes of a fork (it does not start the
// process). The launch command returned by Fork is handed off after the TUI exits.
func (a *App) ApplyForkPlan(ctx context.Context, p domain.MutationPlan) error {
	_, e := transaction.Apply(ctx, p)
	return e
}
func (a *App) Relocate(ctx context.Context, old, new string, sessions []domain.Session) (domain.MutationResult, error) {
	if old == new {
		return domain.MutationResult{}, fmt.Errorf("old and new paths are identical")
	}
	for _, s := range sessions {
		if s.CWD == old && s.Status != "" {
			return domain.MutationResult{}, fmt.Errorf("active session %s prevents relocation", s.SessionID)
		}
	}
	var all domain.MutationResult
	for _, inst := range a.Catalog.Plugins {
		if !inst.Descriptor.Capabilities.Relocate {
			continue
		}
		plan, e := inst.Impl.(plugin.Relocator).PlanRelocate(ctx, old, new, sessions)
		if e != nil {
			return all, e
		}
		if len(plan.Writes) == 0 && len(plan.Moves) == 0 {
			continue
		}
		r, e := transaction.Apply(ctx, plan)
		all.Completed = append(all.Completed, r.Completed...)
		all.Pending = append(all.Pending, r.Pending...)
		all.RolledBack = append(all.RolledBack, r.RolledBack...)
		all.Warnings = append(all.Warnings, r.Warnings...)
		if e != nil {
			return all, e
		}
	}
	return all, nil
}
func (a *App) DetectActive(ctx context.Context, sessions []domain.Session) ([]domain.Session, error) {
	ps, e := platform.Processes(ctx, a.processEnrich())
	if e != nil {
		return sessions, e
	}
	by := map[string][]domain.Session{}
	for _, s := range sessions {
		by[s.PluginID] = append(by[s.PluginID], s)
	}
	out := make([]domain.Session, 0, len(sessions))
	for _, inst := range a.Catalog.Plugins {
		ss := by[inst.ID]
		if !inst.Descriptor.Capabilities.Active {
			out = append(out, ss...)
			continue
		}
		xs, err := inst.Impl.(plugin.ActiveMatcher).DetectActive(ctx, ss, ps)
		if err != nil {
			return sessions, fmt.Errorf("%s active detection: %w", inst.ID, err)
		}
		out = append(out, xs...)
	}
	a.debounceRunning(out)
	return out, nil
}

// debounceRunning smooths status flicker. A session that reported Running within
// the last 3 seconds is held at Running even if it now reports a different
// non-blank status, so a momentary dip doesn't make an active session flicker.
// A blank status clears the debounce entry immediately, and entries for sessions
// that are no longer present are dropped.
func (a *App) debounceRunning(out []domain.Session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	seen := map[domain.SessionKey]bool{}
	for i := range out {
		k := out[i].Key()
		seen[k] = true
		switch {
		case out[i].Status == domain.StatusRunning:
			a.lastRunning[k] = now
		case out[i].Status != "":
			if t, ok := a.lastRunning[k]; ok && now.Sub(t) < 3*time.Second {
				out[i].Status = domain.StatusRunning
			}
		default:
			delete(a.lastRunning, k)
		}
	}
	for k := range a.lastRunning {
		if !seen[k] {
			delete(a.lastRunning, k)
		}
	}
}

// processEnrich returns a predicate deciding which processes should have their
// OpenFiles/Cwd collected. The candidate names are the executable basenames of
// plugins with the Active capability; a process matches if its Name or one of its
// argument basenames (including after stripping the extension) is among them. If
// there are no candidates it returns nil, so every process is enriched as before.
func (a *App) processEnrich() func(name string, args []string) bool {
	names := map[string]bool{}
	for _, inst := range a.Catalog.Plugins {
		if !inst.Descriptor.Capabilities.Active {
			continue
		}
		if exe := inst.Descriptor.Executable; exe != "" {
			if b := filepath.Base(exe); b != "" && b != "." {
				names[b] = true
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	return func(name string, args []string) bool { return matchesExecNames(names, name, args) }
}

// matchesExecNames reports whether the basename of Name or any argument (including
// after stripping the extension) is in the candidate name set. This matches a
// node-launched "claude.js" against "claude".
func matchesExecNames(names map[string]bool, name string, args []string) bool {
	for _, tok := range append([]string{name}, args...) {
		b := filepath.Base(tok)
		if names[b] || names[strings.TrimSuffix(b, filepath.Ext(b))] {
			return true
		}
	}
	return false
}
