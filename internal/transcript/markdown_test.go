package transcript

import (
	"fmt"
	"github.com/agentcarto/core/domain"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Only the prompt and the reply keep their bodies; the other events a document
// keeps become one-line entries, and the rest are dropped.
func TestEventMarkdownSelection(t *testing.T) {
	cases := []struct {
		name  string
		event domain.Event
		want  []string
	}{
		{"user", domain.Event{Kind: domain.EventUser, Text: "やって\n", Prompt: "やって"}, []string{"**USER**", "", "やって"}},
		{"queued", domain.Event{Kind: domain.EventQueued, Text: "急いで"}, []string{"**USER (queued)**", "", "急いで"}},
		{"assistant", domain.Event{Kind: domain.EventAssistant, Text: "できた"}, []string{"**ASSISTANT**", "", "できた"}},
		{"tool call", domain.Event{Kind: domain.EventToolCall, ToolName: "Read", ToolArg: "a.go"}, []string{"- Read a.go"}},
		{"multiline detail", domain.Event{Kind: domain.EventToolCall, ToolName: "Bash",
			ToolArg: "$ cat <<EOF body EOF", ToolDetail: "cat <<EOF\nbody\nEOF"},
			[]string{"- Bash", "", "  ```", "  cat <<EOF", "  body", "  EOF", "  ```"}},
		{"single-line detail", domain.Event{Kind: domain.EventToolCall, ToolName: "Bash",
			ToolArg: "$ go test ./...", ToolDetail: "go test ./..."}, []string{"- Bash $ go test ./..."}},
		// A subagent's report is kept out unless it is asked for, which
		// TestTaskReportIsAskedForNotImplied covers.
		{"task keeps its label only", domain.Event{Kind: domain.EventTask, ToolArg: "abc [done]", ToolDetail: "a\nreport"},
			[]string{"- TASK abc [done]"}},
		{"no arg", domain.Event{Kind: domain.EventToolCall, ToolName: "Read"}, []string{"- Read"}},
		{"unnamed tool", domain.Event{Kind: domain.EventToolCall, ToolArg: "x"}, []string{"- tool x"}},
		{"task", domain.Event{Kind: domain.EventTask, ToolArg: "explore"}, []string{"- TASK explore"}},
		{"attachment", domain.Event{Kind: domain.EventAttachment, ToolArg: "a.go"}, []string{"- attachment a.go"}},
		{"injected system", domain.Event{Kind: domain.EventUser, Text: "reminder"}, nil},
		{"thinking", domain.Event{Kind: domain.EventReasoning, Text: "考える"}, nil},
		{"tool result", domain.Event{Kind: domain.EventToolResult, Text: "package main"}, nil},
		{"system", domain.Event{Kind: domain.EventSystem, Text: "note"}, nil},
		{"empty reply", domain.Event{Kind: domain.EventAssistant, Text: "  "}, nil},
	}
	for _, c := range cases {
		got := eventLines(c.event, Options{})
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s: eventLines=%q want %q", c.name, got, c.want)
		}
	}
	// Nothing is truncated: a long single-line argument comes out whole.
	long := strings.Repeat("x", 3000)
	if got := eventLines(domain.Event{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: long}, Options{}); !strings.HasSuffix(got[0], long) {
		t.Errorf("a long argument was cut: %.60q…", got[0])
	}
}

// An argument that holds code fences of its own cannot break out of its block.
func TestToolEntryFenceOutgrowsTheArgument(t *testing.T) {
	got := toolEntry("Write", "# doc ```go x := 1 ```", "# doc\n```go\nx := 1\n```\n", Options{})
	if got[2] != "  ````" || got[len(got)-1] != "  ````" {
		t.Fatalf("fence=%q/%q want a four-backtick fence\n%q", got[2], got[len(got)-1], got)
	}
	for _, ln := range got[3 : len(got)-1] {
		if ln == "  ````" {
			t.Fatalf("the argument's own fence matched the block's:\n%q", got)
		}
	}
}

// Consecutive entries render as one tight list, not a paragraph each.
func TestTightenLists(t *testing.T) {
	in := []string{"- a", "", "- b", "", "  - c", "", "**USER**", "", "text",
		"- Bash", "", "  ```", "  - x", "", "  - y", "  ```", "- next"}
	want := []string{"- a", "- b", "  - c", "", "**USER**", "", "text",
		"- Bash", "", "  ```", "  - x", "", "  - y", "  ```", "- next"}
	if got := tightenLists(in); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tightenLists=%q want %q", got, want)
	}
}

