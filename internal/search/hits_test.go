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
	return Hits(c, transcript.Turns(c, c.ActivePath()), NewQuery(query), o)
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
	if hits, total := Hits(domain.Conversation{}, nil, NewQuery("hello"), HitOptions{}); hits != nil || total != 0 {
		t.Fatalf("an empty conversation: %v %d", hits, total)
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
	hits, total := Hits(c, transcript.Turns(c, c.ActivePath()), q, HitOptions{Context: 2})
	if total != 2 || len(hits) != 2 {
		t.Fatalf("total=%d hits=%d", total, len(hits))
	}
	if !strings.Contains(hits[0].Snippet, "キャッシュ") || !strings.Contains(hits[1].Snippet, "cache") {
		t.Fatalf("snippets=%q, %q", hits[0].Snippet, hits[1].Snippet)
	}
	// The snippet is cut around the match, not from the start of the message.
	if strings.HasPrefix(hits[1].Snippet, "cache") == false && !strings.HasPrefix(hits[1].Snippet, "…") {
		t.Fatalf("snippet=%q", hits[1].Snippet)
	}
}
