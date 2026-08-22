package search

import (
	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
	"strings"
	"testing"
	"time"
)

func node(id, parent string, events ...domain.Event) domain.ConvNode {
	return domain.ConvNode{ID: id, Parent: parent, Events: events}
}

func prompt(text string) domain.Event {
	return domain.Event{Kind: domain.EventUser, Text: text, Prompt: text}
}

func reply(text string) domain.Event {
	return domain.Event{Kind: domain.EventAssistant, Text: text}
}

func hitsOf(c domain.Conversation, query string, o HitOptions) ([]Hit, int) {
	hits, sum := Hits(c, transcript.Turns(c, c.ActivePath()), NewQuery(query), o)
	return hits, sum.Total
}

func TestHitsCarryTheTurnNumberTheTurnViewShows(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		node("u1", "", prompt("handoff の順序は？")),
		node("a1", "u1", reply("プラグインを先に落とす")),
		node("u2", "a1", prompt("次の話")),
		node("a2", "u2", reply("handoff とは無関係")),
	})
	hits, total := hitsOf(c, "handoff", HitOptions{})
	if total != 2 || len(hits) != 2 {
		t.Fatalf("total=%d hits=%d", total, len(hits))
	}
	// turn #1 is the first prompt and its reply; turn #2 the second.
	if hits[0].Turn != 1 || hits[1].Turn != 2 {
		t.Fatalf("turns=%d,%d want 1,2", hits[0].Turn, hits[1].Turn)
	}
	if hits[0].Kind != domain.EventUser || hits[1].Kind != domain.EventAssistant {
		t.Fatalf("kinds=%q,%q", hits[0].Kind, hits[1].Kind)
	}
}

// The searched events are the ones the index holds — messages, task reports and
// the one-line form of a tool call. Anything else would produce hits in sessions
// the index cannot find at all.
func TestHitsSearchTheSameEventsAsTheIndex(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		node("u1", "", prompt("start")),
		node("a1", "u1",
			domain.Event{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: "$ grep needle x"},
			domain.Event{Kind: domain.EventToolResult, Text: "needle found"},
			domain.Event{Kind: domain.EventReasoning, Text: "needle かもしれない"},
			domain.Event{Kind: domain.EventSystem, Text: "needle reminder"},
			domain.Event{Kind: domain.EventQueued, Text: "needle を探して"},
		),
		node("a2", "a1", domain.Event{Kind: domain.EventTask, Text: "task", ToolDetail: "report about needle"}),
	})
	hits, total := hitsOf(c, "needle", HitOptions{})
	if total != 3 {
		t.Fatalf("total=%d want 3 (the tool call, the queued message and the task report)", total)
	}
	kinds := []domain.EventKind{hits[0].Kind, hits[1].Kind, hits[2].Kind}
	if kinds[0] != domain.EventToolCall || kinds[1] != domain.EventQueued || kinds[2] != domain.EventTask {
		t.Fatalf("kinds=%v want tool_call, queued, task", kinds)
	}
	// The tool call is covered by its name and one-line argument, not by its body:
	// a heredoc writing a file would otherwise put the whole file in the index.
	c = domain.NewConversation([]domain.ConvNode{node("a1", "",
		domain.Event{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: "$ go test ./...",
			ToolDetail: "go test ./...\n# needle lives only in the body"})})
	if _, total := hitsOf(c, "needle", HitOptions{}); total != 0 {
		t.Fatalf("the expanded tool body was searched: total=%d", total)
	}
}

func TestHitsFoldCaseAndReportTheTotal(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		node("u1", "", prompt("Handoff")),
		node("a1", "u1", reply("HANDOFF"), reply("handoff")),
	})
	hits, total := hitsOf(c, "  hAnDoFf ", HitOptions{Max: 2})
	if total != 3 {
		t.Fatalf("total=%d want 3 regardless of the cap", total)
	}
	if len(hits) != 2 {
		t.Fatalf("hits=%d want the 2 the cap allows", len(hits))
	}
	// The newest matches are the ones kept: the prompt is dropped, not the replies.
	for _, h := range hits {
		if h.Kind != domain.EventAssistant {
			t.Fatalf("the cap kept the oldest match instead of the newest: %#v", hits)
		}
	}
}

// A snippet is one line: enough context to judge the hit, with the line breaks
// folded away and the cut ends marked.
func TestHitsSnippetIsOneMarkedLine(t *testing.T) {
	long := strings.Repeat("あ", 300) + "needle" + strings.Repeat("い", 300)
	c := domain.NewConversation([]domain.ConvNode{node("u1", "", prompt(long))})
	hits, _ := hitsOf(c, "needle", HitOptions{Context: 5})
	if len(hits) != 1 {
		t.Fatalf("hits=%d", len(hits))
	}
	if got, want := hits[0].Snippet, "…"+strings.Repeat("あ", 5)+"needle"+strings.Repeat("い", 5)+"…"; got != want {
		t.Fatalf("snippet=%q want %q", got, want)
	}

	c = domain.NewConversation([]domain.ConvNode{node("u1", "", prompt("first line\n\n  needle  \nlast"))})
	hits, _ = hitsOf(c, "needle", HitOptions{})
	if got := hits[0].Snippet; got != "first line needle last" {
		t.Fatalf("snippet=%q, want the whole text on one line without markers", got)
	}
}

