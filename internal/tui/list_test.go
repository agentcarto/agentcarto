package tui

import (
	"fmt"
	searchpkg "github.com/agentcarto/agentcarto/internal/search"
	convlogic "github.com/agentcarto/core/conversation"
	"github.com/agentcarto/core/domain"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestStatusMarkPrototypeFormat(t *testing.T) {
	cases := []struct {
		s    domain.Session
		want string
	}{{domain.Session{Status: domain.StatusRunning, LastKind: domain.EventToolCall}, "● TOOL"}, {domain.Session{Status: domain.StatusRunning, LastKind: domain.EventUser}, "● THINK"}, {domain.Session{Status: domain.StatusRunning, PermissionWait: true}, "● ASK"}, {domain.Session{Status: domain.StatusReady}, "○ READY"}, {domain.Session{Status: domain.StatusOther}, "· OTHER"}}
	for _, c := range cases {
		got := statusMark(c.s)
		if !strings.Contains(got, c.want) || lipgloss.Width(got) != 8 {
			t.Fatalf("%q width=%d", got, lipgloss.Width(got))
		}
	}
}
func TestSessionRowContainsPrototypeMetadata(t *testing.T) {
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "12345678-abcd", CWD: "/home/u/repo", UpdatedAt: time.Date(2026, 6, 23, 12, 34, 0, 0, time.Local), Title: "title"}
	idx := searchpkg.New(100)
	idx.Set(s, "", 42)
	m := Model{width: 100, view: "time", index: idx}
	row := m.sessionRow(s, false, 42, "")
	for _, want := range []string{"06-23 12:34", "codex", "42", "12345678", "/home/u/repo", "title"} {
		if !strings.Contains(row, want) {
			t.Fatalf("missing %q in %q", want, row)
		}
	}
}
func TestFilterExcludesEmptyForks(t *testing.T) {
	ts := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "time", sessions: []domain.Session{
		{PluginID: "claude", SessionID: "normal", UpdatedAt: ts},
		{PluginID: "claude", SessionID: "empty", UpdatedAt: ts, ParentSessionID: "p", ForkAt: "x", EmptyFork: true},
		{PluginID: "claude", SessionID: "continued", UpdatedAt: ts, ParentSessionID: "p", ForkAt: "x"},
	}}
	m.filter()
	var ids []string
	for _, ix := range m.filtered {
		ids = append(ids, m.sessions[ix].SessionID)
	}
	for _, id := range ids {
		if id == "empty" {
			t.Fatalf("empty fork should be hidden, got %v", ids)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 visible (normal, continued), got %v", ids)
	}
}