// The document carries the session's metadata, then every displayed turn in
// chronological order under the number the turn view shows.
func TestExportMarkdownDocument(t *testing.T) {
	ts := func(h int) time.Time { return time.Date(2026, 8, 21, h, 4, 12, 0, time.UTC) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(10), Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって", Timestamp: ts(10)}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(11), Events: []domain.Event{
			{Kind: domain.EventToolCall, ToolName: "Read", ToolArg: "a.go", Text: "{}", Timestamp: ts(11)},
			{Kind: domain.EventToolResult, Text: "package main", Timestamp: ts(11)},
			{Kind: domain.EventReasoning, Text: "考える", Timestamp: ts(11)},
			{Kind: domain.EventAssistant, Text: "できた", Timestamp: ts(11)},
		}},
		{ID: "u2", Parent: "a1", Timestamp: ts(12), Events: []domain.Event{{Kind: domain.EventUser, Text: "次", Prompt: "次", Timestamp: ts(12)}}},
	})
	s := domain.Session{
		PluginID: "claude", AgentType: "claude", SessionID: "8f3a2b1c-4d5e", CWD: "/repo", Title: "りんごの話",
		StartedAt: ts(10), UpdatedAt: ts(12),
	}
	got, _ := Markdown(s, c, Turns(c, c.ActivePath()), Options{})
	for _, want := range []string{
		"# りんごの話\n",
		"- **Agent**: claude\n",
		"- **Session**: 8f3a2b1c-4d5e\n",
		"- **CWD**: `/repo`\n",
		"- **Started**: 2026-08-21 10:04:12\n",
		"- **Updated**: 2026-08-21 12:04:12\n",
		"- **Turns**: 2\n",
		"## Turn 1 — 2026-08-21 10:04:12\n",
		"**USER**\n\nやって\n",
		"- Read a.go\n",
		"**ASSISTANT**\n\nできた\n",
		"## Turn 2 — 2026-08-21 12:04:12\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("export is missing %q\n---\n%s", want, got)
		}
	}
	// Tool output and thinking are left out.
	for _, unwanted := range []string{"package main", "考える", "result ("} {
		if strings.Contains(got, unwanted) {
			t.Errorf("export contains %q, which it should drop\n---\n%s", unwanted, got)
		}
	}
	// Chronological: turn 1 before turn 2, the reverse of the on-screen order.
	if strings.Index(got, "## Turn 1") > strings.Index(got, "## Turn 2") {
		t.Error("turns are not in chronological order")
	}
}

// A /compact boundary is marked, because the agent stopped seeing what came before it.
func TestExportMarksCompactBoundary(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "first", Prompt: "first"}}},
		{ID: "c1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventUser, Text: "(summary)", RawType: "compact_summary"}}},
		{ID: "u2", Parent: "c1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventUser, Text: "second", Prompt: "second"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "x", Title: "t"}
	got, _ := Markdown(s, c, Turns(c, c.ActivePath()), Options{})
	if n := strings.Count(got, "_(context compacted here)_"); n != 1 {
		t.Fatalf("compact notices=%d want 1\n---\n%s", n, got)
	}
	// It belongs to the turn after the boundary, not the one before it.
	if strings.Index(got, "_(context compacted here)_") < strings.Index(got, "second") {
		if strings.Index(got, "first") > strings.Index(got, "_(context compacted here)_") {
			t.Error("the compact notice is attached to the wrong turn")
		}
	}
}

// A session with no title still produces a usable heading.
func TestExportUntitledSession(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "hi", Prompt: "hi"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "x"}
	got, _ := Markdown(s, c, Turns(c, c.ActivePath()), Options{})
	if !strings.HasPrefix(got, "# (untitled)\n") {
		t.Fatalf("export of an untitled session starts with %.40q", got)
	}
	if strings.Contains(got, "**CWD**") {
		t.Error("a session without a cwd still got a CWD line")
	}
}

