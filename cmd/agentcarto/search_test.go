package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/catalog"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/search"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.Local)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"7d", now.AddDate(0, 0, -7)},
		{"2w", now.AddDate(0, 0, -14)},
		{"12h", now.Add(-12 * time.Hour)},
		{"90m", now.Add(-90 * time.Minute)},
		{"2026-08-01", time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)},
	}
	for _, c := range cases {
		got, err := parseSince(c.in, now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("parseSince(%q)=%v %v, want %v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"yesterday", "7dd", "", "d", "2026-13-01"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("parseSince(%q) was accepted", bad)
		}
	}
}

// Flags after the query must still be seen: an agent writes the query first as
// often as not, and the flag package stops at the first positional argument.
func TestParseFlagsAcceptsFlagsAfterTheQuery(t *testing.T) {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	dir := fs.String("cwd", "", "")
	limit := fs.Int("limit", 20, "")
	got := parseFlags(fs, []string{"handoff", "--cwd", ".", "order", "--limit", "3"})
	if strings.Join(got, " ") != "handoff order" {
		t.Fatalf("positional=%v", got)
	}
	if *dir != "." || *limit != 3 {
		t.Fatalf("cwd=%q limit=%d: flags after the query were lost", *dir, *limit)
	}
}

// convStub serves one conversation for every session.
type convStub struct{ conv domain.Conversation }

func (s convStub) LoadConversation(context.Context, domain.SessionRef) (*domain.Conversation, error) {
	return &s.conv, nil
}

func hitApp(c domain.Conversation) *app.App {
	a := app.Build(config.Config{}, nil)
	a.Catalog = catalog.Catalog{Plugins: []plugin.Instance{{
		ID:         "claude",
		Descriptor: plugin.Descriptor{Capabilities: domain.Capabilities{Conversation: true}},
		Impl:       convStub{c},
	}}}
	return a
}

