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
	"github.com/agentcarto/agentcarto/internal/summary"
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
	Elsewhere int `json:"elsewhere,omitempty"`
	// MetaSuppressed counts the sessions left out for having only searched for the
	// query rather than worked on it (--include-meta keeps them). It is what was
	// dropped while collecting --limit sessions, not every such session there is:
	// the ones further down the order are never opened, so they cannot be counted.
	MetaSuppressed int    `json:"meta_suppressed,omitempty"`
	Note           string `json:"note,omitempty"`
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
	// Match is "content" when the query was found in the conversation, "summary"
	// when it was found only in the generated summaries (the session said it in
	// other words, or can no longer be read at all), "metadata" when only the
	// title, path, agent or id matched — a session with no hits is then explained
	// rather than looking like a bug — and "unknown" when the conversation could
	// not be read and there was no summary either, which Error then describes.
	//
	// In a "summary" row the hits are generated text, not what was said. Turn 0
	// is the session's own summary rather than a turn.
	Match string       `json:"match"`
	Hits  []search.Hit `json:"hits"`
	// TotalHits is how many events the query was found in, of which Hits shows at
	// most --hits-per-session. It counts events and not occurrences — an event
	// that says the word ten times is one hit — which is why a two-turn session
	// can hold a hundred of them.
	TotalHits int    `json:"total_hits"`
	Error     string `json:"error,omitempty"`
	// LogDeleted says the log this session was read from is no longer on disk:
	// what is reported came from the cache. Such a session cannot be resumed,
	// forked or relocated, only read.
	LogDeleted bool `json:"log_deleted,omitempty"`
	// rank orders the listed sessions, and is not part of the JSON: it is a number
	// that only means something next to the other results of the same search.
	rank int
	// meta says the session only ran agentcarto over the query and never worked on
	// it. Not part of the JSON either: such a session is left out rather than
	// labelled, and --include-meta lists it like any other.
	meta bool
}