// A turn holding only injected system messages produces no heading: an empty
// "## Turn 7" would be noise, and the numbering follows the turn view anyway.
func TestExportSkipsTurnsWithNothingToShow(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	c := domain.NewConversation([]domain.ConvNode{
		// Prompt-less user event: an injected reminder, dropped by the export.
		{ID: "s1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "<reminder>"}}},
		{ID: "u1", Parent: "s1", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "できた"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "x", Title: "t"}
	got, turns := Markdown(s, c, Turns(c, c.ActivePath()), Options{})
	if n := strings.Count(got, "## Turn "); n != 1 {
		t.Fatalf("turn headings=%d want 1 (the system-only turn is skipped)\n---\n%s", n, got)
	}
	// The stated count is what the document holds, not what the screen lists.
	if turns != 1 || !strings.Contains(got, "- **Turns**: 1") {
		t.Errorf("turns=%d and the header disagree with the single heading\n---\n%s", turns, got)
	}
	if strings.Contains(got, "<reminder>") {
		t.Errorf("an injected system message reached the export:\n%s", got)
	}
}

// ToolsLabel keeps the one-line form of a call the plugin already folded into a
// terminal row; ToolsNone leaves tool calls out altogether. Both exist for a
// reader paying for context: the full form of a heredoc can be many times the
// size of the conversation around it.
func TestToolModesTradeDetailForSize(t *testing.T) {
	e := domain.Event{Kind: domain.EventToolCall, ToolName: "Bash",
		ToolArg: "$ cat <<EOF body EOF", ToolDetail: "cat <<EOF\nbody\nEOF"}
	cases := []struct {
		mode ToolMode
		want []string
	}{
		{ToolsFull, []string{"- Bash", "", "  ```", "  cat <<EOF", "  body", "  EOF", "  ```"}},
		{ToolsLabel, []string{"- Bash $ cat <<EOF body EOF"}},
		{ToolsNone, nil},
	}
	for _, c := range cases {
		got := eventLines(e, Options{Tools: c.mode})
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("Tools=%d: %q want %q", c.mode, got, c.want)
		}
	}
}

// The conversation itself is never affected by the tool mode, and neither is the
// consolidated file section: dropping the calls must not drop what they did.
func TestToolsNoneKeepsTheConversationAndTheEdits(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
		{ID: "a1", Parent: "u1", Events: []domain.Event{
			{Kind: domain.EventToolCall, ToolName: "Read", ToolArg: "a.go"},
			{Kind: domain.EventFileChange, Changes: []domain.FileChange{{Path: abs("/repo/a.go"), Op: "update", Added: 1, Removed: 1}}},
			{Kind: domain.EventAssistant, Text: "できた"},
		}},
	})
	s := domain.Session{AgentType: "claude", SessionID: "x", CWD: abs("/repo"), Title: "t"}
	got, _ := Markdown(s, c, Turns(c, c.ActivePath()), Options{Tools: ToolsNone})
	if strings.Contains(got, "- Read a.go") {
		t.Errorf("a tool call survived ToolsNone:\n%s", got)
	}
	for _, want := range []string{"**USER**", "**ASSISTANT**", "- Edited files (1)", "  - M a.go (+1 -1)"} {
		if !strings.Contains(got, want) {
			t.Errorf("ToolsNone dropped %q:\n%s", want, got)
		}
	}
}

// Rendering no turns is a normal outcome, not an error: a caller asking for a
// range that holds nothing still gets the session's header, with a turn count
// that says the document is empty.
func TestMarkdownWithoutTurns(t *testing.T) {
	s := domain.Session{AgentType: "claude", SessionID: "x", Title: "t"}
	got, n := Markdown(s, domain.Conversation{}, nil, Options{})
	if n != 0 || !strings.Contains(got, "- **Turns**: 0") {
		t.Fatalf("rendered=%d doc=%q", n, got)
	}
	if strings.Contains(got, "## Turn ") {
		t.Fatalf("empty document carries a turn heading:\n%s", got)
	}
}

// An excerpt says so in its header: a reader (or an agent) given three turns of
// a forty-turn session must not take them for the whole thing.
func TestMarkdownHeaderNamesTheWholeSessionForAnExcerpt(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "a", Prompt: "a"}}},
		{ID: "u2", Parent: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "b", Prompt: "b"}}},
	})
	s := domain.Session{AgentType: "claude", SessionID: "x", Title: "t"}
	turns := Turns(c, c.ActivePath())
	if got, _ := Markdown(s, c, turns[:1], Options{SessionTurns: len(turns)}); !strings.Contains(got, "- **Turns**: 1 of 2") {
		t.Fatalf("excerpt header:\n%s", got)
	}
	// Without a selection the count stands on its own.
	if got, _ := Markdown(s, c, turns, Options{SessionTurns: len(turns)}); !strings.Contains(got, "- **Turns**: 2\n") {
		t.Fatalf("whole-session header:\n%s", got)
	}
}