func TestFilterTimeViewNewestFirst(t *testing.T) {
	old := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	newer := old.Add(time.Hour)
	m := Model{view: "time", sessions: []domain.Session{
		{PluginID: "codex", SessionID: "old", UpdatedAt: old},
		{PluginID: "claude", SessionID: "new", UpdatedAt: newer},
		{PluginID: "grok", SessionID: "same", UpdatedAt: newer},
	}}
	m.filter()
	got := []string{m.sessions[m.filtered[0]].SessionID, m.sessions[m.filtered[1]].SessionID, m.sessions[m.filtered[2]].SessionID}
	want := []string{"new", "same", "old"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}
func TestFilterFolderViewGroupsNewestFirst(t *testing.T) {
	t0 := time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local)
	m := Model{view: "folder", sessions: []domain.Session{
		{PluginID: "codex", SessionID: "a-old", CWD: "/a", UpdatedAt: t0},
		{PluginID: "codex", SessionID: "b-new", CWD: "/b", UpdatedAt: t0.Add(3 * time.Hour)},
		{PluginID: "codex", SessionID: "a-new", CWD: "/a", UpdatedAt: t0.Add(time.Hour)},
	}}
	m.filter()
	got := []string{m.sessions[m.filtered[0]].SessionID, m.sessions[m.filtered[1]].SessionID, m.sessions[m.filtered[2]].SessionID}
	want := []string{"b-new", "a-new", "a-old"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}
func TestListCursorScrollsAndMovesWithinViewport(t *testing.T) {
	var sessions []domain.Session
	for i := 0; i < 8; i++ {
		sessions = append(sessions, domain.Session{
			PluginID:  "codex",
			AgentType: "codex",
			SessionID: fmt.Sprintf("s%d", i),
			UpdatedAt: time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local).Add(time.Duration(8-i) * time.Minute),
			Title:     fmt.Sprintf("session %d", i),
		})
	}
	m := Model{view: "time", width: 100, height: 6, sessions: sessions}
	m.filter()
	for i := 0; i < 5; i++ {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = updated.(Model)
	}
	// height=6 -> body = height-3 = 3 lines (line 0=head, line 1=search/status line, last=footer).
	// To fit cursor=5, offset must be 5-3+1=3.
	if m.cursor != 5 || m.offset != 3 {
		t.Fatalf("after down cursor=%d offset=%d, want cursor=5 offset=3", m.cursor, m.offset)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = updated.(Model)
	if m.cursor != 4 || m.offset != 3 {
		t.Fatalf("after up cursor=%d offset=%d, want cursor=4 offset=3", m.cursor, m.offset)
	}
	visibleRow := m.cursor - m.offset
	if visibleRow != 1 {
		t.Fatalf("cursor should move up within viewport, visible row=%d", visibleRow)
	}
}

// Even when the list is shorter than the viewport, the footer stays pinned to the last line, and
// the row layout (total line count, starting line) does not change with or without an active search
// (a regression test for the footer floating up / the list shifting).
func TestListFooterPinnedAndStableLayout(t *testing.T) {
	var sessions []domain.Session
	for i := 0; i < 3; i++ { // a list well shorter than height=12
		sessions = append(sessions, domain.Session{
			PluginID: "codex", AgentType: "codex", SessionID: fmt.Sprintf("s%d", i),
			UpdatedAt: time.Date(2026, 6, 23, 10, 0, 0, 0, time.Local).Add(time.Duration(i) * time.Minute),
			Title:     fmt.Sprintf("session %d", i),
		})
	}
	m := Model{view: "time", width: 80, height: 12, sessions: sessions}
	m.filter()

	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Fatalf("rendered %d lines, want exactly height=%d", len(lines), m.height)
	}
	if !strings.Contains(lines[m.height-1], "move") {
		t.Fatalf("footer must be on the last line, got %q", lines[m.height-1])
	}

	// Starting/ending a search must not move the total line count or footer position.
	m.searching = true
	if l := strings.Split(m.View(), "\n"); len(l) != m.height || !strings.Contains(l[m.height-1], "move") {
		t.Fatalf("searching layout changed: lines=%d footer=%q", len(l), l[m.height-1])
	}
}

func TestShortCWDMiddleEllipsis(t *testing.T) {
	got := shortCWD("/very/long/project/path/to/repository", 20)
	if !strings.Contains(got, "…") || lipgloss.Width(got) > 20 {
		t.Fatal(got)
	}
}
func TestDetailViewPrototypeColorsAndMetadata(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "12345678-x", CWD: "/repo", Title: "title", Status: domain.StatusRunning, LastKind: domain.EventToolCall}
	c := domain.NewConversation([]domain.ConvNode{{ID: "u", Events: []domain.Event{{Kind: domain.EventUser, Text: "question", Prompt: "question", Timestamp: time.Date(2026, 6, 23, 1, 2, 0, 0, time.Local)}}}, {ID: "a", Parent: "u", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}, {Kind: domain.EventToolCall, ToolName: "Bash", Text: "ls"}}}})
	m := Model{width: 100, detailSession: &s, detail: &c, detailTurns: [][]string{{"u", "a"}}}
	out := m.detailView()
	for _, want := range []string{"● TOOL", "claude", "12345678", "#1", "06-23 01:02", "↩1", "⚙1", "question"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	if !strings.Contains(out, "\x1b[7m") && !strings.Contains(out, "\x1b[48;") {
		t.Fatal("selection style missing")
	}
	m.openCurrentTurn(true)
	out = m.detailView()
	for _, want := range []string{"turn #1/1", "USER", "ASSISTANT", "◆ Bash"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in full turn view %q", want, out)
		}
	}
}