func searchCmd(ctx context.Context, a *app.App, cfg config.Config, db *cache.DB, args []string, w io.Writer) {
	// Deferred so the results are printed first — see listCmd.
	defer StartSummaryWorker(cfg, db)
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	format := fs.String("format", "table", "print as lines (table) or as JSON (json)")
	asJSON := fs.Bool("json", false, "the older spelling of --format json")
	dir := fs.String("cwd", "", `only sessions that ran in this directory or below it ("." for the current one)`)
	agent := fs.String("agent", "", "only this agent (plugin id or agent type)")
	since := fs.String("since", "", "only sessions updated since then (7d, 12h, 2026-08-01)")
	limit := fs.Int("limit", 10, "most sessions to list, most relevant first (0: all of them)")
	perSession := fs.Int("hits-per-session", 3, "most hits to list per session, newest kept (0: all of them)")
	width := fs.Int("context", search.DefaultContext, "characters of context on either side of a hit")
	asRegexp := fs.Bool("regex", false, `read the query as a regular expression (RE2), case-insensitive: "cache|キャッシュ"`)
	includeMeta := fs.Bool("include-meta", false, "keep the sessions that only ran agentcarto over the query")
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
	out := resolveFormat(fs, "search", *format, *asJSON)
	// A table gives a hit one line, and a line that wraps three times costs more
	// than the sentence it shows is worth — so a table narrows the snippet twice
	// over, in runes when it is cut out of the message and in columns when it is
	// printed. Asking for a width means it: both limits then step aside, since the
	// caller is the one who knows how wide their terminal is.
	snippet := 0
	if out == "table" && !flagGiven(fs, "context") {
		*width, snippet = search.TableContext, tableSnippet
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
		_ = db.DropSupersededArtifacts(ctx, app.SearchArtifactKind, app.ConversationArtifactKind)
	}
	// Generated summaries are searched beside the index, not inside it. The index
	// is cached against a session's fingerprint, and summaries arrive afterwards
	// and independently — folded in, they would be missing from every index built
	// before them and dropped from every one rebuilt after the log grew.
	//
	// This is the only text a session that has lost both its log and its cached
	// conversation still has. Without it such a session is findable by its title
	// and working directory alone.
	summaryText := map[domain.SessionKey]string{}
	if db != nil {
		summaryText = db.SummaryTexts(ctx)
	}
	var matched []domain.Session
	bySummary := map[domain.SessionKey]bool{}
	for _, s := range sessions {
		inIndex := idx.Match(s, q)
		// The empty string is not a session with no summaries saying nothing — it
		// is a session with no summaries. A pattern that matches empty text would
		// otherwise pull in every one of them.
		text := summaryText[s.Key()]
		if !inIndex && (text == "" || !q.MatchesText(text)) {
			continue
		}
		if !inIndex {
			bySummary[s.Key()] = true
		}
		matched = append(matched, s)
	}
	// Relevance decides the order, and the clock only separates sessions the query
	// cannot tell apart. The session worth opening is the one that keeps coming
	// back to the subject, which is rarely the one that was touched last: sorting
	// by time alone buried a hundred-hit session below the search that found it.
	score := make(map[string]int, len(matched))
	for _, s := range matched {
		score[s.SourceRef.Source] = idx.Score(s, q)
	}
	sort.SliceStable(matched, func(i, j int) bool {
		si, sj := score[matched[i].SourceRef.Source], score[matched[j].SourceRef.Source]
		if si != sj {
			return si > sj
		}
		return matched[i].UpdatedAt.After(matched[j].UpdatedAt)
	})

	result := searchResult{Query: query, Scanned: len(all), Matched: len(matched), Sessions: []searchSession{}}
	// More sessions are opened than are listed, and the ones that were opened are
	// ranked again on what a reader actually gets: the number of turns the query
	// is in. The index score that got them this far counts occurrences in one run
	// of text, which is enough to pick the candidates but puts a session that says
	// a word five times in one message above one that returns to it all afternoon.
	// The margin also absorbs the sessions dropped below, so a search still answers
	// with as many sessions as it was asked for.
	for _, s := range matched[:openCount(len(matched), *limit)] {
		row := sessionHits(ctx, a, db, s, q, *perSession, *width)
		if bySummary[s.Key()] && len(row.Hits) == 0 {
			// Nothing but the summaries put this session here, and opening it
			// produced nothing to point at. The text was matched as one run, so an
			// anchored pattern can match it and no single summary; and a summary of
			// a turn the session has since rewound past is withheld rather than
			// shown against whatever turn now carries that number.
			//
			// The test is on the hits and not on the kind of match: a query of
			// several words is answered by the index only when every one of them is
			// somewhere in the session, while a single event needs just one — so a
			// session found through its summaries can still turn out to say part of
			// it outright, and that is a content match worth showing.
			result.Matched--
			continue
		}
		if row.Match == "metadata" && q.IsRegexp() && !search.MatchesMetadata(s, q) {
			// The index holds a session's text as one run, so an anchored pattern
			// can match it there and nowhere in a single message. Such a session
			// has nothing to show and is dropped rather than listed empty.
			result.Matched--
			continue
		}
		if row.meta && !*includeMeta {
			// Every use of the search leaves behind a session whose only mention of
			// the query is the search itself, and each one answers the same query
			// forever after. Left in, they crowd out the work they were looking for.
			result.MetaSuppressed++
			continue
		}
		row.rank = row.TotalHits + search.MetaScore(s, q)
		result.Sessions = append(result.Sessions, row)
	}
	sort.SliceStable(result.Sessions, func(i, j int) bool {
		if result.Sessions[i].rank != result.Sessions[j].rank {
			return result.Sessions[i].rank > result.Sessions[j].rank
		}
		return result.Sessions[i].UpdatedAt.After(result.Sessions[j].UpdatedAt)
	})
	if *limit > 0 && len(result.Sessions) > *limit {
		result.Sessions = result.Sessions[:*limit]
	}
	// Sessions go missing for two different reasons and only one of them is
	// answered by asking for more: the limit cut the listing off, or a session was
	// left out for having only searched (which meta_suppressed accounts for).
	// Saying "raise --limit" to a listing that never reached the limit sends the
	// reader after sessions that raising it will not produce.
	if *limit > 0 && len(result.Sessions) == *limit && result.Matched > *limit {
		result.Note = fmt.Sprintf("%d sessions matched; listing the %d most relevant (raise --limit for more)", result.Matched, len(result.Sessions))
	}
	// Nothing to show, rather than nothing matched: a session that was opened and
	// turned out to have nothing to point at leaves the same empty answer, and the
	// same hint is the one worth giving.
	if len(result.Sessions) == 0 && filter != (app.SessionFilter{}) {
		if n, where := elsewhere(ctx, a, db, all, sessions, q, filter.CWD); n > 0 {
			result.Elsewhere = n
			result.Note = fmt.Sprintf("nothing here, but %d sessions match without the filters", n)
			if where != "" {
				result.Note += " (" + where + ")"
			}
		}
	}
	if out == "json" {
		printJSON(w, result)
		return
	}
	printSearchTable(w, result, snippet)
}

// tableSnippet is how many columns a hit is given on the line it shares with
// its turn number and kind. It bounds what --context left, which is counted in
// runes: 44 runes of Japanese is 88 columns, and the two limits together are
// what keeps a hit on one line in either language.
const tableSnippet = 100