// The outline says what each turn would cost to print, so the reader choosing
// turns can see that one of them is forty kilobytes before asking for it.
func TestOutlineCarriesTheSizeOfEachTurn(t *testing.T) {
	long := strings.Repeat("あ", 2000)
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "短い", Prompt: "短い"}}},
		{ID: "u2", Parent: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: long, Prompt: long}}},
	})
	s := domain.Session{AgentType: "claude", SessionID: "x", Title: "t"}
	got := Outline(s, c, Turns(c, c.ActivePath()), Options{})
	lines := strings.Split(got, "\n")
	var turn1, turn2 string
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "- Turn 1"):
			turn1 = ln
		case strings.HasPrefix(ln, "- Turn 2"):
			turn2 = ln
		}
	}
	if !strings.Contains(turn1, " B]") {
		t.Errorf("a short turn should be sized in bytes: %q", turn1)
	}
	if !strings.Contains(turn2, " KB]") {
		t.Errorf("a long turn should be sized in KB: %q", turn2)
	}
}

// The header says how many other branches the session holds, so a reader is not
// told "31 turns" about a session that also went three other ways.
func TestHeaderNamesOtherBranches(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "a", Prompt: "a"}}},
	})
	s := domain.Session{AgentType: "claude", SessionID: "x", Title: "t"}
	turns := Turns(c, c.ActivePath())
	got, _ := Markdown(s, c, turns, Options{Branches: 3})
	if !strings.Contains(got, "- **Other branches**: 3") {
		t.Fatalf("header:\n%s", got)
	}
	if got, _ := Markdown(s, c, turns, Options{}); strings.Contains(got, "Other branches") {
		t.Fatalf("a session with no branches should not mention them:\n%s", got)
	}
}

// The size an outline prints for a turn is the size that turn really takes when
// it is asked for: the same lines, measured the same way. A reader choosing
// turns by their cost has nothing else to go on.
func TestOutlineSizesMatchWhatMarkdownPrints(t *testing.T) {
	long := strings.Repeat("あ", 500)
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Events: []domain.Event{
			{Kind: domain.EventUser, Text: "短い", Prompt: "短い"},
			{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: "$ go test ./...", ToolDetail: "go test ./...\n# second line"},
		}},
		{ID: "u2", Parent: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: long, Prompt: long}}},
	})
	s := domain.Session{AgentType: "claude", SessionID: "x", CWD: abs("/repo"), Title: "t"}
	turns := Turns(c, c.ActivePath())
	for _, mode := range []ToolMode{ToolsFull, ToolsLabel, ToolsNone} {
		o := Options{Tools: mode}
		outline := Outline(s, c, turns, o)
		for _, turn := range turns {
			// What the outline claims for this turn…
			var claimed string
			for _, ln := range strings.Split(outline, "\n") {
				if strings.HasPrefix(ln, fmt.Sprintf("- Turn %d ", turn.Index+1)) {
					claimed = ln
				}
			}
			if claimed == "" {
				t.Fatalf("Tools=%d: turn %d is missing from the outline", mode, turn.Index+1)
			}
			// …must be the size of the lines Markdown prints for it.
			want := size(turnLines(c, turn, s.CWD, o))
			if !strings.Contains(claimed, want) {
				t.Errorf("Tools=%d: outline says %q, the turn measures %s", mode, claimed, want)
			}
		}
	}
}

