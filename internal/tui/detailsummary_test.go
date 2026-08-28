package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/core/domain"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

const summaryTestTitle = "このディレクトリ配下のgitを一括でpullするスクリプトつくって。実行権限も付けておいて。"

// twoTurns is the conversation the tests in this file open; what it holds does
// not matter beyond giving the turn list rows of its own and a title.
func twoTurns() domain.Conversation {
	ts := func(s int) time.Time { return time.Date(2026, 8, 28, 1, 0, s, 0, time.Local) }
	return domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: summaryTestTitle, Prompt: summaryTestTitle}}},
		{ID: "a1", Parent: "u1", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
		{ID: "u2", Parent: "a1", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventUser, Text: "again", Prompt: "again"}}},
		{ID: "a2", Parent: "u2", Timestamp: ts(4), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answered"}}},
	})
}

// detailWithSummary loads the conversation into a detail view whose cache holds
// the given session summary, written from fingerprint writtenFP while the
// session is at sessionFP (equal fingerprints mean the summary is current).
func detailWithSummary(t *testing.T, text, writtenFP, sessionFP string, width, height int) Model {
	t.Helper()
	db, e := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if e != nil {
		t.Fatalf("open cache: %v", e)
	}
	t.Cleanup(func() { db.Close() })
	written := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s", CWD: "/repo", Title: summaryTestTitle, Fingerprint: writtenFP}
	if e := db.PutSummaries(context.Background(), written, []cache.Summary{{Turn: 0, Text: text, Model: "claude claude-sonnet-5"}}); e != nil {
		t.Fatalf("put summaries: %v", e)
	}
	s := written
	s.Fingerprint = sessionFP
	m := Model{width: width, height: height, detailSession: &s, cache: db}
	c := twoTurns()
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	return updated.(Model)
}

func pressKey(m Model, key string) Model {
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	return updated.(Model)
}

func pressEnter(m Model) Model {
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return updated.(Model)
}

// headRows returns the rows the head currently occupies.
func headRows(m Model) int {
	n := 0
	for _, r := range m.detailRows {
		if r.Kind == "head" || r.Kind == "headtext" {
			n++
		}
	}
	return n
}

