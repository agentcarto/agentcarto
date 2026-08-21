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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
		db := openCache(ctx, c, *noCache)
		defer closeCache(db)
		listCmd(ctx, a, c, db, args[1:], cmd == "active", os.Stdout)
	case "search":
		db := openCache(ctx, c, *noCache)
		defer closeCache(db)
		searchCmd(ctx, a, c, db, args[1:], os.Stdout)
	case "show":
		db := openCache(ctx, c, *noCache)
		defer closeCache(db)
		showCmd(ctx, a, c, db, args[1:], os.Stdout)
	case "help":
		usage(os.Stdout)
	case "":
		runTUI(ctx, a, c, host, *noCache)
	default:
		// Without this a mistyped subcommand starts the TUI, which fails with
		// "could not open a new TTY" wherever there is no terminal — a puzzle to
		// anyone (or anything) that just misspelled "show".
		fmt.Fprintf(os.Stderr, "agentcarto: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(1)
	}
}

// listCmd prints the scanned sessions, as columns for a person or as JSON for a
// program. The filters are the ones search takes, because the question behind
// "what was I doing here" is the same one — only without a query, which is why
// it cannot be answered by search.
//
// When active is true it also runs active detection and keeps only the sessions
// an agent is working on right now.
func listCmd(ctx context.Context, a *app.App, c config.Config, db *cache.DB, args []string, active bool, w io.Writer) {
	name := "list"
	if active {
		name = "active"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON instead of columns")
	dir := fs.String("cwd", "", `only sessions that ran in this directory or below it ("." for the current one)`)
	agent := fs.String("agent", "", "only this agent (plugin id or agent type)")
	since := fs.String("since", "", "only sessions updated since then (7d, 12h, 2026-08-01)")
	limit := fs.Int("limit", 0, "most sessions to print, newest first (0: all of them)")
	if rest := parseFlags(fs, args); len(rest) > 0 {
		fail(fmt.Errorf("%s: unexpected argument %q", name, rest[0]))
	}
	if *limit < 0 {
		fail(fmt.Errorf("%s: --limit %d: a count cannot be negative", name, *limit))
	}
	filter := app.SessionFilter{Agent: *agent, IncludeEmptyForks: true}
	if *dir != "" {
		abs, err := filepath.Abs(*dir)
		if err != nil {
			fail(fmt.Errorf("--cwd %q: %w", *dir, err))
		}
		filter.CWD = abs
	}
	if *since != "" {
		t, err := parseSince(*since, time.Now())
		if err != nil {
			fail(err)
		}
		filter.Since = t
	}

	sessions := app.FilterSessions(scanSessions(ctx, a, c, db), filter)
	if active {
		var e error
		sessions, e = a.DetectActive(ctx, sessions)
		if e != nil {
			fmt.Fprintln(os.Stderr, "active detection:", e)
		}
		var running []domain.Session
		for _, s := range sessions {
			if s.Status != "" {
				running = append(running, s)
			}
		}
		sessions = running
	}
	// The scan already returns them newest first; sorting here is what makes
	// --limit mean "the newest N" rather than "whatever the scan happened to
	// return first".
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt) })
	if *limit > 0 && len(sessions) > *limit {
		sessions = sessions[:*limit]
	}
	if *asJSON {
		rows := make([]listedSession, 0, len(sessions))
		for _, s := range sessions {
			rows = append(rows, listedSession{
				SessionID: s.SessionID, PluginID: s.PluginID, AgentType: s.AgentType,
				Title: s.Title, CWD: s.CWD, StartedAt: s.StartedAt, UpdatedAt: s.UpdatedAt,
				Source: s.SourceRef.Source, Status: string(s.Status),
			})
		}
		printJSON(w, struct {
			Sessions []listedSession `json:"sessions"`
		}{rows})
		return
	}
	for _, s := range sessions {
		fmt.Fprintf(w, "%-8s %-8s %-20s %-30s %s\n", s.PluginID, s.Status, short(s.SessionID, 20), short(s.CWD, 30), s.Title)
	}
}

