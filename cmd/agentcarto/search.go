package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/search"
	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

// searchResult is the document `agentcarto search` prints. It is meant to be
// read by a program — an agent looking through its own past sessions — so the
// session fields are the ones `show` takes, and the counts say what was left
// out rather than trailing off silently.
type searchResult struct {
	Query    string          `json:"query"`
	Scanned  int             `json:"scanned"`
	Matched  int             `json:"matched"`
	Sessions []searchSession `json:"sessions"`
	// Elsewhere counts the sessions the query matches outside the filters, and is
	// reported only when the filtered search found nothing. A search narrowed to
	// the current directory comes up empty often enough — the work was done one
	// directory up, or by another agent — that an empty result with no hint is
	// misread as "this was never discussed".
	Elsewhere int    `json:"elsewhere,omitempty"`
	Note      string `json:"note,omitempty"`
}

type searchSession struct {
	SessionID string    `json:"session_id"`
	PluginID  string    `json:"plugin_id"`
	AgentType string    `json:"agent_type"`
	Title     string    `json:"title,omitempty"`
	CWD       string    `json:"cwd,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Source    string    `json:"source"`
	// Match is "content" when the query was found in the conversation, "metadata"
	// when only the title, path, agent or id matched — a session with no hits is
	// then explained rather than looking like a bug — and "unknown" when the
	// conversation could not be read, which Error then describes.
	Match    string       `json:"match"`
	Hits     []search.Hit `json:"hits"`
	MoreHits int          `json:"more_hits,omitempty"`
	Error    string       `json:"error,omitempty"`
}

func searchCmd(ctx context.Context, a *app.App, cfg config.Config, db *cache.DB, args []string, w io.Writer) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	dir := fs.String("cwd", "", `only sessions that ran in this directory or below it ("." for the current one)`)
	agent := fs.String("agent", "", "only this agent (plugin id or agent type)")
	since := fs.String("since", "", "only sessions updated since then (7d, 12h, 2026-08-01)")
	limit := fs.Int("limit", 10, "most sessions to list (0: all of them)")
	perSession := fs.Int("hits-per-session", 3, "most hits to list per session, newest kept (0: all of them)")
	width := fs.Int("context", search.DefaultContext, "characters of context on either side of a hit")
	asRegexp := fs.Bool("regex", false, `read the query as a regular expression (RE2), case-insensitive: "cache|キャッシュ"`)
	query := strings.TrimSpace(strings.Join(parseFlags(fs, args), " "))
	if query == "" {
		fail(fmt.Errorf("search: a query is required (agentcarto search --cwd . \"handoff\")"))
	}
	q := search.NewQuery(query)
	if *asRegexp {
		var err error
		if q, err = search.NewRegexpQuery(query); err != nil {
			fail(fmt.Errorf("search: %w", err))
		}
	}
	// A negative count is a typo, not a request for everything: silently treating
	// it as "no limit" prints every session there is.
	for _, f := range []struct {
		name string
		v    int
	}{{"--limit", *limit}, {"--hits-per-session", *perSession}, {"--context", *width}} {
		if f.v < 0 {
			fail(fmt.Errorf("search: %s %d: a count cannot be negative", f.name, f.v))
		}
	}
	filter := app.SessionFilter{Agent: *agent}
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

	all := scanSessions(ctx, a, cfg, db)
	sessions := app.FilterSessions(all, filter)
	defer slowNotice(fmt.Sprintf("still working: %d sessions to go through (the first run parses them, later runs read the cache)", len(sessions)))()
	// The index decides which sessions to open at all: it is either cached or
	// built once here, and opening every session to search it would make the
	// command too slow to reach for.
	idx, _ := a.BuildIndex(ctx, sessions, artifactStore(db), nil, nil)
	if db != nil {
		// Drop a previous generation of the index, which nothing else collects.
		_ = db.DropArtifactsExcept(ctx, app.SearchArtifactKind)
	}
	var matched []domain.Session
	for _, s := range sessions {
		if idx.Match(s, q) {
			matched = append(matched, s)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].UpdatedAt.After(matched[j].UpdatedAt) })

	result := searchResult{Query: query, Scanned: len(all), Matched: len(matched), Sessions: []searchSession{}}
	listed := matched
	if *limit > 0 && len(listed) > *limit {
		listed = listed[:*limit]
		result.Note = fmt.Sprintf("%d sessions matched; listing the %d most recently updated (raise --limit for more)", len(matched), len(listed))
	}
	for _, s := range listed {
		row := sessionHits(ctx, a, s, q, *perSession, *width)
		if row.Match == "metadata" && q.IsRegexp() && !search.MatchesMetadata(s, q) {
			// The index holds a session's text as one run, so an anchored pattern
			// can match it there and nowhere in a single message. Such a session
			// has nothing to show and is dropped rather than listed empty.
			result.Matched--
			continue
		}
		result.Sessions = append(result.Sessions, row)
	}
	if len(matched) == 0 && filter != (app.SessionFilter{}) {
		if n, where := elsewhere(ctx, a, db, all, sessions, q, filter.CWD); n > 0 {
			result.Elsewhere, result.Note = n, fmt.Sprintf("nothing here, but %d sessions match without the filters (%s)", n, where)
		}
	}
	printJSON(w, result)
}

// slowNotice says on stderr what is taking so long, but only once it has taken
// long enough to look like a hang: a first run over a thousand sessions parses
// all of them, which is half a minute of silence otherwise. The returned
// function stops the notice.
func slowNotice(msg string) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(2 * time.Second):
			fmt.Fprintln(os.Stderr, "agentcarto:", msg)
		case <-done:
		}
	}()
	return func() { close(done) }
}

// elsewhere counts the sessions the query matches among those the filters left
// out, and names where they are. It is a second index build, so it runs only
// when the filtered search found nothing at all — which is exactly when the
// answer is worth its cost.
func elsewhere(ctx context.Context, a *app.App, db *cache.DB, all, kept []domain.Session, q search.Query, dir string) (int, string) {
	inFilter := make(map[string]bool, len(kept))
	for _, s := range kept {
		inFilter[s.SourceRef.Source] = true
	}
	var rest []domain.Session
	for _, s := range app.FilterSessions(all, app.SessionFilter{}) {
		if !inFilter[s.SourceRef.Source] {
			rest = append(rest, s)
		}
	}
	idx, _ := a.BuildIndex(ctx, rest, artifactStore(db), nil, nil)
	count := 0
	byDir := map[string]int{}
	for _, s := range rest {
		if idx.Match(s, q) {
			count++
			byDir[s.CWD]++
		}
	}
	return count, topDirs(byDir, dir, 3)
}

// topDirs names the directories holding the matches. A directory that contains
// the one that was searched comes first however few matches it holds: when a
// search of ./sub finds nothing, "the work was done one level up" is the answer
// being looked for, and it would lose a popularity contest against unrelated
// projects.
func topDirs(byDir map[string]int, searched string, max int) string {
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	encloses := func(d string) bool { return searched != "" && d != "" && app.UnderDir(searched, d) }
	sort.Slice(dirs, func(i, j int) bool {
		if encloses(dirs[i]) != encloses(dirs[j]) {
			return encloses(dirs[i])
		}
		if byDir[dirs[i]] != byDir[dirs[j]] {
			return byDir[dirs[i]] > byDir[dirs[j]]
		}
		return dirs[i] < dirs[j]
	})
	var parts []string
	for i, d := range dirs {
		if i == max {
			parts = append(parts, fmt.Sprintf("and %d more directories", len(dirs)-i))
			break
		}
		if d == "" {
			d = "(no recorded directory)"
		}
		parts = append(parts, fmt.Sprintf("%s: %d", d, byDir[d]))
	}
	return strings.Join(parts, ", ")
}

// sessionHits opens one session and locates the query inside it. A session that
// matched on its title or path alone has no hits to show, which the Match field
// spells out.
func sessionHits(ctx context.Context, a *app.App, s domain.Session, q search.Query, max, width int) searchSession {
	row := searchSession{
		SessionID: s.SessionID, PluginID: s.PluginID, AgentType: s.AgentType,
		Title: s.Title, CWD: s.CWD, StartedAt: s.StartedAt, UpdatedAt: s.UpdatedAt,
		Source: s.SourceRef.Source, Match: "metadata", Hits: []search.Hit{},
	}
	conv, err := a.Conversation(ctx, s)
	if err != nil || conv == nil {
		// The index matched, but without the conversation there is no telling
		// whether it matched the text or the metadata. Say so rather than pick one.
		row.Match = "unknown"
		if err != nil {
			row.Error = err.Error()
		} else {
			row.Error = "the plugin returned no conversation"
		}
		return row
	}
	hits, total := search.Hits(*conv, transcript.Turns(*conv, conv.ActivePath()), q,
		search.HitOptions{Max: max, Context: width})
	if total > 0 {
		row.Match = "content"
		row.Hits = hits
		row.MoreHits = total - len(hits)
	}
	return row
}

// parseFlags parses args in which flags and positional arguments are mixed,
// which the flag package stops at by default. An agent writing
// `search "handoff" --cwd .` should not silently lose the filter.
func parseFlags(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			fail(err)
		}
		args = fs.Args()
		if len(args) > 0 {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}
	return positional
}

// parseSince reads a point in time written as an age ("7d", "12h", "90m") or as
// a date ("2026-08-01", read in the local time zone). Go's own duration syntax
// has no day unit, and an age in hours is not how anyone thinks about a log.
func parseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	for _, unit := range []struct {
		suffix string
		days   int
		name   string
	}{{"d", 1, "days"}, {"w", 7, "weeks"}} {
		n, ok := strings.CutSuffix(s, unit.suffix)
		if !ok {
			continue
		}
		count, err := strconv.Atoi(n)
		if err != nil || count < 0 {
			return time.Time{}, fmt.Errorf("--since %q: not a number of %s", s, unit.name)
		}
		return notFuture(now.AddDate(0, 0, -unit.days*count), now), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q: use 7d, 2w, 12h or 2026-08-01", s)
	}
	return notFuture(now.Add(-d), now), nil
}

// notFuture keeps an age from wrapping round. Subtracting a few hundred million
// days overflows the clock and lands in the future, which would quietly select
// nothing at all; an age that large means "as far back as there is", so it
// becomes no lower bound.
func notFuture(t, now time.Time) time.Time {
	if t.After(now) {
		return time.Time{}
	}
	return t
}

func printJSON(w io.Writer, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Fprintln(w, string(b))
}