// The session's own summary is the list's first row, in the place the title
// held: the title is the session's first prompt, which turn #1 carries anyway,
// and the § marks the line as generated rather than as something that was said.
func TestDetailHeadRowShowsSummaryInPlaceOfTitle(t *testing.T) {
	m := detailWithSummary(t, "pull-all.shを作成し6リポジトリのpullを確認", "fp", "fp", 120, 20)

	if got := m.detailRows[0].Kind; got != "head" {
		t.Fatalf("the head row should lead the list, got kind %q", got)
	}
	if headRows(m) != 1 {
		t.Fatalf("closed, the head is one row, got %d", headRows(m))
	}
	out := stripANSI(m.detailView())
	lines := strings.Split(out, "\n")
	if len(lines) != 20 {
		t.Fatalf("detail view should fill terminal height, got %d lines:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[2]), "§ ") {
		t.Fatalf("the head row should carry the summary behind a § mark, got %q\n%s", lines[2], out)
	}
	if !strings.Contains(lines[2], "pull-all.sh") {
		t.Fatalf("head row missing the summary, got %q", lines[2])
	}
	if strings.Contains(lines[2], "このディレクトリ配下") {
		t.Fatalf("the title should give up the line to the summary, got %q", lines[2])
	}
	if !strings.Contains(lines[len(lines)-1], "s summary") {
		t.Fatalf("footer should offer the toggle, got %q", lines[len(lines)-1])
	}
}

// Opening puts the title back in full and the whole summary under it, as rows of
// the list — so a summary longer than the screen scrolls instead of being cut.
func TestDetailHeadRowOpensToFullTitleAndSummary(t *testing.T) {
	long := strings.Repeat("長い要約の文章。", 60)
	m := detailWithSummary(t, long, "fp", "fp", 60, 12)

	m = pressKey(m, "s")
	if headRows(m) < 8 {
		t.Fatalf("the whole summary should become rows (got %d) rather than being clipped to the screen", headRows(m))
	}
	if got := len(strings.Split(stripANSI(m.detailView()), "\n")); got != 12 {
		t.Fatalf("the view still fills exactly the terminal, got %d lines", got)
	}
	// Every line of the summary is reachable: the rows below the fold are the
	// ones the cursor scrolls to.
	m.detailCursor = len(m.detailRows) - 1
	(&m).ensureDetailOffset()
	if out := stripANSI(m.detailView()); !strings.Contains(out, "claude claude-sonnet-5") {
		t.Fatalf("scrolling to the end should reach the credit line:\n%s", out)
	}
	if today := time.Now().Local().Format("2006-01-02"); !strings.Contains(stripANSI(m.detailView()), today) {
		t.Fatalf("the open summary should say when it was written (%s)", today)
	}
}

// The title is what the head row gives up when there is a summary, so opening
// has to bring it back in full — that is where it can still be read.
func TestDetailHeadRowOpenShowsWholeTitle(t *testing.T) {
	m := detailWithSummary(t, "要約", "fp", "fp", 40, 24)
	m = pressKey(m, "s")

	// Joined without the leading indent of each line, so a title split across
	// lines reads as the one string it is.
	head := ""
	for _, ln := range strings.Split(stripANSI(m.detailView()), "\n")[2 : 2+headRows(m)] {
		head += strings.TrimSpace(ln)
	}
	for _, part := range []string{"このディレクトリ配下", "実行権限も付けておいて"} {
		if !strings.Contains(head, part) {
			t.Fatalf("the wrapped title should be shown in full, %q missing from:\n%s", part, head)
		}
	}
}

// Enter on the head does what `s` does, including on the rows the summary added:
// a row that grew out of the head folds back the way it came.
func TestDetailHeadRowEnterToggles(t *testing.T) {
	m := detailWithSummary(t, "セッション全体の要約", "fp", "fp", 80, 20)
	closed := len(m.detailRows)

	m.detailCursor = 0 // the head row; opening a session lands on a turn instead
	m = pressEnter(m)
	if len(m.detailRows) <= closed {
		t.Fatalf("Enter on the head row should open it: %d → %d rows", closed, len(m.detailRows))
	}
	if m.turnOpen {
		t.Fatalf("Enter on the head row must not open a turn")
	}
	m.detailCursor = headRows(m) - 1 // the last line the summary added
	m = pressEnter(m)
	if len(m.detailRows) != closed {
		t.Fatalf("Enter on a head line should fold it back, got %d rows, want %d", len(m.detailRows), closed)
	}
	if m.detailCursor != 0 {
		t.Fatalf("folding should leave the cursor on the line that remains, got row %d", m.detailCursor)
	}
}

// Opening and closing moves the rows below, so the cursor moves with them and
// stays on the turn it was on.
func TestDetailHeadRowKeepsCursorOnItsTurn(t *testing.T) {
	m := detailWithSummary(t, strings.Repeat("要約の文。", 30), "fp", "fp", 80, 24)
	m.detailCursor = len(m.detailRows) - 1 // turn #1, the last row
	turn := m.detailRows[m.detailCursor].Turn

	m = pressKey(m, "s")
	if got := m.detailRows[m.detailCursor].Turn; len(got) == 0 || got[0] != turn[0] {
		t.Fatalf("opening the head moved the cursor off its turn (row %d of %d)", m.detailCursor, len(m.detailRows))
	}
	m = pressKey(m, "s")
	if got := m.detailRows[m.detailCursor].Turn; len(got) == 0 || got[0] != turn[0] {
		t.Fatalf("closing the head moved the cursor off its turn (row %d of %d)", m.detailCursor, len(m.detailRows))
	}
}

// A summary written before the session went on describes something the turns no
// longer are — the same call `show` makes. It is said in front of the summary,
// which is the part of the line a narrow terminal keeps.
func TestDetailSummaryMarksStale(t *testing.T) {
	m := detailWithSummary(t, "古い要約", "old-fp", "new-fp", 120, 20)

	line := strings.Split(stripANSI(m.detailView()), "\n")[2]
	if !strings.Contains(line, "(stale)") {
		t.Fatalf("a summary written from an older fingerprint should be marked stale, got %q", line)
	}
	if i, j := strings.Index(line, "(stale)"), strings.Index(line, "古い要約"); i > j {
		t.Fatalf("the stale mark should come before the summary text, got %q", line)
	}
}

// MarkExamined writes a blank turn-0 row for a session it found nothing to ask
// about. That is a record of having looked, not a summary: the title keeps the
// row, and neither `s` nor the footer pretends there is anything to open.
func TestDetailSummaryIgnoresBlankRow(t *testing.T) {
	m := detailWithSummary(t, "  ", "fp", "fp", 120, 20)

	out := stripANSI(m.detailView())
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[2], "このディレクトリ配下") {
		t.Fatalf("with no summary the head row is the title, got %q", lines[2])
	}
	if strings.Contains(lines[2], "§") {
		t.Fatalf("a blank stored row is not a summary: %q", lines[2])
	}
	if strings.Contains(lines[len(lines)-1], "s summary") {
		t.Fatalf("footer should not offer the toggle with nothing to show, got %q", lines[len(lines)-1])
	}
	rows := len(m.detailRows)
	if m = pressKey(m, "s"); len(m.detailRows) != rows {
		t.Fatalf("`s` should be inert with no summary: %d → %d rows", rows, len(m.detailRows))
	}
}