func TestEventBlockPrototypeTaskAndPseudoUser(t *testing.T) {
	task := eventBlock(domain.Event{Kind: domain.EventTask, ToolArg: "abcdef12 [done]", ToolDetail: "sum\n\nresult line"})
	if task.Sym != "⤤" || task.Style != "task" || !strings.Contains(task.Label, "TASK abcdef12 [done]") {
		t.Fatalf("task block=%#v", task)
	}
	pseudo := eventBlock(domain.Event{Kind: domain.EventUser, Text: "<system-reminder>read only</system-reminder>"})
	if pseudo.Sym != "#" || pseudo.Style != "meta" || !strings.HasPrefix(pseudo.Label, "system: ") {
		t.Fatalf("pseudo block=%#v", pseudo)
	}
}

// Changes-bearing events (edit tool calls, file changes) are surfaced by the
// consolidated file section, not the chronological block list. A file_change
// WITHOUT Changes stays visible in the timeline instead of vanishing.
func TestChangesBearingEventsSkipTimeline(t *testing.T) {
	edit := domain.Event{Kind: domain.EventToolCall, ToolName: "Edit", Changes: []domain.FileChange{{Path: "a.go"}}}
	fc := domain.Event{Kind: domain.EventFileChange, Changes: []domain.FileChange{{Path: "a.go"}}}
	plain := domain.Event{Kind: domain.EventToolCall, ToolName: "Read", ToolArg: "/x/c.py"}
	bare := domain.Event{Kind: domain.EventFileChange, Text: "raw"}
	if !skipInFileSection(edit) || !skipInFileSection(fc) || skipInFileSection(plain) || skipInFileSection(bare) {
		t.Fatalf("skipInFileSection: edit=%v fc=%v plain=%v bare=%v", skipInFileSection(edit), skipInFileSection(fc), skipInFileSection(plain), skipInFileSection(bare))
	}
}

func TestToolCallLabelBodyPrototypeBashShell(t *testing.T) {
	fg := eventBlock(domain.Event{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: "$ ls -la echo done", ToolDetail: "ls -la\necho done"})
	if !strings.Contains(fg.Label, "Bash") || !strings.Contains(fg.Label, "$ ls -la echo done") || strings.Contains(fg.Label, "&") {
		t.Fatalf("foreground bash label=%q", fg.Label)
	}
	if len(fg.Body) != 2 || fg.Body[0] != "ls -la" || fg.Body[1] != "echo done" {
		t.Fatalf("foreground bash body=%#v", fg.Body)
	}
	bg := eventBlock(domain.Event{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: "$ sleep 100 &", ToolDetail: "sleep 100\n\n(run in background)"})
	if !strings.HasSuffix(bg.Label, "$ sleep 100 &") {
		t.Fatalf("background bash label=%q", bg.Label)
	}
}

func TestTurnMarkPartsPrototypeTaskRetryAndEditStats(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: time.Date(2026, 6, 23, 1, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "question", Prompt: "question"}}},
		{ID: "a", Parent: "u", Timestamp: time.Date(2026, 6, 23, 1, 2, 0, 0, time.Local), Events: []domain.Event{
			{Kind: domain.EventQueued, Text: "queued question"},
			{Kind: domain.EventTask, ToolArg: "t"},
			{Kind: domain.EventToolCall, ToolName: "Edit", Changes: []domain.FileChange{{Path: "a.go", Added: 1, Removed: 1, Diff: "@@\n-old\n+new"}}},
		}},
		{ID: "side", Parent: "u", Timestamp: time.Date(2026, 6, 23, 1, 1, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "retry"}}},
	})
	m := Model{detail: &c}
	parts := m.turnMarkParts([]string{"u", "a"}, time.Time{})
	if parts[1] != "▶1" || parts[4] != "⤷1" || parts[5] != "↺1" || parts[6] != "*1" || parts[7] != "+1" || parts[8] != "-1" {
		t.Fatalf("parts=%#v", parts)
	}
}