func TestHitsOfNothing(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{node("u1", "", prompt("hello"))})
	if hits, total := hitsOf(c, "  ", HitOptions{}); hits != nil || total != 0 {
		t.Fatalf("a blank query should locate nothing: %v %d", hits, total)
	}
	if hits, total := hitsOf(c, "absent", HitOptions{}); hits != nil || total != 0 {
		t.Fatalf("a query that is not there: %v %d", hits, total)
	}
	if hits, sum := Hits(domain.Conversation{}, nil, NewQuery("hello"), HitOptions{}); hits != nil || sum.Total != 0 {
		t.Fatalf("an empty conversation: %v %d", hits, sum.Total)
	}
}

// A hit carries the event's own timestamp, which is what an agent uses to tell
// two sessions about the same work apart.
func TestHitsKeepTheEventTimestamp(t *testing.T) {
	at := time.Date(2026, 8, 21, 10, 4, 12, 0, time.UTC)
	e := prompt("needle")
	e.Timestamp = at
	c := domain.NewConversation([]domain.ConvNode{node("u1", "", e)})
	hits, _ := hitsOf(c, "needle", HitOptions{})
	if !hits[0].Timestamp.Equal(at) {
		t.Fatalf("timestamp=%v want %v", hits[0].Timestamp, at)
	}
}

// With several terms, a hit is an event holding any one of them: the session as
// a whole is what has to hold them all.
func TestHitsFindEachTermOfAQuery(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		node("u1", "", prompt("fork の話")),
		node("a1", "u1", reply("relocate も見る")),
		node("u2", "a1", prompt("関係のない話")),
	})
	hits, total := hitsOf(c, "fork relocate", HitOptions{Context: 4})
	if total != 2 || len(hits) != 2 {
		t.Fatalf("total=%d hits=%d want 2 and 2", total, len(hits))
	}
	if !strings.Contains(hits[0].Snippet, "fork") || !strings.Contains(hits[1].Snippet, "relocate") {
		t.Fatalf("snippets are not cut around the term that matched: %q / %q", hits[0].Snippet, hits[1].Snippet)
	}
}

// The snippet is cut around the term that occurs first, whichever it is.
func TestHitsCutAroundTheEarliestTerm(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{node("u1", "",
		prompt(strings.Repeat("あ", 50)+"relocate"+strings.Repeat("い", 200)+"fork"))})
	hits, _ := hitsOf(c, "fork relocate", HitOptions{Context: 3})
	if got := hits[0].Snippet; !strings.Contains(got, "relocate") || strings.Contains(got, "fork") {
		t.Fatalf("snippet=%q, want it cut around the first term in the text", got)
	}
}

// turnsOf is the turn list of a whole conversation, used where a test does not
// care which branch it is on.
func turnsOf(c domain.Conversation) []transcript.Turn { return transcript.Turns(c, c.ActivePath()) }

// A pattern locates the same way words do: the snippet is cut around what
// actually matched, which for an alternation is whichever branch was found.
func TestHitsWithARegexpQuery(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		node("u1", "", prompt("キャッシュを消す")),
		node("a1", "u1", reply("cache を消しました")),
		node("u2", "a1", prompt("無関係な話")),
	})
	q, err := NewRegexpQuery("cache|キャッシュ")
	if err != nil {
		t.Fatal(err)
	}
	hits, sum := Hits(c, transcript.Turns(c, c.ActivePath()), q, HitOptions{Context: 2})
	if sum.Total != 2 || len(hits) != 2 {
		t.Fatalf("total=%d hits=%d", sum.Total, len(hits))
	}
	if !strings.Contains(hits[0].Snippet, "キャッシュ") || !strings.Contains(hits[1].Snippet, "cache") {
		t.Fatalf("snippets=%q, %q", hits[0].Snippet, hits[1].Snippet)
	}
	// The snippet is cut around the match, not from the start of the message.
	if strings.HasPrefix(hits[1].Snippet, "cache") == false && !strings.HasPrefix(hits[1].Snippet, "…") {
		t.Fatalf("snippet=%q", hits[1].Snippet)
	}
}

func call(name, arg string) domain.Event {
	return domain.Event{Kind: domain.EventToolCall, ToolName: name, ToolArg: arg}
}