// A search inside the session matches the summary too: the session list finds
// sessions by their summaries (cache.SummaryTexts), and one opened that way has
// to find it here as well.
func TestDetailSearchMatchesSummary(t *testing.T) {
	m := detailWithSummary(t, "pull-all.shを作成", "fp", "fp", 120, 20)
	m.turnQuery = "pull-all"
	m.turnHitsStale = true
	(&m).syncTurnHits()

	hits := m.turnListHits()
	if len(hits) == 0 || hits[0] != 0 {
		t.Fatalf("the head row should match a query only its summary carries, got hits %v", hits)
	}
}

// The head row is a row of the list, so the cursor lands on it and it is
// highlighted like any other — summary or title, whichever it is holding.
func TestDetailHeadRowHighlightedWhenSelected(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := detailWithSummary(t, "セッション全体の要約", "fp", "fp", 80, 20)
	m.detailCursor = 0

	line := strings.Split(m.detailView(), "\n")[2]
	if !strings.Contains(line, "48;5;238") {
		t.Fatalf("the selected head row should carry the selection background, got %q", line)
	}
}

// Every line the summary adds is a row of its own, so each of them highlights
// when the cursor reaches it — not just the first.
func TestDetailHeadTextRowsHighlightWhenSelected(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := detailWithSummary(t, strings.Repeat("要約の文。", 20), "fp", "fp", 80, 24)
	m = pressKey(m, "s")
	if headRows(m) < 3 {
		t.Fatalf("expected the summary to span several rows, got %d", headRows(m))
	}
	for row := 0; row < headRows(m); row++ {
		m.detailCursor = row
		line := strings.Split(m.detailView(), "\n")[2+row]
		if !strings.Contains(line, "48;5;238") {
			t.Fatalf("head row %d should highlight when selected, got %q", row, line)
		}
	}
}

// A resize rewraps an open summary into a different number of rows. The cursor
// moves with them, so the turn it was on is still the turn it is on — including
// while a turn is open, where the cursor is what that view reads.
func TestDetailHeadRowsReflowOnResize(t *testing.T) {
	m := detailWithSummary(t, strings.Repeat("要約の文。", 20), "fp", "fp", 100, 24)
	m = pressKey(m, "s")
	wide := headRows(m)
	m.detailCursor = len(m.detailRows) - 1
	turn := m.detailRows[m.detailCursor].Turn

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 50, Height: 24})
	m = updated.(Model)
	if headRows(m) <= wide {
		t.Fatalf("a narrower terminal should wrap the summary into more rows: %d → %d", wide, headRows(m))
	}
	if got := m.detailRows[m.detailCursor].Turn; len(got) == 0 || got[0] != turn[0] {
		t.Fatalf("the resize moved the cursor off its turn (row %d of %d)", m.detailCursor, len(m.detailRows))
	}
}