// listedSession is a session without the hits: what `list --json` prints, with
// the same field names search uses so the two can be read by one reader.
type listedSession struct {
	SessionID string    `json:"session_id"`
	PluginID  string    `json:"plugin_id"`
	AgentType string    `json:"agent_type"`
	Title     string    `json:"title,omitempty"`
	CWD       string    `json:"cwd,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Source    string    `json:"source"`
	Status    string    `json:"status,omitempty"`
}

// usage lists the commands. The TUI is what agentcarto does when it is given
// nothing; the rest is for scripts and agents.
func usage(w io.Writer) {
	fmt.Fprint(w, `usage: agentcarto [--config FILE] [--no-cache] [command]

  (no command)              launch the TUI
  list [flags]              list sessions (--json, --cwd, --agent, --since, --limit)
  active [flags]            list running sessions (same flags)
  search [flags] QUERY      search sessions, printing JSON hits
  show [flags] SESSION      print a session's outline, or the turns asked for
  config validate           validate the config and list enabled plugins
  plugins list              list plugins and their capabilities
  doctor                    diagnose config, executables and storage
  cache stats|clear         inspect or drop the local cache
  help                      print this

Run "agentcarto search -h" or "agentcarto show -h" for their flags.
`)
}

// openCache opens the local cache unless it is switched off, degrading to no
// cache (and saying so) rather than failing: the cache only saves work.
func openCache(ctx context.Context, c config.Config, noCache bool) *cache.DB {
	if noCache || !c.Cache.Enabled {
		return nil
	}
	db, err := cache.Open("")
	if err != nil {
		// Say it out loud: silently losing the cache makes every run re-parse
		// everything, which looks like the command being slow for no reason.
		fmt.Fprintf(os.Stderr, "warning: cache disabled (open failed: %v)\n", err)
		return nil
	}
	return db
}

func closeCache(db *cache.DB) {
	if db != nil {
		_ = db.Close()
	}
}

// artifactStore hands the cache to app.BuildIndex without letting a nil *cache.DB
// through as a non-nil interface, which would panic on first use.
func artifactStore(db *cache.DB) app.ArtifactStore {
	if db == nil {
		return nil
	}
	return db
}

// scanSessions lists every session the plugins can see, warmed from the cache so
// a command does not re-parse sessions that have not changed. Empty forks are
// marked (not dropped) so each caller decides whether they belong in its answer.
func scanSessions(ctx context.Context, a *app.App, c config.Config, db *cache.DB) []domain.Session {
	var warm []domain.Session
	if db != nil {
		warm, _ = db.Load(ctx)
	}
	snap := a.Scan(ctx, warm, nil)
	for _, x := range snap.Errors {
		fmt.Fprintf(os.Stderr, "%s: %s: %v\n", x.PluginID, x.Reason, x.Err)
	}
	if db != nil {
		_ = db.Save(ctx, snap.Sessions)
		// The same housekeeping the TUI does after a scan. Without it, a machine
		// where agentcarto is only ever called from the command line keeps every
		// session it has ever seen and honours neither cache.max_age nor max_size.
		failed := map[string]bool{}
		for _, e := range snap.Errors {
			failed[e.PluginID] = true
		}
		successful := map[string]bool{}
		for _, p := range a.Catalog.Plugins {
			successful[p.ID] = !failed[p.ID]
		}
		_ = db.Prune(ctx, snap.Sessions, successful, time.Duration(c.Cache.MaxAge))
		_ = db.Enforce(ctx, int64(c.Cache.MaxSize))
	}
	return a.MarkEmptyForks(ctx, snap.Sessions)
}

// runTUI loads any cached sessions, runs the interactive TUI, and, if the TUI
// returns a launch command, hands off to it after shutting the plugin host down.
func runTUI(ctx context.Context, a *app.App, c config.Config, host *pluginhost.Hosted, noCache bool) {
	var cached []domain.Session
	db := openCache(ctx, c, noCache)
	defer closeCache(db)
	if db != nil {
		cached, _ = db.Load(ctx)
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