func TestFormatDurationPrototypeUnits(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, ""},
		{3 * time.Second, "3s"},
		{59 * time.Second, "59s"},
		{62 * time.Second, "1m02s"},
		{3599 * time.Second, "59m59s"},
		{3700 * time.Second, "1h01m"},
		{25 * time.Hour, "1d01h"},
	}
	for _, c := range cases {
		if got := formatDuration(c.d); got != c.want {
			t.Fatalf("formatDuration(%s)=%q want %q", c.d, got, c.want)
		}
	}
}

func TestEventBlockLineCounts(t *testing.T) {
	reasoning := eventBlock(domain.Event{Kind: domain.EventReasoning, Text: "1\n2\n3"})
	if reasoning.Label != "thinking (3 lines)" {
		t.Fatalf("reasoning label=%q", reasoning.Label)
	}
	result := eventBlock(domain.Event{Kind: domain.EventToolResult, Text: "1\n2\n3"})
	if result.Label != "result (3 lines)" || result.Open {
		t.Fatalf("result block=%#v", result)
	}
}

func TestEventBlockTaskBodyFromToolDetail(t *testing.T) {
	// Intentional Japanese (multibyte) test data ("調査" = investigation;
	// "結果本文\n2行目" = result body / line 2): the plugin-normalized label and
	// body must survive rendering verbatim.
	b := eventBlock(domain.Event{Kind: domain.EventTask, ToolArg: "acbe1947 [completed]", ToolDetail: "Agent \"調査\" came to rest\n\n結果本文\n2行目"})
	joined := strings.Join(b.Body, "\n")
	if b.Style != "task" || b.Open || !strings.Contains(b.Label, "acbe1947") || !strings.Contains(b.Label, "[completed]") {
		t.Fatalf("task block=%#v", b)
	}
	for _, want := range []string{"Agent \"調査\" came to rest", "結果本文"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in body %q", want, joined)
		}
	}
}

func TestEditStatsPrototypeCountsUniqueFiles(t *testing.T) {
	tc := func(fc ...domain.FileChange) domain.Event {
		return domain.Event{Kind: domain.EventToolCall, ToolName: "Edit", Changes: fc}
	}
	events := []domain.Event{
		{Kind: domain.EventUser, Text: "do it", Prompt: "do it"},
		tc(domain.FileChange{Path: "/x/a.py", Added: 3, Removed: 2, Diff: "@@\n-old1\n-old2\n+new1\n+new2\n+new3"}),
		tc(domain.FileChange{Path: "/x/b.py", Op: "add", Added: 2, Diff: "@@\n+1\n+2"}),
		{Kind: domain.EventToolCall, ToolName: "Read", ToolArg: "/x/c.py"},
	}
	files, added, removed := editStats(events)
	if files != 2 || added != 5 || removed != 2 {
		t.Fatalf("editStats=(%d,%d,%d), want (2,5,2)", files, added, removed)
	}
}

