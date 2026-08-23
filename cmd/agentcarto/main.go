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
	"github.com/mattn/go-runewidth"
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
		a.Store = artifactStore(db)
		listCmd(ctx, a, c, db, args[1:], cmd == "active", os.Stdout)
	case "search":
		db := openCache(ctx, c, *noCache)
		defer closeCache(db)
		a.Store = artifactStore(db)
		searchCmd(ctx, a, c, db, args[1:], os.Stdout)
	case "show":
		db := openCache(ctx, c, *noCache)
		defer closeCache(db)
		a.Store = artifactStore(db)
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
	format := fs.String("format", "table", "print as columns (table) or as JSON (json)")
	asJSON := fs.Bool("json", false, "the older spelling of --format json")
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

	all := scanSessions(ctx, a, c, db)
	sessions := app.FilterSessions(all, filter)
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
	// A listing narrowed to a directory that holds nothing is the same dead end a
	// narrowed search was: the work is usually one level up, because what gets
	// recorded is where the agent was started and not what it was working on.
	// `active` is left out — nothing running here is an answer in itself, and the
	// sessions outside the filter would have to be re-detected to be counted.
	note := ""
	if len(sessions) == 0 && !active {
		note = listElsewhere(all, filter)
	}
	if resolveFormat(fs, name, *format, *asJSON) == "json" {
		rows := make([]listedSession, 0, len(sessions))
		for _, s := range sessions {
			rows = append(rows, listedSession{
				SessionID: s.SessionID, PluginID: s.PluginID, AgentType: s.AgentType,
				Title: s.Title, CWD: s.CWD, StartedAt: s.StartedAt, UpdatedAt: s.UpdatedAt,
				Source: s.SourceRef.Source, Status: string(s.Status), LogDeleted: s.LogDeleted,
			})
		}
		printJSON(w, struct {
			Sessions []listedSession `json:"sessions"`
			Note     string          `json:"note,omitempty"`
		}{rows, note})
		return
	}
	if note != "" {
		fmt.Fprintln(w, note)
	}
	// The columns open the way a search result does — id, agent, when — so the two
	// listings read as one thing. The status column is only there for `active`,
	// where it is the answer; in `list` it would be an empty column on every row.
	for _, s := range sessions {
		status := ""
		if active {
			status = fmt.Sprintf("%-8s ", s.Status)
		}
		title := short(oneLine(s.Title), 60)
		if s.LogDeleted {
			title = "(log deleted) " + short(oneLine(s.Title), 46)
		}
		fmt.Fprintf(w, "%-8s %-8s %s%-10s %-30s %s\n",
			idPrefix(s.SessionID), s.PluginID, status, day(s.UpdatedAt), short(s.CWD, 30), title)
	}
}

// listElsewhere says how many sessions the filters left out and where they are,
// for a listing that came back empty. Unlike the same answer in a search this
// costs nothing to work out — there is no query, so no index to build — which is
// why it is computed whenever the listing is empty rather than only on request.
func listElsewhere(all []domain.Session, filter app.SessionFilter) string {
	rest := app.FilterSessions(all, app.SessionFilter{IncludeEmptyForks: true})
	if len(rest) == 0 {
		return "" // there are no sessions at all: the filters are not what is wrong
	}
	note := fmt.Sprintf("nothing here, but %d sessions exist without the filters", len(rest))
	// Where they are is the answer to "I narrowed by directory and found nothing".
	// It answers nothing about an agent or a date, where naming the directories
	// would only point away from the filter that is actually in the way.
	if filter.CWD != "" {
		byDir := map[string]int{}
		for _, s := range rest {
			byDir[s.CWD]++
		}
		note += " (" + topDirs(byDir, filter.CWD, 3) + ")"
	}
	return note
}

// day is the date a listing shows for a session, or nothing when the plugin
// records no times.
func day(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02")
}

// resolveFormat settles the --format / --json pair the listings take. --json is
// the older spelling of --format json and stays because it is written into
// scripts and into the past-sessions skill; asking for both at once is a
// mistake worth naming rather than quietly resolving.
func resolveFormat(fs *flag.FlagSet, cmd, format string, asJSON bool) string {
	if asJSON {
		if flagGiven(fs, "format") && format != "json" {
			fail(fmt.Errorf("%s: --json and --format %s ask for different things", cmd, format))
		}
		return "json"
	}
	if format != "table" && format != "json" {
		fail(fmt.Errorf("%s: --format %q: use table or json", cmd, format))
	}
	return format
}

// flagGiven reports whether the flag was set on the command line, as opposed to
// left at its default. A default that depends on another flag has to know the
// difference.
func flagGiven(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// idPrefix is the handle a table row hands to the next command. It is not
// elided: an id is given to `show` as a prefix, so these eight characters are
// something to copy rather than a shortened form of something else.
func idPrefix(id string) string {
	if r := []rune(id); len(r) > 8 {
		return string(r[:8])
	}
	return id
}

// oneLine folds a value onto the single line a table row has for it. A title is
// the first thing that was said in the session, and what was said can run to
// several lines.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

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
	// LogDeleted says the session outlived the log it was read from: it is listed
	// from the cache, and cannot be resumed, forked or relocated.
	LogDeleted bool `json:"log_deleted,omitempty"`
}

// usage lists the commands. The TUI is what agentcarto does when it is given
// nothing; the rest is for scripts and agents.
func usage(w io.Writer) {
	fmt.Fprint(w, `usage: agentcarto [--config FILE] [--no-cache] [command]

  (no command)              launch the TUI
  list [flags]              list sessions (--format, --cwd, --agent, --since, --limit)
  active [flags]            list running sessions (same flags)
  search [flags] QUERY      search sessions and print where the query was found
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
	failed := map[string]bool{}
	for _, e := range snap.Errors {
		failed[e.PluginID] = true
	}
	successful := map[string]bool{}
	for _, p := range a.Catalog.Plugins {
		successful[p.ID] = !failed[p.ID]
	}
	if db != nil {
		_ = db.Save(ctx, snap.Sessions)
		// The same housekeeping the TUI does after a scan. Without it, a machine
		// where agentcarto is only ever called from the command line keeps every
		// session it has ever seen and honours neither cache.max_age nor max_size.
		_ = db.Prune(ctx, snap.Sessions, successful, time.Duration(c.Cache.MaxAge))
		_ = db.Enforce(ctx, int64(c.Cache.MaxSize))
	}
	// The cache is asked what the scan could not find. A log that was deleted took
	// its session out of every listing until now, though what it takes to read it
	// was here all along.
	return app.MergeDeletedLogs(a.MarkEmptyForks(ctx, snap.Sessions), warm, successful)
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

// short cuts a value to n columns of terminal, marking the cut. It counts
// display width rather than runes: these logs are written in Japanese as often
// as in English, and a column that holds 30 runes of one holds 15 of the other
// — counting runes is what turns a table into wrapped paragraphs.
func short(s string, n int) string { return runewidth.Truncate(s, n, "…") }
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
