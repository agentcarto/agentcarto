package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/platform"
	"github.com/agentcarto/agentcarto/internal/pluginhost"
	"github.com/agentcarto/agentcarto/internal/tui"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"sort"
	"strings"
)

func fail(e error) { fmt.Fprintln(os.Stderr, "agentcarto:", e); os.Exit(1) }
func main() {
	fs := flag.NewFlagSet("agentcarto", flag.ExitOnError)
	cfgPath := fs.String("config", "", "additional configuration file")
	noCache := fs.Bool("no-cache", false, "disable persistent cache")
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()
	c, e := config.Load(*cfgPath)
	if e != nil {
		fail(e)
	}
	host, e := pluginhost.Launch(c)
	if e != nil {
		fail(e)
	}
	defer host.Close()
	for _, w := range host.Warnings {
		fmt.Fprintln(os.Stderr, "agentcarto: warning:", w)
	}
	a := app.Build(c, host.Instances)
	ctx := context.Background()
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "config":
		configCmd(a, args[1:])
	case "plugins":
		pluginsCmd(a)
	case "cache":
		cacheCmd(ctx, args[1:])
	case "doctor":
		doctor(a)
	case "list", "active":
		listCmd(ctx, a, cmd == "active")
	default:
		runTUI(ctx, a, c, host, *noCache)
	}
}

// listCmd prints the scanned sessions. When active is true it also runs active
// detection and limits output to sessions with a non-blank status.
func listCmd(ctx context.Context, a *app.App, active bool) {
	snap := a.Scan(ctx, nil, nil)
	if active {
		var e error
		snap.Sessions, e = a.DetectActive(ctx, snap.Sessions)
		if e != nil {
			fmt.Fprintln(os.Stderr, "active detection:", e)
		}
	}
	for _, s := range snap.Sessions {
		if active && s.Status == "" {
			continue
		}
		fmt.Printf("%-8s %-8s %-20s %-30s %s\n", s.PluginID, s.Status, short(s.SessionID, 20), short(s.CWD, 30), s.Title)
	}
	for _, x := range snap.Errors {
		fmt.Fprintf(os.Stderr, "%s: %s: %v\n", x.PluginID, x.Reason, x.Err)
	}
}

// runTUI loads any cached sessions, runs the interactive TUI, and, if the TUI
// returns a launch command, hands off to it after shutting the plugin host down.
func runTUI(ctx context.Context, a *app.App, c config.Config, host *pluginhost.Hosted, noCache bool) {
	var cached []domain.Session
	var db *cache.DB
	if !noCache && c.Cache.Enabled {
		if d, err := cache.Open(""); err == nil {
			db = d
			defer db.Close()
			cached, _ = db.Load(ctx)
		} else {
			// Degrade to running without a cache, but say so: silently losing the
			// cache makes every launch re-parse everything.
			fmt.Fprintf(os.Stderr, "warning: cache disabled (open failed: %v)\n", err)
		}
	}
	if db != nil && len(cached) == 0 {
		snap := a.Scan(ctx, nil, nil)
		_ = db.Save(ctx, snap.Sessions)
		cached = snap.Sessions
	}
	launch, e := tui.Run(a, cached, db)
	if e != nil {
		fail(e)
	}
	if launch == nil {
		return
	}
	// syscall.Exec replaces this process, so deferred calls never run. Shut the
	// plugin processes down explicitly before handing off so they aren't orphaned.
	host.Close()
	// Once the TUI has fully restored the terminal, hand control to the resume/fork
	// launch command (on Unix this replaces the current process, so on success it
	// does not return here).
	if e := platform.Handoff(*launch); e != nil {
		fail(e)
	}
}
func short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
func configCmd(a *app.App, args []string) {
	if len(args) == 0 {
		fail(fmt.Errorf("config requires validate or print"))
	}
	switch args[0] {
	case "validate":
		// Reaching here means config.Load's syntax/range validation (config.Validate)
		// and app.Build's per-plugin type / capability / options validation have already
		// passed. List the resolved, enabled plugins to make visible what was validated.
		ps := append([]plugin.Instance(nil), a.Catalog.Plugins...)
		sort.Slice(ps, func(i, j int) bool { return ps[i].ID < ps[j].ID })
		fmt.Printf("configuration is valid (%d plugin(s) enabled)\n", len(ps))
		for _, p := range ps {
			fmt.Printf("  %-12s type=%-12s %s\n", p.ID, p.Descriptor.Type, p.Descriptor.DisplayName)
		}
	case "print":
		b, e := yaml.Marshal(a.Config)
		if e != nil {
			fail(e)
		}
		_, _ = os.Stdout.Write(b)
	default:
		fail(fmt.Errorf("unknown config command %q", args[0]))
	}
}
func pluginsCmd(a *app.App) {
	ps := append([]plugin.Instance(nil), a.Catalog.Plugins...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].ID < ps[j].ID })
	for _, p := range ps {
		c := p.Descriptor.Capabilities
		fmt.Printf("%-12s %-20s scan=%t conversation=%t active=%t resume=%t rewind=%t relocate=%t\n", p.ID, p.Descriptor.DisplayName, c.Scan, c.Conversation, c.Active, c.Resume, c.Rewind, c.Relocate)
	}
}
func cacheCmd(ctx context.Context, args []string) {
	if len(args) == 0 {
		fail(fmt.Errorf("cache requires stats or clear"))
	}
	switch args[0] {
	case "clear":
		if e := cache.Clear(""); e != nil {
			fail(e)
		}
		fmt.Println("cache cleared")
	case "stats":
		d, e := cache.Open("")
		if e != nil {
			fail(e)
		}
		defer d.Close()
		n, size, e := d.Stats(ctx)
		if e != nil {
			fail(e)
		}
		fmt.Printf("entries: %d\nsize: %d bytes\n", n, size)
	default:
		fail(fmt.Errorf("unknown cache command %q", args[0]))
	}
}
func doctor(a *app.App) {
	fmt.Printf("config: %s\ncache: %s\n", config.UserPath(), cache.Path())
	// Index the plugins that started up by id. Enabled plugins from the config that
	// failed to start are reported explicitly as "plugin binary missing / failed to
	// start" (the ones skipped by graceful degradation).
	loaded := map[string]plugin.Instance{}
	for _, p := range a.Catalog.Plugins {
		loaded[p.ID] = p
	}
	for _, cp := range a.Config.Plugins {
		if !cp.Enabled {
			continue
		}
		p, ok := loaded[cp.ID]
		if !ok {
			fmt.Printf("%-12s unavailable (plugin executable not found or failed to start)\n", cp.ID)
			continue
		}
		state := "ok"
		if p.Descriptor.Capabilities.Resume {
			exe := p.Descriptor.Executable
			if exe == "" {
				exe = executable(p)
			}
			if _, e := exec.LookPath(exe); e != nil {
				state = "resume unavailable: " + e.Error()
			}
		}
		fmt.Printf("%-12s %s\n", p.ID, state)
	}
}
func executable(p plugin.Instance) string {
	f := strings.Fields(p.Descriptor.DisplayName)
	if len(f) == 0 {
		return ""
	}
	return strings.ToLower(f[0])
}