func TestEventBlockPrototypeGeneralToolShowsKeyArg(t *testing.T) {
	b := eventBlock(domain.Event{Kind: domain.EventToolCall, ToolName: "Read", ToolArg: "/x/c.py"})
	if !strings.HasPrefix(b.Label, "Read") || !strings.Contains(b.Label, "c.py") {
		t.Fatalf("tool block=%#v", b)
	}
}
func TestDetailTurnsNewestFirst(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Date(2026, 6, 23, 1, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "old question", Prompt: "old question"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Date(2026, 6, 23, 1, 1, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "old answer"}}},
		{ID: "u2", Parent: "a1", Timestamp: time.Date(2026, 6, 23, 2, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "new question", Prompt: "new question"}}},
		{ID: "a2", Parent: "u2", Timestamp: time.Date(2026, 6, 23, 2, 1, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "new answer"}}},
	})
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/repo", Title: "title"}
	m := Model{width: 100, height: 20, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c})
	m = updated.(Model)
	if got := convlogic.TurnHeadline(*m.detail, m.detailTurns[0]); got != "new question" {
		t.Fatalf("top turn = %q", got)
	}
	out := m.detailView()
	if !strings.Contains(out, "#2") || !strings.Contains(out, "#1") || strings.Index(out, "#2") > strings.Index(out, "#1") {
		t.Fatalf("turn numbers not newest first:\n%s", out)
	}
}

func TestBranchRowsSelectableAndEnterDives(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: time.Date(2026, 6, 23, 1, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "question", Prompt: "question"}}},
		{ID: "a", Parent: "u", Timestamp: time.Date(2026, 6, 23, 3, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "active"}}},
		{ID: "b", Parent: "u", Timestamp: time.Date(2026, 6, 23, 2, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "branch question", Prompt: "branch question"}}},
		{ID: "ba", Parent: "b", Timestamp: time.Date(2026, 6, 23, 2, 1, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "branch answer"}}},
	})
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/repo", Title: "title"}
	m := Model{width: 100, height: 20, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)
	out := m.detailView()
	if !strings.Contains(out, "branch question") {
		t.Fatalf("branch lead missing:\n%s", out)
	}
	if len(m.detailRows) != 2 || m.detailRows[1].Kind != "branch" {
		t.Fatalf("detailRows=%#v", m.detailRows)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = updated.(Model)
	if m.detailCursor != 1 {
		t.Fatalf("branch row should be selectable, cursor=%d", m.detailCursor)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if len(m.detailPathStack) != 2 {
		t.Fatalf("enter on branch should dive, stack=%#v", m.detailPathStack)
	}
	if got := convlogic.TurnHeadline(*m.detail, m.detailTurns[0]); got != "branch question" {
		t.Fatalf("dove to wrong branch: %q rows=%#v", got, m.detailRows)
	}
}

func TestForkBranchLabelUsesForkKind(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: time.Date(2026, 6, 23, 1, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "question", Prompt: "question"}}},
		{ID: "a", Parent: "u", Timestamp: time.Date(2026, 6, 23, 3, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "active"}}},
		{ID: "fork", Parent: "u", Timestamp: time.Date(2026, 6, 23, 2, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "fork question", Prompt: "fork question"}}},
	})
	c.ForkRoots = []string{"fork"}
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/repo", Title: "title"}
	m := Model{width: 100, height: 20, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)
	out := m.detailView()
	if !strings.Contains(out, "fork (1turn/1msg/0branch)") || !strings.Contains(out, "fork question") {
		t.Fatalf("fork label missing:\n%s", out)
	}
}

