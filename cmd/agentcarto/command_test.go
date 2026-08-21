package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/catalog"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
)

// scanStub is a plugin that reports a fixed set of sessions and serves one
// conversation per session, which is enough to drive the commands end to end
// without a plugin subprocess.
type scanStub struct {
	sessions []domain.Session
	convs    map[string]domain.Conversation
}

func (s scanStub) Scan(context.Context, plugin.ScanInput) (plugin.ScanOutput, error) {
	return plugin.ScanOutput{Sessions: s.sessions}, nil
}

func (s scanStub) LoadConversation(_ context.Context, ref domain.SessionRef) (*domain.Conversation, error) {
	c, ok := s.convs[ref.Source]
	if !ok {
		c = domain.NewConversation(nil)
	}
	return &c, nil
}

func talk(prompt, reply string) []domain.Event {
	return []domain.Event{
		{Kind: domain.EventUser, Text: prompt, Prompt: prompt},
		{Kind: domain.EventAssistant, Text: reply},
	}
}

// commandApp builds an App over two sessions in different directories: one that
// talks about handoff, one that does not.
func commandApp() (*app.App, config.Config) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	sessions := []domain.Session{
		{PluginID: "claude", AgentType: "claude", SessionID: "aaaa1111-2222", CWD: "/repo/app", Title: "handoff の順序",
			UpdatedAt: now, StartedAt: now.Add(-time.Hour), SourceRef: domain.SessionRef{Source: "/logs/a.jsonl"}},
		{PluginID: "claude", AgentType: "claude", SessionID: "bbbb3333-4444", CWD: "/elsewhere", Title: "別の話",
			UpdatedAt: now.Add(-48 * time.Hour), SourceRef: domain.SessionRef{Source: "/logs/b.jsonl"}},
	}
	convs := map[string]domain.Conversation{
		"/logs/a.jsonl": domain.NewConversation([]domain.ConvNode{
			{ID: "u1", Timestamp: now, Events: talk("handoff の順序は？", "プラグインを先に落とす")},
			{ID: "u2", Parent: "u1", Timestamp: now, Events: talk("次の話", "了解")},
		}),
		"/logs/b.jsonl": domain.NewConversation([]domain.ConvNode{
			{ID: "u1", Timestamp: now, Events: talk("handoff について", "こちらでも触れた")},
		}),
	}
	a := app.Build(config.Config{Index: config.Index{MaxCharsPerSession: 1 << 16}}, nil)
	a.Catalog = catalog.Catalog{Plugins: []plugin.Instance{{
		ID:         "claude",
		Descriptor: plugin.Descriptor{Type: "claude", Capabilities: domain.Capabilities{Scan: true, Conversation: true}},
		Impl:       scanStub{sessions: sessions, convs: convs},
	}}}
	return a, a.Config
}

func runSearch(t *testing.T, args ...string) searchResult {
	t.Helper()
	a, cfg := commandApp()
	var b bytes.Buffer
	searchCmd(context.Background(), a, cfg, nil, args, &b)
	var got searchResult
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatalf("output is not the documented JSON: %v\n%s", err, b.String())
	}
	return got
}

func TestSearchCommandEndToEnd(t *testing.T) {
	got := runSearch(t, "handoff")
	if got.Query != "handoff" || got.Scanned != 2 || got.Matched != 2 || len(got.Sessions) != 2 {
		t.Fatalf("result=%+v", got)
	}
	// Newest first, and each session reports where the query was found.
	if got.Sessions[0].SessionID != "aaaa1111-2222" {
		t.Errorf("sessions are not newest first: %+v", got.Sessions)
	}
	if got.Sessions[0].Match != "content" || len(got.Sessions[0].Hits) == 0 {
		t.Errorf("first session: %+v", got.Sessions[0])
	}
	if got.Sessions[0].Hits[0].Turn != 1 {
		t.Errorf("hit turn=%d want 1", got.Sessions[0].Hits[0].Turn)
	}
}

func TestSearchCommandFilters(t *testing.T) {
	if got := runSearch(t, "--cwd", "/repo/app", "handoff"); got.Matched != 1 || got.Sessions[0].CWD != "/repo/app" {
		t.Errorf("--cwd kept %+v", got.Sessions)
	}
	if got := runSearch(t, "--since", "24h", "handoff"); got.Matched != 1 {
		t.Errorf("--since kept %d sessions", got.Matched)
	}
	if got := runSearch(t, "--agent", "claude", "handoff"); got.Matched != 2 {
		t.Errorf("--agent claude kept %d", got.Matched)
	}
	// A limit says what it left out rather than trailing off.
	got := runSearch(t, "--limit", "1", "handoff")
	if len(got.Sessions) != 1 || got.Matched != 2 || !strings.Contains(got.Note, "2 sessions matched") {
		t.Errorf("limit=%+v note=%q", got.Sessions, got.Note)
	}
}

// A filtered search that finds nothing says where the matches are instead, with
// the directory that contains the searched one named first.
func TestSearchCommandReportsMatchesOutsideTheFilter(t *testing.T) {
	got := runSearch(t, "--cwd", "/repo/app/sub", "handoff")
	if got.Matched != 0 || got.Elsewhere != 2 {
		t.Fatalf("matched=%d elsewhere=%d", got.Matched, got.Elsewhere)
	}
	if !strings.HasPrefix(strings.SplitN(got.Note, "(", 2)[1], "/repo/app: 1") {
		t.Errorf("note should name the enclosing directory first: %q", got.Note)
	}
}