func TestSessionHitsReportsWhereAndHowMany(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "handoff の順序", Prompt: "handoff の順序"}}},
		{ID: "a1", Parent: "u1", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "handoff はプラグインを先に落とす"}}},
		{ID: "u2", Parent: "a1", Events: []domain.Event{{Kind: domain.EventUser, Text: "handoff をもう一度", Prompt: "handoff をもう一度"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s1", Title: "t",
		SourceRef: domain.SessionRef{Source: "/logs/s1.jsonl"}}

	row := sessionHits(context.Background(), hitApp(c), nil, s, search.NewQuery("handoff"), 2, 20)
	if row.Match != "content" {
		t.Fatalf("match=%q want content", row.Match)
	}
	// Two hits shown of the three the query was found in: the total is the whole
	// count, not the remainder, so nothing has to be added back to read it.
	if len(row.Hits) != 2 || row.TotalHits != 3 {
		t.Fatalf("hits=%d total=%d want 2 and 3", len(row.Hits), row.TotalHits)
	}
	// The turn numbers are the ones show takes.
	if row.Hits[0].Turn != 1 || row.Hits[1].Turn != 2 {
		t.Fatalf("turns=%d,%d", row.Hits[0].Turn, row.Hits[1].Turn)
	}

	// A session matched by its title alone says so instead of returning an empty
	// hit list that reads like a bug.
	row = sessionHits(context.Background(), hitApp(c), nil, s, search.NewQuery("t"), 2, 20)
	if row.Match != "metadata" || len(row.Hits) != 0 {
		t.Fatalf("metadata match=%q hits=%d", row.Match, len(row.Hits))
	}
}

// The JSON is the interface: an agent reads these names, and hits are an empty
// array rather than null so a caller can iterate without a nil check.
func TestSearchResultJSONShape(t *testing.T) {
	row := searchSession{SessionID: "s1", PluginID: "claude", AgentType: "claude",
		Source: "/logs/s1.jsonl", Match: "metadata", Hits: []search.Hit{}}
	b, err := json.Marshal(searchResult{Query: "q", Scanned: 3, Matched: 1, Sessions: []searchSession{row}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"query":"q"`, `"scanned":3`, `"matched":1`,
		`"session_id":"s1"`, `"plugin_id":"claude"`, `"source":"/logs/s1.jsonl"`, `"match":"metadata"`, `"hits":[]`, `"total_hits":0`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("result JSON is missing %s: %s", want, b)
		}
	}
	if strings.Contains(string(b), `"note"`) {
		t.Errorf("empty fields should be left out: %s", b)
	}
	// The value the results are ordered by only means something next to the other
	// results of the same search, so it stays out of the interface.
	if strings.Contains(string(b), `"rank"`) {
		t.Errorf("the ordering value should not be part of the JSON: %s", b)
	}
	// A session whose plugin records no times must not report the zero date:
	// encoding/json does not consider a struct empty, so these fields need
	// omitzero rather than omitempty.
	if strings.Contains(string(b), "0001-01-01") {
		t.Errorf("an unset time was printed as a date: %s", b)
	}
	hit := search.Hit{Turn: 1, Kind: "assistant", Snippet: "x"}
	if hb, _ := json.Marshal(hit); strings.Contains(string(hb), "0001-01-01") {
		t.Errorf("a hit without a timestamp printed the zero date: %s", hb)
	}
}

// An age cannot be negative: "-7d" would otherwise select the future and report
// nothing, with no hint as to why.
func TestParseSinceRejectsNegativeAges(t *testing.T) {
	now := time.Now()
	for _, bad := range []string{"-7d", "-2w"} {
		if got, err := parseSince(bad, now); err == nil {
			t.Errorf("parseSince(%q)=%v was accepted", bad, got)
		}
	}
}

// When a filtered search finds nothing, the directories that hold matches are
// named — and the one containing the directory that was searched comes first,
// however few matches it holds. "The work was done one level up" is the answer
// being looked for.
func TestTopDirsPutsTheEnclosingDirectoryFirst(t *testing.T) {
	byDir := map[string]int{
		absPath("/home/u/repo"):       1, // encloses the searched directory
		absPath("/home/u/other"):      5,
		absPath("/home/u/repo/sub"):   3, // a sibling branch, not on the path
		absPath("/home/u/repo/sub/x"): 2,
	}
	got := topDirs(byDir, absPath("/home/u/repo/app"), 3)
	if !strings.HasPrefix(got, absPath("/home/u/repo")+": 1") {
		t.Fatalf("topDirs=%q, want the enclosing directory first", got)
	}
	if !strings.Contains(got, "and 1 more directories") {
		t.Errorf("topDirs=%q, want the remainder counted", got)
	}
	// Without a searched directory it is a plain popularity order.
	if got := topDirs(byDir, "", 2); !strings.HasPrefix(got, absPath("/home/u/other")+": 5") {
		t.Fatalf("topDirs=%q", got)
	}
	// A session with no recorded directory is named rather than shown blank.
	if got := topDirs(map[string]int{"": 2}, "", 3); !strings.Contains(got, "(no recorded directory)") {
		t.Fatalf("topDirs=%q", got)
	}
}

// list --json and search print the same session fields under the same names, so
// one reader can take either.
func TestListedSessionJSONShape(t *testing.T) {
	b, err := json.Marshal(listedSession{SessionID: "s1", PluginID: "claude", AgentType: "claude",
		Source: "/logs/s1.jsonl", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"session_id":"s1"`, `"plugin_id":"claude"`, `"source":"/logs/s1.jsonl"`, `"status":"running"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("list JSON is missing %s: %s", want, b)
		}
	}
	if strings.Contains(string(b), "0001-01-01") || strings.Contains(string(b), `"title"`) {
		t.Errorf("unset fields should be left out: %s", b)
	}
}

// An age so large that the clock wraps round means "as far back as there is",
// not "nothing at all": time.Time overflows a few hundred million days out, and
// the wrapped value would be in the future and select no session.
func TestParseSinceDoesNotWrapIntoTheFuture(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	for _, in := range []string{"107000000000000d", "99999999999w", "2562047h", "9223372036854775807h"} {
		got, err := parseSince(in, now)
		if err != nil {
			continue // rejected outright is fine too
		}
		if got.After(now) {
			t.Errorf("--since %q resolved to %v, after %v", in, got, now)
		}
	}
	// A sane age is untouched.
	if got, err := parseSince("7d", now); err != nil || !got.Equal(now.AddDate(0, 0, -7)) {
		t.Errorf("parseSince(7d) = %v %v", got, err)
	}
}

// A session that talked around a subject without using the word is one a search
// misses today. Its generated summary says what came of it in other words, and
// that is what makes the session findable.
func TestSearchFindsASessionByItsSummary(t *testing.T) {
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: now, Events: talk("これ直しといて", "直した")},
		{ID: "u2", Parent: "u1", Timestamp: now, Events: talk("もう一回", "やった")},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "quiet", Title: "無題",
		CWD: absPath("/repo/app"), UpdatedAt: now, SourceRef: domain.SessionRef{Source: "/logs/quiet.jsonl"}}
	a, cfg := fixtureApp([]domain.Session{s}, map[string]domain.Conversation{"/logs/quiet.jsonl": c})

	turns := transcript.Turns(c, c.ActivePath())
	nodes := summary.NodesByTurn(turns)
	scanned := app.FilterSessions(scanSessions(ctx, a, cfg, d), app.SessionFilter{})[0]
	if err := d.PutSummaries(ctx, scanned, []cache.Summary{
		{Turn: 0, Text: "PNG の減色まわりを詰めた", Model: "m"},
		{Turn: 2, NodeID: nodes[2], Text: "パレット生成を median cut に差し替えた", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}

	got := runSearchWithCache(t, a, cfg, d, "median cut")
	if got.Matched != 1 || len(got.Sessions) != 1 {
		t.Fatalf("the summary did not make the session findable: %+v", got)
	}
	row := got.Sessions[0]
	if row.Match != "summary" {
		t.Errorf("match=%q want summary — the conversation never says it", row.Match)
	}
	if len(row.Hits) != 1 || row.Hits[0].Turn != 2 {
		t.Fatalf("hits=%+v want one, in turn 2", row.Hits)
	}
	if !strings.Contains(row.Hits[0].Snippet, "median cut") {
		t.Errorf("snippet=%q", row.Hits[0].Snippet)
	}
	// The session summary is turn 0, which is what a document keys it by.
	if got := runSearchWithCache(t, a, cfg, d, "減色"); got.Sessions[0].Hits[0].Turn != 0 {
		t.Errorf("the session summary should come back as turn 0: %+v", got.Sessions[0].Hits)
	}
	// What the session itself says still wins: a summary is a paraphrase, and a
	// session that says the thing outright is the better answer.
	if got := runSearchWithCache(t, a, cfg, d, "もう一回"); got.Sessions[0].Match != "content" {
		t.Errorf("match=%q want content", got.Sessions[0].Match)
	}
}

// The session a summary is worth most to is the one that can no longer be read
// at all: its log was rotated away and the cached conversation evicted. A
// kilobyte of summary is what outlives both. Turn numbers are positions along a
// branch, and there is no branch left to check them against, so only the session
// summary is offered.
func TestSearchFindsASessionWhoseConversationIsGone(t *testing.T) {
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "gone", Title: "無題",
		CWD: absPath("/repo/app"), UpdatedAt: now, LogDeleted: true,
		SourceRef: domain.SessionRef{Source: "/logs/gone.jsonl"}}
	// No conversation for this source: reading it fails the way a deleted log does.
	a, cfg := fixtureApp([]domain.Session{s}, nil)
	scanned := app.FilterSessions(scanSessions(ctx, a, cfg, d), app.SessionFilter{IncludeEmptyForks: true})[0]
	if err := d.PutSummaries(ctx, scanned, []cache.Summary{
		{Turn: 0, Text: "PNG の減色まわりを詰めた", Model: "m"},
		{Turn: 1, NodeID: "n1", Text: "パレット生成を median cut に差し替えた", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}

	got := runSearchWithCache(t, a, cfg, d, "減色")
	if len(got.Sessions) != 1 || got.Sessions[0].Match != "summary" {
		t.Fatalf("a session with nothing but summaries was not found: %+v", got)
	}
	if len(got.Sessions[0].Hits) != 1 || got.Sessions[0].Hits[0].Turn != 0 {
		t.Errorf("hits=%+v want the session summary alone", got.Sessions[0].Hits)
	}
	// The turn summary is not offered: nothing here can say the stored turn 1 is
	// still the turn 1 a reader would open.
	if got := runSearchWithCache(t, a, cfg, d, "median cut"); len(got.Sessions) != 0 {
		t.Errorf("a turn summary was offered with no branch to check it against: %+v", got.Sessions)
	}
}

// A session matches on its summaries, but the summary that held the query
// belongs to a turn that is no longer there — the session was rewound, and turn
// 1 is a different turn now. SummaryTexts matches the text without knowing that;
// Summaries refuses to hand a summary to a turn it was not made from. The
// session then has nothing to point at and is dropped rather than listed empty.
func TestSearchDropsASummaryMatchWithNothingToShow(t *testing.T) {
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: now, Events: talk("これ直しといて", "直した")},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "rewound", Title: "無題",
		CWD: absPath("/repo/app"), UpdatedAt: now, SourceRef: domain.SessionRef{Source: "/logs/rewound.jsonl"}}
	a, cfg := fixtureApp([]domain.Session{s}, map[string]domain.Conversation{"/logs/rewound.jsonl": c})
	scanned := app.FilterSessions(scanSessions(ctx, a, cfg, d), app.SessionFilter{})[0]
	if err := d.PutSummaries(ctx, scanned, []cache.Summary{
		// Made from a node the conversation no longer holds.
		{Turn: 1, NodeID: "gone", Text: "median cut に差し替えた", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	got := runSearchWithCache(t, a, cfg, d, "median cut")
	if got.Matched != 0 || len(got.Sessions) != 0 {
		t.Errorf("a summary was shown against a turn it was not made from: matched=%d %+v", got.Matched, got.Sessions)
	}
}

func runSearchWithCache(t *testing.T, a *app.App, cfg config.Config, d *cache.DB, args ...string) searchResult {
	t.Helper()
	var b bytes.Buffer
	searchCmd(context.Background(), a, cfg, d, append([]string{"--format", "json"}, args...), &b)
	var got searchResult
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatalf("output is not the documented JSON: %v\n%s", err, b.String())
	}
	return got
}

// A summary row is generated text, and the table has to say so: nothing else on
// the line distinguishes it from something that was said. The session's own
// summary has no turn number to print, so the turn column names it instead of
// showing a turn 0 that `show --turns 0` would not take.
func TestSearchTableMarksSummaryHits(t *testing.T) {
	r := searchResult{
		Query: "png", Scanned: 1, Matched: 1,
		Sessions: []searchSession{{
			SessionID: "aaaa1111-2222", PluginID: "claude", Match: "summary", TotalHits: 2,
			Hits: []search.Hit{
				{Turn: 0, Snippet: "PNG の減色まわりを詰めた"},
				{Turn: 2, Snippet: "パレット生成を差し替えた"},
			},
		}},
	}
	var b bytes.Buffer
	printSearchTable(&b, r, 0)
	out := b.String()
	// Where the hit count goes, the row says what kind of match it is.
	if !strings.Contains(out, "summary") {
		t.Errorf("the row does not say the match is generated:\n%s", out)
	}
	if !strings.Contains(out, "whole ") {
		t.Errorf("the session summary is not named in the turn column:\n%s", out)
	}
	if strings.Contains(out, "t0 ") {
		t.Errorf("turn 0 is printed as a turn `show --turns` would take:\n%s", out)
	}
	if !strings.Contains(out, "t2 ") {
		t.Errorf("a turn summary should keep the number show --turns takes:\n%s", out)
	}
}

// A query of several words is answered by the index only when every one of them
// is somewhere in the session; a single event needs just one. So a session found
// through its summaries can still turn out to say part of the query outright —
// and those are real hits, not something to throw away for having arrived by the
// summary door.
func TestASessionFoundBySummaryKeepsItsContentHits(t *testing.T) {
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	// The conversation says "png" and never "減色"; the summary says both.
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: now, Events: talk("png を小さくしたい", "やってみる")},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "partial", Title: "無題",
		CWD: absPath("/repo/app"), UpdatedAt: now, SourceRef: domain.SessionRef{Source: "/logs/partial.jsonl"}}
	a, cfg := fixtureApp([]domain.Session{s}, map[string]domain.Conversation{"/logs/partial.jsonl": c})
	scanned := app.FilterSessions(scanSessions(ctx, a, cfg, d), app.SessionFilter{})[0]
	if err := d.PutSummaries(ctx, scanned, []cache.Summary{
		{Turn: 0, Text: "png の減色まわりを詰めた", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}

	got := runSearchWithCache(t, a, cfg, d, "png 減色")
	if got.Matched != 1 || len(got.Sessions) != 1 {
		t.Fatalf("a session with real hits was dropped: matched=%d %+v", got.Matched, got.Sessions)
	}
	if got.Sessions[0].Match != "content" || len(got.Sessions[0].Hits) == 0 {
		t.Errorf("the hits the session really holds were thrown away: %+v", got.Sessions[0])
	}
}

// A pattern that matches the empty string must not pull in every session that
// has no summaries at all. Their summary text is "" because there is none, not
// because a session said nothing.
//
// The count is what shows it. Only the first openCount candidates are opened,
// and the drop below only fires for those — so sessions pulled in wrongly and
// never opened are left in "matched" as sessions that answered the query.
func TestAnEmptyMatchingPatternFindsNoSummaries(t *testing.T) {
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// More sessions than one run opens: with --limit 1 that is eleven.
	a, cfg := settledAppN(15)

	// \A\z matches only wholly empty text, so it answers nothing in any of them.
	got := runSearchWithCache(t, a, cfg, d, "--limit", "1", "--regex", `\A\z`)
	if got.Matched != 0 || len(got.Sessions) != 0 {
		t.Errorf("sessions with no summaries were matched by an empty pattern: matched=%d listed=%d", got.Matched, len(got.Sessions))
	}
}