// printSearchTable prints the result as lines meant to be read: one row per
// session with the handle `show` takes, then the title, then the hits with the
// turn number `show --turns` takes. Everything the JSON says about what was left
// out is said here too, at the end — a listing that quietly stops at ten is read
// as "there were ten". snippet is how many columns a hit may take, or 0 to print
// it whole.
func printSearchTable(w io.Writer, r searchResult, snippet int) {
	for _, s := range r.Sessions {
		date := ""
		if !s.UpdatedAt.IsZero() {
			date = s.UpdatedAt.Local().Format("2006-01-02")
		}
		// A session that matched on its title or path alone says so where the hit
		// count would be, rather than showing "0 hits" and no hits.
		found := fmt.Sprintf("%d hits", s.TotalHits)
		if s.Match != "content" {
			found = s.Match
		}
		gone := ""
		if s.LogDeleted {
			// Said on the row rather than left to be discovered: the id is about to be
			// handed to a command that cannot resume it.
			gone = "  (log deleted)"
		}
		fmt.Fprintf(w, "%-8s %-8s %-10s %-9s %s%s\n", idPrefix(s.SessionID), s.PluginID, date, found, s.CWD, gone)
		if s.Title != "" {
			fmt.Fprintf(w, "  %s\n", short(oneLine(s.Title), 72))
		}
		for _, h := range s.Hits {
			text := h.Snippet
			if snippet > 0 {
				text = short(text, snippet)
			}
			turn, kind := fmt.Sprintf("t%d", h.Turn), string(h.Kind)
			if s.Match == "summary" {
				// The kind column says the line is generated rather than something
				// that was said, since nothing else on the row would.
				kind = "summary"
				if h.Turn == 0 {
					turn = "whole" // the session's own summary, not a turn
				}
			}
			fmt.Fprintf(w, "  %-5s %-10s %s\n", turn, kind, text)
		}
		if s.Error != "" {
			fmt.Fprintf(w, "  %s\n", s.Error)
		}
	}
	if len(r.Sessions) == 0 {
		fmt.Fprintf(w, "nothing matched %q in %d sessions\n", r.Query, r.Scanned)
	}
	if r.MetaSuppressed > 0 {
		fmt.Fprintf(w, "%d left out for only having searched for it (--include-meta keeps them)\n", r.MetaSuppressed)
	}
	if r.Note != "" {
		fmt.Fprintln(w, r.Note)
	}
}

// openCount is how many of the candidate sessions to open for a limit of n.
// Opening every match would make a broad query as slow as a search that reads
// everything, and opening exactly as many as are listed would let the coarse
// index order decide the answer. The margin is what the second ranking has to
// work with — and what replaces the sessions that turn out to have nothing to
// show.
func openCount(matched, limit int) int {
	if limit <= 0 {
		return matched
	}
	return min(matched, max(3*limit, limit+10))
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
	if dir == "" {
		// Nothing was narrowed by directory, so naming directories would point away
		// from the filter that is actually in the way.
		return count, ""
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
func sessionHits(ctx context.Context, a *app.App, db *cache.DB, s domain.Session, q search.Query, max, width int) searchSession {
	row := searchSession{
		SessionID: s.SessionID, PluginID: s.PluginID, AgentType: s.AgentType,
		Title: s.Title, CWD: s.CWD, StartedAt: s.StartedAt, UpdatedAt: s.UpdatedAt,
		Source: s.SourceRef.Source, Match: "metadata", Hits: []search.Hit{},
		LogDeleted: s.LogDeleted,
	}
	conv, err := a.Conversation(ctx, s)
	if err != nil || conv == nil {
		// The conversation is gone, so the generated summaries are all that is
		// left to point at — and a session in this state is the one they were
		// worth keeping for. Turn numbers are positions along a branch and there
		// is no branch here to check them against, so only the session summary is
		// asked for: Summaries withholds every turn the caller cannot vouch for.
		if hits, total := summaryHits(ctx, db, s, nil, q, max, width); total > 0 {
			row.Match, row.Hits, row.TotalHits = "summary", hits, total
			return row
		}
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
	turns := transcript.Turns(*conv, conv.ActivePath())
	hits, sum := search.Hits(*conv, turns, q, search.HitOptions{Max: max, Context: width})
	if sum.Total > 0 {
		row.Match = "content"
		row.Hits = hits
		row.TotalHits = sum.Total
	} else if hits, total := summaryHits(ctx, db, s, summary.NodesByTurn(turns), q, max, width); total > 0 {
		// Only when the session itself says nothing. A summary is a paraphrase,
		// and a session that says the thing outright is the better answer — this
		// is for the one that does not, or can no longer be read.
		row.Match, row.Hits, row.TotalHits = "summary", hits, total
	}
	// A search for agentcarto itself is asking for these sessions, so it is not
	// told there are none.
	row.meta = sum.OnlyRanAgentcarto() && !search.IsSelfQuery(q)
	return row
}

// summaryHits looks for the query in a session's generated summaries. nodeByTurn
// names the turns the caller can vouch for; passing nil asks for the session
// summary alone.
func summaryHits(ctx context.Context, db *cache.DB, s domain.Session, nodeByTurn map[int]string, q search.Query, max, width int) ([]search.Hit, int) {
	if db == nil {
		return nil, 0
	}
	stored := db.Summaries(ctx, s, nodeByTurn)
	if len(stored) == 0 {
		return nil, 0
	}
	texts := make(map[int]string, len(stored))
	for n, sum := range stored {
		texts[n] = sum.Text
	}
	return search.SummaryHits(texts, q, search.HitOptions{Max: max, Context: width})
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