// A subagent's report is prose the agent went and fetched, and it is what a
// search matches on inside a task — so a document asked for with TaskReports
// carries it. Nothing else does, including the export the TUI writes.
func TestTaskReportIsAskedForNotImplied(t *testing.T) {
	e := domain.Event{Kind: domain.EventTask, ToolArg: "explore the parser [done]",
		ToolDetail: "調べた結果、パーサは3か所で分岐している。"}
	full := eventLines(e, Options{Tools: ToolsFull, TaskReports: true})
	if len(full) < 3 || full[0] != "- TASK explore the parser [done]" {
		t.Fatalf("full=%q", full)
	}
	if strings.Join(full, "\n") != "- TASK explore the parser [done]\n\n**TASK report**\n\n調べた結果、パーサは3か所で分岐している。" {
		t.Fatalf("full form:\n%q", strings.Join(full, "\n"))
	}
	if label := eventLines(e, Options{Tools: ToolsLabel}); len(label) != 1 {
		t.Fatalf("the label form should be one line: %q", label)
	}
	// Asked for alongside the shorter call form, the report still comes: the two
	// choices are about different things.
	if both := eventLines(e, Options{Tools: ToolsLabel, TaskReports: true}); len(both) < 3 {
		t.Fatalf("label + reports: %q", both)
	}
	// The export the TUI writes asks for the full form of a call and nothing more:
	// a report would change what a session has always exported as.
	if export := eventLines(e, Options{}); len(export) != 1 {
		t.Fatalf("the export form should be one line: %q", export)
	}
	if none := eventLines(e, Options{Tools: ToolsNone}); len(none) != 0 {
		t.Fatalf("none=%q", none)
	}
	// A task that reported nothing is still a line.
	bare := domain.Event{Kind: domain.EventTask, ToolArg: "explore"}
	if got := eventLines(bare, Options{Tools: ToolsFull, TaskReports: true}); len(got) != 1 {
		t.Fatalf("a task without a report: %q", got)
	}
}

// ToolsBrief is the one mode that truncates: its reader is a model being asked
// what happened, not someone who might run the command again.
func TestToolsBriefCutsLongCallsAndKeepsShortOnes(t *testing.T) {
	long := strings.Repeat("x", 400)
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Unix(2, 0), Events: []domain.Event{
			{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: long, ToolDetail: "line1\nline2"},
			{Kind: domain.EventToolCall, ToolName: "Read", ToolArg: "a.go"},
			{Kind: domain.EventAssistant, Text: "できた"},
		}},
	})
	s := domain.Session{PluginID: "p", AgentType: "claude", SessionID: "x", Title: "t"}
	turns := Turns(c, c.ActivePath())
	doc, _ := Markdown(s, c, turns, Options{Tools: ToolsBrief})

	// The long call is cut and says so; the short one is untouched.
	if !strings.Contains(doc, "…") {
		t.Errorf("a 400-rune call was not marked as cut:\n%s", doc)
	}
	if strings.Contains(doc, long) {
		t.Error("the whole argument survived ToolsBrief")
	}
	if !strings.Contains(doc, "- Read a.go\n") {
		t.Errorf("a short call should be left alone:\n%s", doc)
	}
	// The conversation itself is never cut — it is what a summary is made of.
	for _, want := range []string{"**USER**", "やって", "**ASSISTANT**", "できた"} {
		if !strings.Contains(doc, want) {
			t.Errorf("ToolsBrief dropped %q from the conversation:\n%s", want, doc)
		}
	}
	// A multi-line detail never becomes a code block under ToolsBrief.
	if strings.Contains(doc, "```") {
		t.Errorf("ToolsBrief opened a code block:\n%s", doc)
	}
}

// Cutting counts runes, not bytes: half a multibyte character is worse than the
// ellipsis that replaces it.
func TestToolsBriefCutsOnRuneBoundaries(t *testing.T) {
	arg := strings.Repeat("あ", 400)
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "p", Prompt: "p"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Unix(2, 0), Events: []domain.Event{
			{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: arg},
		}},
	})
	s := domain.Session{PluginID: "p", AgentType: "claude", SessionID: "x", Title: "t"}
	doc, _ := Markdown(s, c, Turns(c, c.ActivePath()), Options{Tools: ToolsBrief})
	if !utf8.ValidString(doc) {
		t.Fatal("the document is not valid UTF-8 after cutting")
	}
	// "Bash " is 5 runes, so 195 of the 400 fit within the 200-rune limit.
	if n := strings.Count(doc, "あ"); n != 195 {
		t.Errorf("kept %d runes of the argument, want 195", n)
	}
}