func summaryOf(c domain.Conversation, query string) Summary {
	_, sum := Hits(c, transcript.Turns(c, c.ActivePath()), NewQuery(query), HitOptions{})
	return sum
}

// A session whose only matching tool call ran agentcarto looked the subject up.
// One that ran anything else — read a file, built something — worked on it, and
// so did one that never called a tool at all.
func TestSummaryTellsALookupFromWork(t *testing.T) {
	lookup := domain.NewConversation([]domain.ConvNode{
		node("u1", "", prompt("png の話を探して")),
		node("a1", "u1", call("Bash", "$ agentcarto search --regex 'png|PNG'"),
			reply("png の本命は 57cdf262 でした")),
	})
	sum := summaryOf(lookup, "png")
	if sum.Total != 3 || sum.ToolCalls != 1 || sum.SelfCalls != 1 {
		t.Fatalf("summary=%+v want 3 hits, 1 tool call, 1 of them agentcarto", sum)
	}
	if !sum.OnlyRanAgentcarto() {
		t.Error("a session that only ran the search should be recognized as one")
	}

	work := domain.NewConversation([]domain.ConvNode{
		node("u1", "", prompt("png の表紙を見て")),
		node("a1", "u1", call("Read", "/repo/png/cover.png"), reply("見ました")),
	})
	if sum := summaryOf(work, "png"); sum.OnlyRanAgentcarto() || sum.SelfCalls != 0 {
		t.Errorf("work was taken for a lookup: %+v", sum)
	}

	// This project's own files live under directories called agentcarto. Reading
	// one is work on the command, not a lookup with it — and if it counted, a
	// search for one of these paths would be told there is nothing.
	onItself := domain.NewConversation([]domain.ConvNode{
		node("u1", "", prompt("show.go を直す"),
			call("Read", "/home/ubuntu/repo/agentcarto/agentcarto/cmd/agentcarto/show.go")),
	})
	if sum := summaryOf(onItself, "cmd/agentcarto/show.go"); sum.OnlyRanAgentcarto() {
		t.Errorf("reading a file under an agentcarto directory was taken for a lookup: %+v", sum)
	}

	// However the command was spelled, running it is a lookup.
	for _, arg := range []string{
		"$ agentcarto search --regex 'png|PNG'",
		"$ /home/ubuntu/repo/agentcarto/agentcarto/bin/agentcarto search png",
		"$ agentcarto --no-cache list --cwd /repo/png",
		"$ timeout 900 agentcarto show 57cdf262 | grep png",
	} {
		c := domain.NewConversation([]domain.ConvNode{node("u1", "", call("Bash", arg))})
		if sum := summaryOf(c, "png"); !sum.OnlyRanAgentcarto() {
			t.Errorf("%q was not recognized as running agentcarto: %+v", arg, sum)
		}
	}
	// Naming the directory is not running the command.
	for _, arg := range []string{
		"$ cd /home/ubuntu/repo/agentcarto && make check",
		"$ grep -rn png /home/ubuntu/repo/agentcarto/",
	} {
		c := domain.NewConversation([]domain.ConvNode{node("u1", "", call("Bash", arg))})
		if sum := summaryOf(c, "png"); sum.SelfCalls != 0 {
			t.Errorf("%q was counted as running agentcarto: %+v", arg, sum)
		}
	}

	// One agentcarto call among others is a session that did something else too.
	mixed := domain.NewConversation([]domain.ConvNode{
		node("u1", "", call("Bash", "$ agentcarto search png"), call("Read", "/repo/png/cover.png")),
	})
	if summaryOf(mixed, "png").OnlyRanAgentcarto() {
		t.Error("a session that also read a file is not a lookup")
	}

	// Talking about a subject is not searching for it.
	talkOnly := domain.NewConversation([]domain.ConvNode{
		node("u1", "", prompt("png は可逆圧縮"), reply("そうですね")),
	})
	if summaryOf(talkOnly, "png").OnlyRanAgentcarto() {
		t.Error("a session with no matching tool call is not a lookup")
	}
}

// Looking for agentcarto itself is asking for the sessions that ran it, so the
// rule that hides them stands down.
func TestIsSelfQuery(t *testing.T) {
	for _, q := range []string{"agentcarto", "agentcarto search", "AgentCarto"} {
		if !IsSelfQuery(NewQuery(q)) {
			t.Errorf("%q should be recognized as a search for agentcarto", q)
		}
	}
	// The search matches on substrings, so a prefix of the name finds the same
	// sessions and counts as the same question.
	if !IsSelfQuery(NewQuery("agentcart")) {
		t.Error("a prefix of the name finds the same sessions and should count")
	}
	if IsSelfQuery(NewQuery("png キャッシュ")) {
		t.Error("an unrelated query should not count")
	}
	re, err := NewRegexpQuery("agent(carto|smith)")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSelfQuery(re) {
		t.Error("a pattern that matches the name should count")
	}
}