func TestBranchLeadAvoidsNoContentForAssistantOnlyBranch(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: time.Date(2026, 6, 23, 1, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "question", Prompt: "question"}}},
		{ID: "a", Parent: "u", Timestamp: time.Date(2026, 6, 23, 3, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "active"}}},
		{ID: "b", Parent: "u", Timestamp: time.Date(2026, 6, 23, 2, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "old branch work"}}},
		{ID: "b1", Parent: "b", Timestamp: time.Date(2026, 6, 23, 2, 1, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventToolCall, ToolName: "Read"}}},
		{ID: "b2", Parent: "b1", Timestamp: time.Date(2026, 6, 23, 2, 2, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventToolResult, Text: "ok"}}},
		{ID: "b3", Parent: "b2", Timestamp: time.Date(2026, 6, 23, 2, 3, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventToolCall, ToolName: "Edit"}}},
		{ID: "b4", Parent: "b3", Timestamp: time.Date(2026, 6, 23, 2, 4, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventToolResult, Text: "ok"}}},
		{ID: "b5", Parent: "b4", Timestamp: time.Date(2026, 6, 23, 2, 5, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "done"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s", CWD: "/repo", Title: "title"}
	m := Model{width: 100, height: 20, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)
	out := m.detailView()
	if strings.Contains(out, "(no content)") || !strings.Contains(out, "old branch work") {
		t.Fatalf("bad branch label:\n%s", out)
	}
}
func TestDetailCursorScrollsAndMovesWithinViewport(t *testing.T) {
	nodes := make([]domain.ConvNode, 0, 12)
	parent := ""
	for i := 1; i <= 6; i++ {
		u := domain.ConvNode{ID: fmt.Sprintf("u%d", i), Parent: parent, Timestamp: time.Date(2026, 6, 23, i, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: fmt.Sprintf("question %d", i), Prompt: fmt.Sprintf("question %d", i)}}}
		a := domain.ConvNode{ID: fmt.Sprintf("a%d", i), Parent: u.ID, Timestamp: time.Date(2026, 6, 23, i, 1, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: fmt.Sprintf("answer %d", i)}}}
		nodes = append(nodes, u, a)
		parent = a.ID
	}
	c := domain.NewConversation(nodes)
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/repo", Title: "title"}
	m := Model{width: 100, height: 6, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c})
	m = updated.(Model)
	for i := 0; i < 4; i++ {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = updated.(Model)
	}
	if m.detailCursor != 4 || m.detailOffset != 2 {
		t.Fatalf("after down cursor=%d offset=%d, want cursor=4 offset=2", m.detailCursor, m.detailOffset)
	}
	out := m.detailView()
	if !strings.Contains(out, "#2") || strings.Contains(out, "#6") {
		t.Fatalf("viewport did not scroll down as expected:\n%s", out)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = updated.(Model)
	if m.detailCursor != 3 || m.detailOffset != 2 {
		t.Fatalf("after up cursor=%d offset=%d, want cursor=3 offset=2", m.detailCursor, m.detailOffset)
	}
	out = m.detailView()
	if !strings.Contains(out, "#3") || !strings.Contains(out, "#4") || strings.Index(out, "#3") < strings.Index(out, "#4") {
		t.Fatalf("cursor should move up within the existing viewport:\n%s", out)
	}
}
func TestConversationRefreshPreservesDetailPosition(t *testing.T) {
	nodes := make([]domain.ConvNode, 0, 12)
	parent := ""
	for i := 1; i <= 6; i++ {
		u := domain.ConvNode{ID: fmt.Sprintf("u%d", i), Parent: parent, Timestamp: time.Date(2026, 6, 23, i, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: fmt.Sprintf("question %d", i), Prompt: fmt.Sprintf("question %d", i)}}}
		a := domain.ConvNode{ID: fmt.Sprintf("a%d", i), Parent: u.ID, Timestamp: time.Date(2026, 6, 23, i, 1, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: fmt.Sprintf("answer %d", i)}}}
		nodes = append(nodes, u, a)
		parent = a.ID
	}
	c := domain.NewConversation(nodes)
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/repo", Title: "title"}
	m := Model{width: 100, height: 6, detail: &c, detailSession: &s, detailCursor: 4, detailOffset: 2}
	updated, _ := m.Update(convMsg{c: &c})
	m = updated.(Model)
	if m.detailCursor != 4 || m.detailOffset != 2 {
		t.Fatalf("refresh should preserve position, cursor=%d offset=%d", m.detailCursor, m.detailOffset)
	}
	updated, _ = m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)
	if m.detailCursor != 0 || m.detailOffset != 0 {
		t.Fatalf("explicit open should reset position, cursor=%d offset=%d", m.detailCursor, m.detailOffset)
	}
}

