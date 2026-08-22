package search

import (
	"github.com/agentcarto/core/domain"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMatchMetadataUnicode(t *testing.T) {
	// The Japanese title and query are intentional test data: they exercise
	// matching over multibyte (non-ASCII) runes, so they are kept as-is.
	i := New(100)
	s := domain.Session{PluginID: "codex", Title: "日本語の題名", CWD: "/work"}
	if !i.Match(s, NewQuery("日本語")) {
		t.Fatal("expected Unicode match")
	}
	if i.Match(s, NewQuery("claude")) {
		t.Fatal("unexpected match")
	}
}

func TestMatchSessionID(t *testing.T) {
	i := New(100)
	s := domain.Session{PluginID: "codex", Title: "title", SessionID: "abc123-def456"}
	if !i.Match(s, NewQuery("def456")) {
		t.Fatal("expected SessionID match")
	}
}

// A tool call is searchable by its name and one-line argument: the command that
// was run and the file that was read are most of what a past session is looked
// up by. Its expanded body and its output are not indexed.
func TestIndexCoversToolCalls(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{{ID: "n1", Events: []domain.Event{
		{Kind: domain.EventUser, Text: "やって", Prompt: "やって"},
		{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: "$ git push origin main",
			ToolDetail: "git push origin main\n# body only"},
		{Kind: domain.EventToolResult, Text: "output only"},
	}}})
	i := New(1 << 16)
	i.Add(domain.Session{}, c)
	s := domain.Session{}
	for _, want := range []string{"git push", "bash", "やって"} {
		if !i.Match(s, NewQuery(want)) {
			t.Errorf("index does not match %q", want)
		}
	}
	for _, unwanted := range []string{"body only", "output only"} {
		if i.Match(s, NewQuery(unwanted)) {
			t.Errorf("index matched %q, which it should not hold", unwanted)
		}
	}
	// Tool calls are not messages: the count the list shows stays the number of
	// things that were said.
	if n, _ := i.Count(s); n != 1 {
		t.Errorf("message count=%d want 1", n)
	}
}

// The message count is not cut short by the character cap: a long session still
// reports how many messages it holds.
func TestIndexCountsEveryMessagePastTheCap(t *testing.T) {
	long := strings.Repeat("あ", 200)
	var events []domain.Event
	for range 10 {
		events = append(events, domain.Event{Kind: domain.EventAssistant, Text: long})
	}
	c := domain.NewConversation([]domain.ConvNode{{ID: "n1", Events: events}})
	i := New(16) // room for one message at most
	i.Add(domain.Session{}, c)
	if n, _ := i.Count(domain.Session{}); n != 10 {
		t.Errorf("count=%d want 10", n)
	}
}

// A query of several words names several things to find in one session, not a
// phrase. "fork relocate" must find the session that discussed both, in any
// order and any distance apart.
func TestMatchRequiresEveryTerm(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{{ID: "n1", Events: []domain.Event{
		{Kind: domain.EventUser, Text: "fork の話", Prompt: "fork の話"},
		{Kind: domain.EventAssistant, Text: "あとで relocate も見る"},
	}}})
	i := New(1 << 16)
	s := domain.Session{Title: "セッション整理", PluginID: "claude"}
	i.Add(s, c)
	for _, q := range []string{"fork relocate", "relocate fork", "  fork   relocate  ", "fork"} {
		if !i.Match(s, NewQuery(q)) {
			t.Errorf("query %q should match", q)
		}
	}
	// Every term has to be there, and a term may be satisfied by the metadata.
	if i.Match(s, NewQuery("fork rewind")) {
		t.Error(`"fork rewind" matched a session without "rewind"`)
	}
	if !i.Match(s, NewQuery("fork 整理")) {
		t.Error(`a term in the title should count`)
	}
}

