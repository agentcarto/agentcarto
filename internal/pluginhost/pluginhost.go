// Package pluginhost launches each configured agent plugin as a subprocess and assembles the
// plugin.Instance values the main program uses (each Impl is an RPC client).
package pluginhost

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/core/plugin"
)

// Hosted bundles the launched plugins together with their teardown. Warnings holds messages
// for plugins that failed to start and were skipped (binary missing, launch failure, Init
// failure, etc.).
type Hosted struct {
	Instances []plugin.Instance
	Warnings  []string
	launched  []*plugin.Launched
	closed    bool
}

// Close terminates every launched plugin process (idempotent). It must be called when the
// main program exits, and before the TUI's resume/fork handoff replaces the main process via
// syscall.Exec.
func (h *Hosted) Close() {
	if h == nil || h.closed {
		return
	}
	h.closed = true
	for _, l := range h.launched {
		l.Kill()
	}
	plugin.CleanupClients()
}

// Launch starts every enabled plugin from the config as a subprocess, then builds an Instance
// from the Descriptor that Init returns. If a plugin fails to start (binary missing, launch
// failure, or Init failure), it is recorded in Warnings and skipped, and the remaining
// plugins continue (graceful degradation): missing plugins don't prevent the others from
// being usable.
func Launch(c config.Config) (*Hosted, error) {
	h := &Hosted{}
	for _, p := range c.Plugins {
		if !p.Enabled {
			continue
		}
		inst, l, err := launchOne(p)
		if err != nil {
			if l != nil {
				l.Kill() // clean up an already-started process, e.g. when Init failed
			}
			h.Warnings = append(h.Warnings, fmt.Sprintf("plugin %s skipped: %v", p.ID, err))
			continue
		}
		h.launched = append(h.launched, l)
		h.Instances = append(h.Instances, inst)
	}
	return h, nil
}

// launchOne starts a single plugin and returns its Instance. On failure, if the process was
// already launched it also returns the *Launched so the caller can Kill it.
func launchOne(p config.Plugin) (plugin.Instance, *plugin.Launched, error) {
	bin, err := resolveBinary(p)
	if err != nil {
		return plugin.Instance{}, nil, err
	}
	l, err := plugin.Launch(bin)
	if err != nil {
		return plugin.Instance{}, nil, fmt.Errorf("launch %s: %w", bin, err)
	}
	opts := p.Options
	desc, err := l.API.Init(p.ID, &opts)
	if err != nil {
		return plugin.Instance{}, l, fmt.Errorf("init: %w", err)
	}
	return plugin.Instance{ID: p.ID, Color: p.Color, Descriptor: desc, Impl: l.API}, l, nil
}

// resolveBinary resolves the path to the plugin executable. Lookup order: the config's
// command → "agentcarto-plugin-<type>" in the same directory as the main executable → the
// same name on PATH.
func resolveBinary(p config.Plugin) (string, error) {
	if p.Command != "" {
		return p.Command, nil
	}
	name := "agentcarto-plugin-" + p.Type
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), name)
		if _, e := os.Stat(cand); e == nil {
			return cand, nil
		}
	}
	if pth, err := exec.LookPath(name); err == nil {
		return pth, nil
	}
	return "", fmt.Errorf("plugin binary %q not found (set plugins[].command, or place it beside agentcarto or on PATH)", name)
}