func TestSearchCommandMetadataOnlyMatch(t *testing.T) {
	got := runSearch(t, "別の話")
	if got.Matched != 1 {
		t.Fatalf("matched=%d", got.Matched)
	}
	if got.Sessions[0].Match != "metadata" || len(got.Sessions[0].Hits) != 0 {
		t.Errorf("a title-only match should say so: %+v", got.Sessions[0])
	}
}

func runShow(t *testing.T, args ...string) string {
	t.Helper()
	a, cfg := commandApp()
	var b bytes.Buffer
	showCmd(context.Background(), a, cfg, nil, args, &b)
	return b.String()
}

func TestShowCommandOutlineAndTurns(t *testing.T) {
	outline := runShow(t, "aaaa1111")
	for _, want := range []string{"# handoff の順序", "- **Turns**: 2", "- Turn 1", "- Turn 2", " B]"} {
		if !strings.Contains(outline, want) {
			t.Errorf("outline is missing %q:\n%s", want, outline)
		}
	}
	if strings.Contains(outline, "**USER**") {
		t.Errorf("the outline should not carry bodies:\n%s", outline)
	}

	doc := runShow(t, "aaaa1111", "--turns", "2")
	if !strings.Contains(doc, "## Turn 2") || !strings.Contains(doc, "**USER**\n\n次の話") {
		t.Errorf("turn 2:\n%s", doc)
	}
	if !strings.Contains(doc, "- **Turns**: 1 of 2") {
		t.Errorf("an excerpt should say what it is part of:\n%s", doc)
	}
	if strings.Contains(doc, "## Turn 1") {
		t.Errorf("only the asked-for turn should be printed:\n%s", doc)
	}

	if last := runShow(t, "aaaa1111", "--last", "1"); !strings.Contains(last, "## Turn 2") || strings.Contains(last, "## Turn 1") {
		t.Errorf("--last 1:\n%s", last)
	}
	if all := runShow(t, "aaaa1111", "--all"); !strings.Contains(all, "## Turn 1") || !strings.Contains(all, "## Turn 2") {
		t.Errorf("--all:\n%s", all)
	}
}

func runList(t *testing.T, args ...string) string {
	t.Helper()
	a, cfg := commandApp()
	var b bytes.Buffer
	listCmd(context.Background(), a, cfg, nil, args, false, &b)
	return b.String()
}

func TestListCommand(t *testing.T) {
	var out struct {
		Sessions []listedSession `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(runList(t, "--json")), &out); err != nil {
		t.Fatalf("list --json: %v", err)
	}
	if len(out.Sessions) != 2 || out.Sessions[0].SessionID != "aaaa1111-2222" {
		t.Fatalf("sessions=%+v", out.Sessions)
	}
	if err := json.Unmarshal([]byte(runList(t, "--json", "--cwd", "/repo/app", "--limit", "1")), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].CWD != "/repo/app" {
		t.Fatalf("filtered=%+v", out.Sessions)
	}
	// The column form stays the default: a person running "list" gets a table.
	if text := runList(t); !strings.Contains(text, "claude") || strings.Contains(text, "{") {
		t.Errorf("default output should be columns:\n%s", text)
	}
}

// usage has to name every command the dispatcher accepts. Adding one without
// telling anyone is how a CLI grows features nobody can find.
func TestUsageListsEveryCommand(t *testing.T) {
	var b strings.Builder
	usage(&b)
	text := b.String()
	for _, cmd := range []string{"list", "active", "search", "show", "config", "plugins", "doctor", "cache", "help"} {
		if !strings.Contains(text, cmd) {
			t.Errorf("usage does not mention %q:\n%s", cmd, text)
		}
	}
	if !strings.Contains(text, "--config") || !strings.Contains(text, "--no-cache") {
		t.Errorf("usage omits the global flags:\n%s", text)
	}
}

// A nil *cache.DB must not reach BuildIndex as a non-nil interface holding a nil
// pointer: every call on it would panic.
func TestArtifactStoreKeepsNilNil(t *testing.T) {
	if s := artifactStore(nil); s != nil {
		t.Fatalf("artifactStore(nil) = %#v, want a nil interface", s)
	}
}

func TestSelectedTurnsLabel(t *testing.T) {
	one := []transcript.Turn{{Index: 11}}
	if got := selectedTurnsLabel(one); got != "turn 12" {
		t.Errorf("one turn: %q", got)
	}
	many := []transcript.Turn{{Index: 0}, {Index: 4}}
	if got := selectedTurnsLabel(many); got != "turns 1, 5" {
		t.Errorf("several turns: %q", got)
	}
}

// --regex is the flag that makes one search do what two searches did: the same
// idea written two ways is one alternation.
func TestSearchCommandRegex(t *testing.T) {
	got := runSearch(t, "--regex", "handoff|別の")
	if got.Matched != 2 {
		t.Fatalf("matched=%d, want both spellings: %+v", got.Matched, got.Sessions)
	}
	// A pattern that only the title answers is still a metadata match.
	if got := runSearch(t, "--regex", "^別の話$"); got.Matched != 1 || got.Sessions[0].Match != "metadata" {
		t.Fatalf("title-only pattern: %+v", got.Sessions)
	}
	// Line anchors work on what was said.
	if got := runSearch(t, "--regex", "^handoff の順序は"); got.Matched != 1 || got.Sessions[0].Match != "content" {
		t.Fatalf("anchored pattern: %+v", got.Sessions)
	}
	if got := runSearch(t, "--regex", "見つからないはず"); got.Matched != 0 || len(got.Sessions) != 0 {
		t.Fatalf("a pattern that matches nothing: %+v", got)
	}
}
