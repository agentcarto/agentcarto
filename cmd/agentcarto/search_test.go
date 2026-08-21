package main

import (
	"context"
	"encoding/json"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/catalog"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/search"
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

func TestSessionHitsReportsWhereAndHowManyMore(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "handoff の順序", Prompt: "handoff の順序"}}},
		{ID: "a1", Parent: "u1", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "handoff はプラグインを先に落とす"}}},
		{ID: "u2", Parent: "a1", Events: []domain.Event{{Kind: domain.EventUser, Text: "handoff をもう一度", Prompt: "handoff をもう一度"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s1", Title: "t",
		SourceRef: domain.SessionRef{Source: "/logs/s1.jsonl"}}

	row := sessionHits(context.Background(), hitApp(c), s, search.NewQuery("handoff"), 2, 20)
	if row.Match != "content" {
		t.Fatalf("match=%q want content", row.Match)
	}
	if len(row.Hits) != 2 || row.MoreHits != 1 {
		t.Fatalf("hits=%d more=%d want 2 and 1", len(row.Hits), row.MoreHits)
	}
	// The turn numbers are the ones show takes.
	if row.Hits[0].Turn != 1 || row.Hits[1].Turn != 2 {
		t.Fatalf("turns=%d,%d", row.Hits[0].Turn, row.Hits[1].Turn)
	}

	// A session matched by its title alone says so instead of returning an empty
	// hit list that reads like a bug.
	row = sessionHits(context.Background(), hitApp(c), s, search.NewQuery("t"), 2, 20)
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
		`"session_id":"s1"`, `"plugin_id":"claude"`, `"source":"/logs/s1.jsonl"`, `"match":"metadata"`, `"hits":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("result JSON is missing %s: %s", want, b)
		}
	}
	if strings.Contains(string(b), `"note"`) || strings.Contains(string(b), `"more_hits"`) {
		t.Errorf("empty fields should be left out: %s", b)
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