// A tool call is indexed by what identifies it, not by the file a heredoc
// writes: an unbounded argument would put whole files into the index.
func TestIndexCutsLongToolArguments(t *testing.T) {
	body := strings.Repeat("x", 5000)
	c := domain.NewConversation([]domain.ConvNode{{ID: "n1", Events: []domain.Event{
		{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: "$ cat > note.md <<EOF " + body + " needle EOF"},
	}}})
	i := New(1 << 16)
	s := domain.Session{}
	i.Add(s, c)
	if !i.Match(s, NewQuery("cat > note.md")) {
		t.Error("the front of the command should stay searchable")
	}
	if i.Match(s, NewQuery("needle")) {
		t.Error("the tail of a long argument reached the index")
	}
	if text, _, _ := i.Lookup(s); len([]rune(text)) > toolTextLimit+1 {
		t.Errorf("indexed %d runes for one tool call, want at most %d", len([]rune(text)), toolTextLimit)
	}
}

// The per-session budget has to bound one enormous message too: the room was
// checked before writing an event, not against its size, so a single multi-MB
// reply was written whole and the cap meant nothing.
func TestIndexBudgetBoundsASingleHugeEvent(t *testing.T) {
	huge := strings.Repeat("あ", 200_000) + " needleatend"
	c := domain.NewConversation([]domain.ConvNode{{ID: "n1", Events: []domain.Event{
		{Kind: domain.EventUser, Text: "はじめ", Prompt: "はじめ"},
		{Kind: domain.EventAssistant, Text: huge},
	}}})
	i := New(4096)
	s := domain.Session{}
	i.Add(s, c)
	text, _, _ := i.Lookup(s)
	if len(text) > 4096+8 { // the budget, plus the newline of the last event
		t.Fatalf("indexed %d bytes for a 4096-byte budget", len(text))
	}
	if !utf8.ValidString(text) {
		t.Error("the text was cut in the middle of a rune")
	}
	if !i.Match(s, NewQuery("はじめ")) {
		t.Error("what fits should still be searchable")
	}
	if i.Match(s, NewQuery("needleatend")) {
		t.Error("what is past the budget must not be indexed")
	}
}

// What is searchable is what a transcript shows. Two events say it: a user
// message the agent was handed rather than told (no normalized prompt), which a
// transcript drops; and an edit, which a transcript shows as a file path in the
// turn's edited-file section rather than as a call.
func TestIndexFollowsWhatATranscriptShows(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{{ID: "n1", Events: []domain.Event{
		{Kind: domain.EventUser, Text: "<system-reminder>ここは読み飛ばして</system-reminder>"},
		{Kind: domain.EventUser, Text: "直して", Prompt: "直して"},
		{Kind: domain.EventToolCall, ToolName: "apply_patch", ToolArg: "apply_patch",
			Changes: []domain.FileChange{{Path: "internal/tui/tui.go", Op: "update", Added: 3}}},
		{Kind: domain.EventFileChange, Changes: []domain.FileChange{{Path: "cmd/agentcarto/show.go", Op: "add"}}},
	}}})
	i := New(1 << 16)
	s := domain.Session{}
	i.Add(s, c)
	for _, want := range []string{"直して", "internal/tui/tui.go", "cmd/agentcarto/show.go"} {
		if !i.Match(s, NewQuery(want)) {
			t.Errorf("index should hold %q", want)
		}
	}
	for _, unwanted := range []string{"system-reminder", "読み飛ばして", "apply_patch"} {
		if i.Match(s, NewQuery(unwanted)) {
			t.Errorf("index holds %q, which no transcript shows", unwanted)
		}
	}
	// An injected message is still a message: the count the session list shows
	// does not change.
	if n, _ := i.Count(s); n != 2 {
		t.Errorf("message count=%d want 2", n)
	}
}