// If a convMsg that arrives late after the detail view is closed (detailSession==nil) were adopted,
// only detail would become non-nil, causing a panic at *detailSession in View->detailView. Once closed, ignore it.
func TestStaleConvMsgAfterDetailClosedIgnored(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Events: []domain.Event{{Kind: domain.EventUser, Text: "q", Prompt: "q"}}},
		{ID: "a", Parent: "u", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "a"}}},
	})
	// A conversation-load result arrives while detailSession==nil (already closed).
	m := Model{width: 80, height: 10}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)
	if m.detail != nil {
		t.Fatal("stale conversation must not be adopted after detail closed")
	}
	// View returns the list without panicking.
	_ = m.View()
}
func TestEnterOpensTurnFullViewAndQReturns(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Events: []domain.Event{{Kind: domain.EventUser, Text: "question", Prompt: "question"}}},
		{ID: "a", Parent: "u", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
	})
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/repo", Title: "title"}
	m := Model{width: 80, height: 10, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(Model)
	if !m.turnOpen || !strings.Contains(m.detailView(), "turn #1/1") {
		t.Fatalf("enter should open turn full view: open=%v\n%s", m.turnOpen, m.detailView())
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = updated.(Model)
	if m.turnOpen {
		t.Fatal("q should return from turn full view to turn list")
	}
}
func TestFileChangeAppearsInTurnListAndFullView(t *testing.T) {
	change := domain.FileChange{Path: "internal/tui/tui.go", Added: 2, Removed: 1}
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Events: []domain.Event{{Kind: domain.EventUser, Text: "edit", Prompt: "edit"}}},
		{ID: "fc", Parent: "u", Events: []domain.Event{{Kind: domain.EventFileChange, Changes: []domain.FileChange{change}}}},
	})
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/repo", Title: "title"}
	m := Model{width: 120, height: 10, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)
	out := m.detailView()
	for _, want := range []string{"*1", "+2", "-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in turn list:\n%s", want, out)
		}
	}
	m.openCurrentTurn(true)
	// stripANSI: the counts render as separately colored segments, so the plain
	// text is only contiguous once the escape codes are removed.
	out = stripANSI(m.detailView())
	// The file_change is now surfaced in the consolidated "Edited files" section.
	// It carries no diff body (aggregate-only), but the single-file counts are shown.
	for _, want := range []string{"Edited files", "M internal/tui/tui.go", "+2 -1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in full view:\n%s", want, out)
		}
	}
	// The "*** ... File:" header would repeat the label's op/path; it must not render.
	if strings.Contains(out, "*** Update File") {
		t.Fatalf("redundant patch header in full view:\n%s", out)
	}
	// The file row is colored by its op (update → diff-mod), with the counts
	// as separate green/red segments — parentheses stay plain — whose
	// concatenation is the plain label.
	if len(m.turnBlocks) < 2 || m.turnBlocks[1].Style != "diff-mod" {
		t.Fatalf("file row style = %+v", m.turnBlocks)
	}
	fb := m.turnBlocks[1]
	joined := ""
	for _, sp := range fb.LabelSpans {
		joined += sp.text
	}
	if joined != fb.Label {
		t.Fatalf("span concat %q != label %q", joined, fb.Label)
	}
	if len(fb.LabelSpans) != 5 || fb.LabelSpans[1].style != "plain" || fb.LabelSpans[2].style != "add" ||
		fb.LabelSpans[3].style != "del" || fb.LabelSpans[4].style != "plain" {
		t.Fatalf("label spans = %+v", fb.LabelSpans)
	}
}

// The full turn view header shows the session's status mark like the detail header.
func TestTurnFullViewHeaderShowsStatusMark(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Events: []domain.Event{{Kind: domain.EventUser, Text: "q", Prompt: "q"}}},
	})
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/repo", Title: "t", Status: domain.StatusRunning}
	m := Model{width: 120, height: 12, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)
	m.openCurrentTurn(true)
	out := stripANSI(m.detailView())
	if !strings.Contains(strings.Split(out, "\n")[0], "● RUN") {
		t.Fatalf("missing status mark in full view header:\n%s", out)
	}
}