// A summary says what came of a turn; the headline says what was asked for. The
// outline carries both, because a person finds a turn by what they asked and an
// agent chooses one by what happened.
func TestOutlineCarriesSummariesBesideHeadlines(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "一回コミット", Prompt: "一回コミット"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "した"}}},
		{ID: "u2", Parent: "a1", Timestamp: time.Unix(3, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "done", Prompt: "done"}}},
		{ID: "a2", Parent: "u2", Timestamp: time.Unix(4, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "確認した"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "x", Title: "t"}
	turns := Turns(c, c.ActivePath())
	got := Outline(s, c, turns, Options{Summaries: map[int]string{
		0: "コピー機能を実装しv0.11.0をリリース",
		1: "clipboard関連をmainに1コミット（3a2b100）",
		// Turn 2 has none: a partial map is the normal case.
	}})

	if !strings.Contains(got, "コピー機能を実装しv0.11.0をリリース") {
		t.Errorf("the session summary is missing from the header:\n%s", got)
	}
	if !strings.Contains(got, "  ↳ clipboard関連をmainに1コミット（3a2b100）") {
		t.Errorf("turn 1's summary is missing:\n%s", got)
	}
	// The headline stays: it is how a person recognizes the turn they remember.
	if !strings.Contains(got, "一回コミット") {
		t.Errorf("the summary replaced the headline:\n%s", got)
	}
	// A turn without a summary keeps the line it always had, with no marker.
	for _, ln := range strings.Split(got, "\n") {
		if strings.Contains(ln, "done") && strings.Contains(ln, "↳") {
			t.Errorf("turn 2 got a summary marker without a summary: %q", ln)
		}
	}
}

// An outline is an index. A summary generated without a length limit runs past
// a thousand characters, which would make the list unreadable at the one place
// it exists to be scanned.
func TestOutlineCutsALongSummary(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "p", Prompt: "p"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "a"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "x", Title: "t"}
	long := strings.Repeat("あ", 1200)
	got := Outline(s, c, Turns(c, c.ActivePath()), Options{Summaries: map[int]string{1: long}})
	if strings.Contains(got, long) {
		t.Error("a 1200-character summary went into the outline whole")
	}
	if !strings.Contains(got, "…") {
		t.Errorf("the summary was not marked as cut:\n%s", got)
	}
	// A multi-line summary is folded onto the entry's own line.
	got = Outline(s, c, Turns(c, c.ActivePath()), Options{Summaries: map[int]string{1: "一行目\n二行目"}})
	if !strings.Contains(got, "  ↳ 一行目 二行目") {
		t.Errorf("a multi-line summary was not folded:\n%s", got)
	}
}

// show --turns N prints the summary whole, above the turn: whoever opened this
// far still has to decide how much of it to read.
func TestMarkdownPutsTheSummaryAboveTheTurn(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "できた"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "x", Title: "t"}
	long := strings.Repeat("あ", 1200)
	got, _ := Markdown(s, c, Turns(c, c.ActivePath()), Options{Summaries: map[int]string{
		0: "セッション全体",
		1: "行1\n行2",
	}})
	// Not cut here, unlike the outline.
	whole, _ := Markdown(s, c, Turns(c, c.ActivePath()), Options{Summaries: map[int]string{1: long}})
	if !strings.Contains(whole, long) {
		t.Error("--turns cut the summary; it should print it whole")
	}
	if !strings.Contains(got, "> 行1\n> 行2") {
		t.Errorf("a multi-line summary was not quoted line by line:\n%s", got)
	}
	if strings.Index(got, "> 行1") > strings.Index(got, "できた") {
		t.Error("the summary comes after the turn body")
	}
	if !strings.Contains(got, "セッション全体") {
		t.Errorf("the session summary is missing:\n%s", got)
	}
}

// Everything above must leave a document without summaries exactly as it was.
func TestWithoutSummariesNothingChanges(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "できた"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "x", Title: "t"}
	turns := Turns(c, c.ActivePath())
	for _, o := range []Options{{}, {Summaries: nil}, {Summaries: map[int]string{}}, {Summaries: map[int]string{1: "   "}}} {
		if got := Outline(s, c, turns, o); strings.Contains(got, "↳") {
			t.Errorf("an outline with no summaries carries a marker:\n%s", got)
		}
		if got, _ := Markdown(s, c, turns, o); strings.Contains(got, "> ") {
			t.Errorf("a document with no summaries carries a quote:\n%s", got)
		}
	}
}