// A pattern is what a two-language corpus needs most: the same idea is written
// "cache" in one session and "キャッシュ" in the next, and only alternation
// finds both in one pass.
func TestRegexpQueryMatchesEitherSpelling(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{{ID: "n1", Events: []domain.Event{
		{Kind: domain.EventUser, Text: "キャッシュを消したい", Prompt: "キャッシュを消したい"},
	}}})
	i := New(1 << 16)
	s := domain.Session{Title: "t", PluginID: "claude"}
	i.Add(s, c)
	q, err := NewRegexpQuery("cache|キャッシュ")
	if err != nil {
		t.Fatal(err)
	}
	if !i.Match(s, q) {
		t.Error("the alternation should find the Japanese spelling")
	}
	// Capitals in the pattern still match: the indexed text is folded, so the
	// pattern is compiled case-insensitive.
	up, _ := NewRegexpQuery("CACHE|キャッシュ")
	if !i.Match(s, up) {
		t.Error("a pattern with capitals should still match folded text")
	}
	// ^ and $ are line anchors, in the index and in a message alike.
	anchored, _ := NewRegexpQuery("^キャッシュ")
	if !i.Match(s, anchored) {
		t.Error("a line anchor should match the start of the message")
	}
	if elsewhere, _ := NewRegexpQuery("^消したい"); i.Match(s, elsewhere) {
		t.Error("a line anchor should not match mid-line")
	}
	if _, err := NewRegexpQuery("foo("); err == nil {
		t.Error("an unparsable pattern was accepted")
	}
}

func TestMatchesMetadata(t *testing.T) {
	s := domain.Session{Title: "セッション整理", CWD: "/repo/app", PluginID: "claude", SessionID: "8f3a"}
	if !MatchesMetadata(s, NewQuery("整理 claude")) {
		t.Error("the title and the agent should both count as metadata")
	}
	if MatchesMetadata(s, NewQuery("会話本文")) {
		t.Error("what was said is not metadata")
	}
	q, _ := NewRegexpQuery("/repo/(app|lib)")
	if !MatchesMetadata(s, q) {
		t.Error("a pattern should match the working directory")
	}
}

// scored indexes one session's worth of text and returns what the query is
// worth against it.
func scored(t *testing.T, s domain.Session, text string, q Query) int {
	t.Helper()
	c := domain.NewConversation([]domain.ConvNode{{ID: "n1", Events: []domain.Event{
		{Kind: domain.EventAssistant, Text: text},
	}}})
	i := New(1 << 16)
	i.Add(s, c)
	return i.Score(s, q)
}

// The session that keeps coming back to the subject outranks the one that
// mentions it once, which is the whole point of scoring.
func TestScoreCountsOccurrences(t *testing.T) {
	one := domain.Session{SourceRef: domain.SessionRef{Source: "/a"}}
	many := domain.Session{SourceRef: domain.SessionRef{Source: "/b"}}
	q := NewQuery("handoff")
	lo := scored(t, one, "handoff was mentioned", q)
	hi := scored(t, many, strings.Repeat("handoff ", 5), q)
	if lo != 1 || hi != 5 {
		t.Fatalf("scores are %d and %d, want 1 and 5", lo, hi)
	}
}

// Each word of a query contributes, so a session holding both of them beats one
// that repeats a single word.
func TestScoreAddsUpTerms(t *testing.T) {
	s := domain.Session{SourceRef: domain.SessionRef{Source: "/a"}}
	if n := scored(t, s, "fork fork relocate", NewQuery("fork relocate")); n != 3 {
		t.Errorf("score=%d want 3", n)
	}
	// The query is a pattern: every place it matches counts once.
	q, _ := NewRegexpQuery("cache|キャッシュ")
	if n := scored(t, s, "cache キャッシュ cache", q); n != 3 {
		t.Errorf("regexp score=%d want 3", n)
	}
}

// A title or working directory that names the subject is worth more than a
// passing mention, and it lifts a session whose text was never indexed at all.
func TestScoreRewardsMetadata(t *testing.T) {
	named := domain.Session{SourceRef: domain.SessionRef{Source: "/a"}, Title: "handoff の設計", CWD: "/repo/app"}
	other := domain.Session{SourceRef: domain.SessionRef{Source: "/b"}}
	q := NewQuery("handoff")
	if lifted, plain := scored(t, named, "handoff", q), scored(t, other, "handoff", q); lifted <= plain {
		t.Errorf("a matching title did not lift the score (%d vs %d)", lifted, plain)
	}
	// Both words are answered by the session's own fields, so both are rewarded.
	two := NewQuery("handoff app")
	if n := scored(t, named, "", two); n != 2*metaTermScore {
		t.Errorf("score=%d want %d", n, 2*metaTermScore)
	}
	if n := scored(t, named, "handoff", Query{}); n != 0 {
		t.Errorf("an empty query scored %d", n)
	}
}
