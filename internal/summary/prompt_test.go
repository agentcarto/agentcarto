package summary

import (
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

func conv(t *testing.T) (domain.Session, domain.Conversation, []transcript.Turn) {
	t.Helper()
	ts := func(n int64) time.Time { return time.Unix(n, 0).UTC() }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "コピーしたい", Prompt: "コピーしたい"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{
			{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: "$ cat <<EOF " + strings.Repeat("x", 3000) + " EOF", ToolDetail: "cat <<EOF\nbody\nEOF"},
			{Kind: domain.EventToolResult, Text: "ツール出力は要約の入力に載せない"},
			{Kind: domain.EventReasoning, Text: "思考も載せない"},
			{Kind: domain.EventAssistant, Text: "yキーを足した"},
		}},
		{ID: "u2", Parent: "a1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventUser, Text: "一回コミット", Prompt: "一回コミット"}}},
		{ID: "a2", Parent: "u2", Timestamp: ts(4), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "3a2b100 でコミットした"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s1", Title: "コピー機能"}
	return s, c, transcript.Turns(c, c.ActivePath())
}

func TestPromptCarriesTheConversationAndCutsTheTools(t *testing.T) {
	s, c, turns := conv(t)
	doc, asked := Prompt(s, c, turns, Options{})
	if len(asked) != 2 || asked[0] != 1 || asked[1] != 2 {
		t.Fatalf("asked=%v want [1 2]", asked)
	}
	for _, want := range []string{"コピーしたい", "yキーを足した", "一回コミット", "3a2b100", "## Turn 1", "## Turn 2"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the prompt is missing %q", want)
		}
	}
	// Tool output and thinking are what makes a log big and a summary no better.
	for _, unwanted := range []string{"ツール出力は要約の入力に載せない", "思考も載せない"} {
		if strings.Contains(doc, unwanted) {
			t.Errorf("the prompt carries %q", unwanted)
		}
	}
	// The 3000-character heredoc is cut; the call is still named.
	if strings.Contains(doc, strings.Repeat("x", 3000)) {
		t.Error("a heredoc went into the prompt whole")
	}
	if !strings.Contains(doc, "- Bash") || !strings.Contains(doc, "…") {
		t.Errorf("the tool call was dropped instead of cut:\n%s", doc)
	}
}

// A session that grew is summarized turn by turn: the prompt then carries only
// the turns that still need one.
func TestPromptCanAskAboutSomeTurnsOnly(t *testing.T) {
	s, c, turns := conv(t)
	doc, asked := Prompt(s, c, turns, Options{Turns: map[int]bool{2: true}})
	if len(asked) != 1 || asked[0] != 2 {
		t.Fatalf("asked=%v want [2]", asked)
	}
	if strings.Contains(doc, "コピーしたい") {
		t.Error("a turn that was not asked about went into the prompt")
	}
	if !strings.Contains(doc, "一回コミット") {
		t.Error("the turn that was asked about is missing")
	}
	// The header says the session holds more than the excerpt shows, so the
	// model does not read a fragment as the whole session.
	if !strings.Contains(doc, "1 of 2") {
		t.Errorf("the prompt does not say it is an excerpt:\n%s", doc)
	}
}

// asked names the turns the document actually describes, not the ones the
// caller offered. A turn that renders to nothing would otherwise be waited on
// forever: the model cannot summarize what it was not shown, and its silence
// would be read as "this turn has nothing to say".
func TestPromptAsksOnlyAboutTurnsTheDocumentDescribes(t *testing.T) {
	s, c, turns := conv(t)
	// A turn whose nodes are not in the conversation renders to nothing — the
	// same shape as a turn holding only events a document drops.
	turns = append(turns, transcript.Turn{Index: 9, Nodes: []string{"gone"}})
	doc, asked := Prompt(s, c, turns, Options{})
	for _, n := range asked {
		if n == 10 {
			t.Errorf("asked about turn 10, which the document does not describe: asked=%v", asked)
		}
	}
	if strings.Contains(doc, "## Turn 10") {
		t.Errorf("a turn with no content got a heading:\n%s", doc)
	}
	if len(asked) != 2 {
		t.Errorf("asked=%v want the two real turns", asked)
	}
}

func TestPromptOfNoTurns(t *testing.T) {
	s, c, turns := conv(t)
	doc, asked := Prompt(s, c, turns, Options{Turns: map[int]bool{}})
	if doc != "" || asked != nil {
		t.Fatalf("asking about no turns produced doc=%.40q asked=%v", doc, asked)
	}
}

// The session summary is written by two different calls — one that saw the whole
// session (System) and one built from the turn summaries (SessionSystem) — and
// both have to ask for a headline. If one stops asking, the sessions that take
// that path silently lose theirs.
func TestBothSessionPromptsAskForAHeadline(t *testing.T) {
	for name, prompt := range map[string]string{"System": System(""), "SessionSystem": SessionSystem("")} {
		if !strings.Contains(prompt, headlineMarker) {
			t.Errorf("%s does not ask for %s", name, headlineMarker)
		}
		if !strings.Contains(prompt, sessionMarker) {
			t.Errorf("%s does not ask for %s", name, sessionMarker)
		}
	}
}

// The prompts are in English; what the summaries come out in is a setting.
// Empty follows the session, which is what suits the person who had it.
func TestPromptsCarryTheLanguageSetting(t *testing.T) {
	for name, of := range map[string]func(string) string{"System": System, "SessionSystem": SessionSystem} {
		if got := of("日本語"); !strings.Contains(got, "in 日本語") {
			t.Errorf("%s does not pass the configured language through:\n%s", name, got)
		}
		auto := of("")
		if strings.Contains(auto, "in 日本語") {
			t.Errorf("%s kept a language it was not given", name)
		}
		if !strings.Contains(auto, "the language the session itself is written in") {
			t.Errorf("%s with no setting should follow the session:\n%s", name, auto)
		}
		// Whitespace is not a language.
		if of("   ") != auto {
			t.Errorf("%s treated blank as a language name", name)
		}
	}
}

// The instruction itself is English regardless of what it asks the summaries to
// be written in: a prompt in one language and summaries in another is what lets
// sessions in any language be summarized well.
func TestPromptsAreWrittenInEnglish(t *testing.T) {
	for name, prompt := range map[string]string{"System": System(""), "SessionSystem": SessionSystem("")} {
		for _, r := range prompt {
			if r > 0x3000 { // CJK and kana start here
				t.Errorf("%s holds non-ASCII instruction text: %q", name, string(r))
				break
			}
		}
	}
}

// The turn a growing session adds is often a reply to what the turn before it
// closed with — "はい", "実装" — and that turn was summarized on an earlier run,
// so it is not in this document. Its closing reply is carried over instead.
func TestPromptCarriesTheReplyTheAbsentTurnClosedWith(t *testing.T) {
	s, c, turns := conv(t)
	doc, asked := Prompt(s, c, turns, Options{Turns: map[int]bool{2: true}})

	if len(asked) != 1 || asked[0] != 2 {
		t.Fatalf("asked=%v want [2] — context is not a turn to summarize", asked)
	}
	if !strings.Contains(doc, transcript.ContextLabel) || !strings.Contains(doc, "> yキーを足した") {
		t.Errorf("the preceding reply was not carried over:\n%s", doc)
	}
	// Only the reply. The rest of that turn — its prompt, its tool calls — is
	// what the reader is not paying for.
	for _, unwanted := range []string{"コピーしたい", "- Bash"} {
		if strings.Contains(doc, unwanted) {
			t.Errorf("the absent turn's %q came along with its reply:\n%s", unwanted, doc)
		}
	}
	if strings.Index(doc, "> yキーを足した") > strings.Index(doc, "## Turn 2") {
		t.Errorf("the context is below the turn it belongs to:\n%s", doc)
	}
}

// A document that already holds the preceding turn carries no context: the turn
// itself is there, whole, and a quoted excerpt of it would be the same text
// twice.
func TestPromptCarriesNoContextWhenThePrecedingTurnIsThere(t *testing.T) {
	s, c, turns := conv(t)
	for name, o := range map[string]Options{
		"the whole session": {},
		"the first turn":    {Turns: map[int]bool{1: true}},
		"both turns":        {Turns: map[int]bool{1: true, 2: true}},
	} {
		if doc, _ := Prompt(s, c, turns, o); strings.Contains(doc, transcript.ContextLabel) {
			t.Errorf("%s: a context block was added where none was needed:\n%s", name, doc)
		}
	}
}

// A turn whose agent said nothing that survives to a document — a reply that is
// encrypted thinking and no text — leaves the turn after it without context.
// That is the state every prompt was in before this existed, not a failure.
func TestPromptCarriesNoContextFromASilentTurn(t *testing.T) {
	ts := func(n int64) time.Time { return time.Unix(n, 0).UTC() }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{
			{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: "$ go test"},
			{Kind: domain.EventAssistant, Text: "   "},
		}},
		{ID: "u2", Parent: "a1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventUser, Text: "はい", Prompt: "はい"}}},
		{ID: "a2", Parent: "u2", Timestamp: ts(4), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "直した"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s1"}
	turns := transcript.Turns(c, c.ActivePath())
	if doc, _ := Prompt(s, c, turns, Options{Turns: map[int]bool{2: true}}); strings.Contains(doc, transcript.ContextLabel) {
		t.Errorf("a turn that said nothing produced a context block:\n%s", doc)
	}
}

// The tail is what is kept: a reply's proposal, its question, the choices it
// offered are at its end, and that is what the next turn answers.
func TestPromptKeepsTheEndOfALongReply(t *testing.T) {
	ts := func(n int64) time.Time { return time.Unix(n, 0).UTC() }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "調べて", Prompt: "調べて"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{
			{Kind: domain.EventAssistant, Text: "冒頭の分析" + strings.Repeat("あ", contextRunes) + "どちらにしますか"},
		}},
		{ID: "u2", Parent: "a1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventUser, Text: "前者", Prompt: "前者"}}},
		{ID: "a2", Parent: "u2", Timestamp: ts(4), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "やった"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s1"}
	turns := transcript.Turns(c, c.ActivePath())
	doc, _ := Prompt(s, c, turns, Options{Turns: map[int]bool{2: true}})

	if !strings.Contains(doc, "どちらにしますか") {
		t.Errorf("the question the turn answers was cut off:\n%s", doc)
	}
	if strings.Contains(doc, "冒頭の分析") {
		t.Error("a reply longer than the limit went into the prompt whole")
	}
	if !strings.Contains(doc, "> …") {
		t.Errorf("a cut context does not say it was cut:\n%s", doc)
	}
}

func TestTailRunes(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"shorter than the limit": {"あい", "あい"},
		"exactly the limit":      {"あいう", "あいう"},
		"cut":                    {"あいうえお", "…うえお"},
		// The cut can land just before a space, which would otherwise print as
		// an ellipsis with a gap after it.
		"space at the seam": {"あいう えお", "…えお"},
	} {
		if got := tailRunes(tc.in, 3); got != tc.want {
			t.Errorf("%s: tailRunes(%q, 3)=%q want %q", name, tc.in, got, tc.want)
		}
	}
}

// The document holds one kind of block that is not a turn, and the instruction
// is the only thing that says so. If it stops naming the marker Prompt actually
// prints, the model reads the carried-over reply as a turn and writes a @@TURN
// section for one that was summarized on an earlier run.
func TestSystemNamesTheMarkerPromptPrints(t *testing.T) {
	s, c, turns := conv(t)
	doc, _ := Prompt(s, c, turns, Options{Turns: map[int]bool{2: true}})
	label := transcript.ContextLabel
	if !strings.Contains(doc, label) {
		t.Fatalf("the document carries no context block to describe:\n%s", doc)
	}
	if !strings.Contains(System(""), label) {
		t.Errorf("System does not name %q, which the document it is asked about carries", label)
	}
}