// The head row is a row of the list now, so it can be the cursor's row — and the
// breadcrumb that says which level was descended into has to survive that.
func TestDetailHeadRowKeepsBreadcrumbWhenSelected(t *testing.T) {
	ts := func(s int64) time.Time { return time.Unix(s, 0) }
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Timestamp: ts(1), Events: []domain.Event{{Kind: domain.EventUser, Text: "question", Prompt: "question"}}},
		{ID: "a", Parent: "u", Timestamp: ts(3), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "main-line continuation"}}},
		{ID: "b", Parent: "u", Timestamp: ts(2), Events: []domain.Event{{Kind: domain.EventUser, Text: "rewound continuation", Prompt: "rewound continuation"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "sessabcd99", CWD: "/repo", Title: "t"}
	m := Model{width: 140, height: 20, detailSession: &s}
	upd, _ := m.Update(convMsg{c: &c, focusLeaf: "b", reset: true})
	m = upd.(Model)
	if len(m.detailPathStack) != 2 {
		t.Fatalf("did not descend into the rewound branch: stack depth=%d", len(m.detailPathStack))
	}
	if out := stripANSI(m.detailView()); !strings.Contains(out, "▸") {
		t.Fatalf("no breadcrumb to begin with:\n%s", out)
	}

	m.detailCursor = 0 // the head row
	if out := stripANSI(m.detailView()); !strings.Contains(out, "▸") {
		t.Fatalf("the breadcrumb disappears when the head row is the cursor's row:\n%s", out)
	}
}

// The fork lineage belongs to the head row's first line whether that line is
// holding the title or the summary in its place.
func TestDetailHeadRowKeepsForkLineageWithSummary(t *testing.T) {
	m := detailWithSummary(t, "セッション全体の要約", "fp", "fp", 140, 20)
	s := *m.detailSession
	s.ParentSessionID = "parent01"
	m.detailSession = &s

	line := stripANSI(m.detailHeadLines(&s, 0)[0])
	if !strings.Contains(line, "§") {
		t.Fatalf("the head row should still be the summary, got %q", line)
	}
	if !strings.Contains(line, "forked from: parent01") {
		t.Fatalf("the fork lineage should stay on the head row, got %q", line)
	}
}

// One summary is one hit, however many lines it is wrapped into: the footer's
// count and n/N would otherwise report the wrapping as matches of its own.
func TestDetailSearchCountsOpenSummaryOnce(t *testing.T) {
	m := detailWithSummary(t, "pull-all.shを作成。git pull --ff-onlyのみを使う方針で実装し、6リポジトリ全てを確認。", "fp", "fp", 60, 24)
	m = pressKey(m, "s")
	if headRows(m) < 3 {
		t.Fatalf("expected the summary to wrap into several rows, got %d", headRows(m))
	}
	m.turnQuery = "pull-all"
	m.turnHitsStale = true
	(&m).syncTurnHits()

	if hits := m.turnListHits(); len(hits) != 1 || hits[0] != 0 {
		t.Fatalf("an open summary should be one hit on the head row, got %v", hits)
	}
}

// The worker rewrites summaries in the background, so a reload can change how
// many rows the head takes under an open view. The cursor moves with the rows.
func TestDetailHeadRowsFollowASummaryRewrittenWhileOpen(t *testing.T) {
	m := detailWithSummary(t, "短い要約", "fp", "fp", 60, 24)
	m = pressKey(m, "s")
	m.detailCursor = len(m.detailRows) - 1 // turn #1
	turn := m.detailRows[m.detailCursor].Turn

	if e := m.cache.PutSummaries(context.Background(), *m.detailSession,
		[]cache.Summary{{Turn: 0, Text: strings.Repeat("長くなった要約。", 20), Model: "claude claude-sonnet-5"}}); e != nil {
		t.Fatalf("put summaries: %v", e)
	}
	c := twoTurns()
	updated, _ := m.Update(convMsg{c: &c}) // the reload a scan sends, not an open
	m = updated.(Model)

	if got := m.detailRows[m.detailCursor].Turn; len(got) == 0 || got[0] != turn[0] {
		t.Fatalf("a summary rewritten under the open view moved the cursor off its turn (row %d of %d)", m.detailCursor, len(m.detailRows))
	}
}