// Each block header line in the full turn view carries its event's time as an
// HH:MM:SS gutter.
func TestTurnFullViewShowsEventTimeGutter(t *testing.T) {
	ts := time.Date(2026, 6, 23, 14, 3, 22, 0, time.Local)
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: ts, Events: []domain.Event{{Kind: domain.EventUser, Text: "q", Prompt: "q", Timestamp: ts}}},
		{ID: "a", Parent: "u", Timestamp: ts.Add(3 * time.Second), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "ans", Timestamp: ts.Add(3 * time.Second)}}},
	})
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/repo", Title: "t"}
	m := Model{width: 120, height: 12, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)
	m.openCurrentTurn(true)
	out := stripANSI(m.detailView())
	for _, want := range []string{"14:03:22 ", "14:03:25 "} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing time gutter %q in full view:\n%s", want, out)
		}
	}
}

func TestTurnListColumnsAlignAndFooterStaysBottom(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Date(2026, 6, 23, 1, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "short", Prompt: "short"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Date(2026, 6, 23, 1, 0, 2, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
		{ID: "u2", Parent: "a1", Timestamp: time.Date(2026, 6, 23, 2, 0, 0, 0, time.Local), Events: []domain.Event{{Kind: domain.EventUser, Text: "longer headline", Prompt: "longer headline"}}},
		{ID: "t2", Parent: "u2", Timestamp: time.Date(2026, 6, 23, 2, 0, 1, 0, time.Local), Events: []domain.Event{{Kind: domain.EventToolCall, ToolName: "Bash", Text: "ls"}}},
		{ID: "a2", Parent: "t2", Timestamp: time.Date(2026, 6, 23, 2, 1, 10, 0, time.Local), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
	})
	s := domain.Session{PluginID: "codex", AgentType: "codex", SessionID: "s", CWD: "/repo", Title: "title"}
	m := Model{width: 120, height: 12, detailSession: &s}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	m = updated.(Model)
	out := stripANSI(m.detailView())
	lines := strings.Split(out, "\n")
	if len(lines) != 12 {
		t.Fatalf("detail view should fill terminal height, got %d lines:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[len(lines)-1], "o resume") {
		t.Fatalf("footer should be last line, got %q", lines[len(lines)-1])
	}
	var shortLine, longLine string
	for _, line := range lines {
		if strings.Contains(line, "short") {
			shortLine = line
		}
		if strings.Contains(line, "longer headline") {
			longLine = line
		}
	}
	if shortLine == "" || longLine == "" {
		t.Fatalf("missing headlines:\n%s", out)
	}
	col1 := lipgloss.Width(shortLine[:strings.Index(shortLine, "short")])
	col2 := lipgloss.Width(longLine[:strings.Index(longLine, "longer headline")])
	if col1 != col2 {
		t.Fatalf("headline columns differ: short=%d longer=%d\n%s", col1, col2, out)
	}
	m.openCurrentTurn(true)
	out = stripANSI(m.detailView())
	lines = strings.Split(out, "\n")
	if len(lines) != 12 || !strings.Contains(lines[len(lines)-1], "Tab/[ ] block") {
		t.Fatalf("turn full footer should be bottom, lines=%d last=%q\n%s", len(lines), lines[len(lines)-1], out)
	}
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

// A tool result renders the plugin-normalized ToolDetail (already cleaned of
// agent-specific metadata) when present, and its line count drives the label.
func TestToolResultUsesToolDetail(t *testing.T) {
	b := eventBlock(domain.Event{Kind: domain.EventToolResult, Text: "Chunk ID: abc\nraw", ToolDetail: "real output\njson output"})
	if strings.Join(b.Body, "\n") != "real output\njson output" || b.Label != "result (2 lines)" {
		t.Fatalf("result block=%#v", b)
	}
}
