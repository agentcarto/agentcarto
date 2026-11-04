package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
)

// A scan parses everything and the hook after it opens conversations, which can take
// many seconds, and the worker moves on regardless. A status that waited for them
// would go on saying which session is in hand after the worker had left it.
func TestATickReadsTheSummaryStatusWhileAScanIsRunning(t *testing.T) {
	want := summary.Status{Enabled: true, Queued: 12}
	m := Model{view: "time", app: &app.App{}, scanning: true, readSummaryStatus: func() summary.Status { return want }}

	_, cmd := m.Update(tickMsg(time.Now()))

	if cmd == nil {
		t.Fatal("the tick returned no command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("the tick returned %T", cmd())
	}
	for _, c := range batch {
		if got, ok := c().(summaryStatusMsg); ok {
			if got.Queued != 12 {
				t.Errorf("status=%+v", got)
			}
			return
		}
	}
	t.Error("nothing the tick started reads the status")
}

// A host with nothing to report leaves the zero status alone, tick after tick.
func TestATickWithNothingToReadStillTicks(t *testing.T) {
	m := Model{view: "time", app: &app.App{}, scanning: true}
	_, cmd := m.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("the tick stopped")
	}
}

// listWithStatus is a list of the given sessions with a summary status reported.
func listWithStatus(st summary.Status, width int, sessions ...domain.Session) Model {
	m := Model{view: "time", width: width, height: 10, sessions: sessions, summaryStatus: st}
	m.filter()
	return m
}

func viewLines(m Model) []string { return strings.Split(ansi.Strip(m.View()), "\n") }

func sessionKey(id string) domain.SessionKey {
	return domain.SessionKey{PluginID: "claude", SessionID: id}
}

func claudeSession(id string) domain.Session {
	return domain.Session{PluginID: "claude", AgentType: "claude", SessionID: id, Title: "title " + id}
}

func TestListHeaderSaysWhereSummarizingStands(t *testing.T) {
	cases := []struct {
		name string
		st   summary.Status
		want string
	}{
		{"nothing to do", summary.Status{Enabled: true}, "§ idle"},
		{"a worker with nothing left behind it", summary.Status{Enabled: true, Running: true}, "§ running"},
		{"a worker and a queue", summary.Status{Enabled: true, Running: true, Queued: 12}, "§ running · 12 queued"},
		{"everything at once", summary.Status{Enabled: true, Running: true, Queued: 12, Failed: 2}, "§ running · 12 queued · 2 failed"},
		{"waiting for a worker to start", summary.Status{Enabled: true, Queued: 12}, "§ 12 queued"},
		{"only failures left", summary.Status{Enabled: true, Failed: 2}, "§ 2 failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			head := viewLines(listWithStatus(c.st, 120, claudeSession("a")))[0]
			if !strings.HasSuffix(head, c.want) {
				t.Errorf("header=%q want it to end in %q", head, c.want)
			}
		})
	}
}

// It is the default, so anything here would be on the screen of everyone who never
// asked for summaries.
func TestListSaysNothingWithSummarizingOff(t *testing.T) {
	lines := viewLines(listWithStatus(summary.Status{}, 120, claudeSession("a")))
	if strings.Contains(strings.Join(lines[:2], "\n"), summaryMark) {
		t.Errorf("the top of the list mentions summaries:\n%s", strings.Join(lines[:2], "\n"))
	}
	if strings.TrimSpace(lines[1]) != "" {
		t.Errorf("the reserved line is not blank: %q", lines[1])
	}
}

// "scanning..." comes and goes every few seconds. A status laid out after it would
// jump sideways just as often.
func TestListHeaderStatusStaysPutWhileScanning(t *testing.T) {
	st := summary.Status{Enabled: true, Queued: 3}
	idle := listWithStatus(st, 120, claudeSession("a"))
	scanning := idle
	scanning.scanning = true
	a, b := viewLines(idle)[0], viewLines(scanning)[0]
	if !strings.Contains(b, "scanning...") {
		t.Fatalf("the scan is not on the line: %q", b)
	}
	if strings.Index(a, summaryMark) != strings.Index(b, summaryMark) {
		t.Errorf("the status moved:\n%q\n%q", a, b)
	}
}

// What the header always said matters more than what was added to it.
func TestListHeaderKeepsItsLeftSideOnANarrowTerminal(t *testing.T) {
	st := summary.Status{Enabled: true, Running: true, Queued: 12, Failed: 2}
	head := viewLines(listWithStatus(st, 70, claudeSession("a")))[0]
	if !strings.Contains(head, "active: 0") {
		t.Errorf("header=%q lost what it always said", head)
	}
	if w := runewidth.StringWidth(head); w > 69 {
		t.Errorf("header is %d wide on a 70-column terminal: %q", w, head)
	}
	if !strings.Contains(head, summaryMark) {
		t.Errorf("header=%q dropped the status with room left for part of it", head)
	}
}

// With too little room what goes first is what can wait: a count of what is failing
// is the reason to look at this at all.
func TestListHeaderKeepsTheFailuresOnANarrowTerminal(t *testing.T) {
	st := summary.Status{Enabled: true, Running: true, Queued: 12, Failed: 2}
	m := listWithStatus(st, 80, claudeSession("a"))
	if head := viewLines(m)[0]; !strings.HasSuffix(head, "2 failed") {
		t.Errorf("header=%q", head)
	}
	m.scanning = true
	if head := viewLines(m)[0]; !strings.HasSuffix(head, "§ 2 failed") {
		t.Errorf("scanning: header=%q", head)
	}
}

func TestListStatusLineSpeaksForTheSelectedSession(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		ss   summary.SessionStatus
		want string
	}{
		{"queued", summary.SessionStatus{State: summary.SessionQueued}, "§ queued"},
		{"on a call", summary.SessionStatus{State: summary.SessionWorking, Call: 2, Calls: 3, CallStarted: now.Add(-4 * time.Minute)}, "§ summarizing now, call 2 of 3 (4m0"},
		{"on the session's own summary", summary.SessionStatus{State: summary.SessionWorking, CallStarted: now.Add(-30 * time.Second)}, "§ summarizing now, the session's own summary (3"},
		{"waiting out a failure", summary.SessionStatus{State: summary.SessionFailed, Attempts: 2, RetryAt: now.Add(3*time.Hour + 12*time.Minute + 30*time.Second), LastError: "claude: limit"}, "§ failed 2×, retry in 3h12m: claude: limit"},
		{"backoff just over", summary.SessionStatus{State: summary.SessionFailed, Attempts: 2, RetryAt: now.Add(-time.Second), LastError: "claude: limit"}, "§ failed 2×, retrying soon: claude: limit"},
		{"out of attempts", summary.SessionStatus{State: summary.SessionFailed, Attempts: 3, LastError: "claude: limit"}, "§ failed 3×: claude: limit"},
		{"failed before reasons were kept", summary.SessionStatus{State: summary.SessionFailed, Attempts: 1, RetryAt: now.Add(time.Hour + time.Minute)}, "§ failed 1×, retry in 1h"},
		{"retrying", summary.SessionStatus{State: summary.SessionQueued, Attempts: 1, LastError: "claude: limit"}, "§ queued to retry, failed 1×: claude: limit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := summary.Status{Enabled: true, Sessions: map[domain.SessionKey]summary.SessionStatus{sessionKey("a"): c.ss}}
			line := viewLines(listWithStatus(st, 120, claudeSession("a")))[1]
			if !strings.Contains(line, c.want) {
				t.Errorf("line=%q want %q", line, c.want)
			}
		})
	}
}

// A status line that ended in ": " would read as a reason cut off.
func TestAFailureWithNoReasonDoesNotTrailOff(t *testing.T) {
	line := sessionSummaryLine(summary.SessionStatus{State: summary.SessionFailed, Attempts: 3}, time.Now())
	if line != "§ failed 3×" {
		t.Errorf("line=%q", line)
	}
}

func TestListStatusLineFollowsTheCursor(t *testing.T) {
	st := summary.Status{Enabled: true, Sessions: map[domain.SessionKey]summary.SessionStatus{sessionKey("b"): {State: summary.SessionQueued}}}
	m := listWithStatus(st, 120, claudeSession("a"), claudeSession("b"))
	if line := viewLines(m)[1]; strings.TrimSpace(line) != "" {
		t.Errorf("a session with nothing happening: %q", line)
	}
	m.cursor = 1
	if line := viewLines(m)[1]; !strings.Contains(line, "§ queued") {
		t.Errorf("the session with a request: %q", line)
	}
}

// The line is shared with the search box, and the query is what someone typing is
// looking at.
func TestSearchTakesTheStatusLine(t *testing.T) {
	st := summary.Status{Enabled: true, Sessions: map[domain.SessionKey]summary.SessionStatus{sessionKey("a"): {State: summary.SessionQueued}}}
	m := listWithStatus(st, 120, claudeSession("a"))
	m.searching = true
	if line := viewLines(m)[1]; strings.Contains(line, "queued") {
		t.Errorf("line=%q", line)
	}
}

// Moving the status line must not move the list: it takes the row that was already
// reserved for it, whatever it has to say.
func TestTheStatusLineDoesNotMoveTheList(t *testing.T) {
	quiet := viewLines(listWithStatus(summary.Status{}, 120, claudeSession("a")))
	st := summary.Status{Enabled: true, Sessions: map[domain.SessionKey]summary.SessionStatus{sessionKey("a"): {State: summary.SessionQueued}}}
	busy := viewLines(listWithStatus(st, 120, claudeSession("a")))
	if len(quiet) != len(busy) {
		t.Errorf("%d lines became %d", len(quiet), len(busy))
	}
}

func TestListRowsMarkWhatAReaderGoesLookingFor(t *testing.T) {
	st := summary.Status{Enabled: true, Sessions: map[domain.SessionKey]summary.SessionStatus{
		sessionKey("failed"):  {State: summary.SessionFailed, Attempts: 1},
		sessionKey("working"): {State: summary.SessionWorking},
		sessionKey("queued"):  {State: summary.SessionQueued},
	}}
	m := listWithStatus(st, 120)
	for _, c := range []struct{ id, want string }{
		{"failed", "! title failed"},
		{"working", "⟳ title working"},
		{"queued", " title queued"},
		{"quiet", " title quiet"},
	} {
		row := ansi.Strip(m.sessionRow(claudeSession(c.id), false, 0, ""))
		if !strings.Contains(row, c.want) {
			t.Errorf("row of %s=%q want %q", c.id, row, c.want)
		}
		if (c.id == "queued" || c.id == "quiet") && (strings.Contains(row, "!") || strings.Contains(row, "⟳")) {
			t.Errorf("row of %s carries a mark: %q", c.id, row)
		}
	}
}

// detailWithStatus opens a two-turn session with the first turn described.
func detailWithStatus(t *testing.T, st summary.Status, width int) Model {
	t.Helper()
	db, e := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if e != nil {
		t.Fatalf("open cache: %v", e)
	}
	t.Cleanup(func() { db.Close() })
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "a", CWD: "/repo", Title: summaryTestTitle, Fingerprint: "fp"}
	c := twoTurns()
	nodes := summary.NodesByTurn(transcript.Turns(c, c.ActivePath()))
	if e := db.PutSummaries(context.Background(), s, []cache.Summary{{Turn: 1, NodeID: nodes[1], Text: "ターン1の要約", Model: "m"}}); e != nil {
		t.Fatalf("put summaries: %v", e)
	}
	m := Model{width: width, height: 20, detailSession: &s, cache: db, summaryStatus: st}
	updated, _ := m.Update(convMsg{c: &c, reset: true})
	return updated.(Model)
}

func TestDetailHeaderSaysHowFarTheSummariesHaveGot(t *testing.T) {
	cases := []struct {
		name string
		ss   summary.SessionStatus
		want string
	}{
		{"nothing happening", summary.SessionStatus{}, "turns:2  §1/2"},
		{"queued", summary.SessionStatus{State: summary.SessionQueued}, "turns:2  §1/2  queued"},
		{"on a call", summary.SessionStatus{State: summary.SessionWorking, Call: 2, Calls: 3}, "turns:2  §1/2  summarizing 2/3"},
		{"on the session's own summary", summary.SessionStatus{State: summary.SessionWorking}, "turns:2  §1/2  summarizing"},
		{"failed", summary.SessionStatus{State: summary.SessionFailed, Attempts: 2, LastError: "the reason"}, "turns:2  §1/2  failed 2×"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := summary.Status{Enabled: true, Sessions: map[domain.SessionKey]summary.SessionStatus{sessionKey("a"): c.ss}}
			lead := viewLines(detailWithStatus(t, st, 160))[0]
			if !strings.HasSuffix(strings.TrimRight(lead, " "), c.want) {
				t.Errorf("lead=%q want it to end in %q", lead, c.want)
			}
			if strings.Contains(lead, "the reason") {
				t.Errorf("lead=%q has no room for a reason", lead)
			}
		})
	}
}

func TestDetailHeaderSaysNothingWithSummarizingOff(t *testing.T) {
	lead := viewLines(detailWithStatus(t, summary.Status{}, 160))[0]
	if strings.Contains(lead, summaryMark) {
		t.Errorf("lead=%q", lead)
	}
}

// Color is what the width of a line must not be measured in: cut by the bytes of
// its escape sequences, a line ends well before the edge of the terminal, and what
// was appended last is what goes.
func TestDetailHeaderIsCutByWhatIsVisible(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	st := summary.Status{Enabled: true, Sessions: map[domain.SessionKey]summary.SessionStatus{sessionKey("a"): {State: summary.SessionWorking, Call: 2, Calls: 3}}}
	whole := ansi.Strip(strings.Split(detailWithStatus(t, st, 200).View(), "\n")[0])
	// Exactly wide enough: the line's last column is the terminal's last usable one.
	width := runewidth.StringWidth(whole) + 1
	lead := ansi.Strip(strings.Split(detailWithStatus(t, st, width).View(), "\n")[0])
	if lead != whole {
		t.Errorf("at %d columns:\n got %q\nwant %q", width, lead, whole)
	}
	// And a column less cuts the visible text by one, not by the length of a sequence.
	short := ansi.Strip(strings.Split(detailWithStatus(t, st, width-1).View(), "\n")[0])
	if got, want := runewidth.StringWidth(short), width-2; got != want {
		t.Errorf("at %d columns the line is %d wide, want %d: %q", width-1, got, want, short)
	}
}

// The worker finishes and the next read says so. Nothing keeps an old report on
// screen.
func TestAStatusReportReplacesTheOneBefore(t *testing.T) {
	m := listWithStatus(summary.Status{Enabled: true, Queued: 3}, 120, claudeSession("a"))
	if head := viewLines(m)[0]; !strings.HasSuffix(head, "§ 3 queued") {
		t.Fatalf("header=%q", head)
	}
	updated, _ := m.Update(summaryStatusMsg(summary.Status{Enabled: true}))
	if head := viewLines(updated.(Model))[0]; !strings.HasSuffix(head, "§ idle") {
		t.Errorf("header=%q", head)
	}
}

// A fork's summaries are made from its own conversation, in its own numbering, while
// its detail view is built from the root of its tree. Measured against that tree, a
// fork with every turn described reads as half done.
func TestDetailHeaderDoesNotCountTheTurnsOfAFork(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "one", Prompt: "one"}}},
		{ID: "a1", Parent: "u1", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
		{ID: "u2", Parent: "a1", Events: []domain.Event{{Kind: domain.EventUser, Text: "root two", Prompt: "root two"}}},
		{ID: "a2", Parent: "u2", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
		{ID: "f0:uF", Parent: "a1", Events: []domain.Event{{Kind: domain.EventUser, Text: "fork two", Prompt: "fork two"}}},
		{ID: "f0:aF", Parent: "f0:uF", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
	})
	c.ActiveLeaf = "a2"
	db, e := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if e != nil {
		t.Fatalf("open cache: %v", e)
	}
	t.Cleanup(func() { db.Close() })
	fork := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "fork", ParentSessionID: "root", Fingerprint: "fp"}
	if e := db.PutSummaries(context.Background(), fork, []cache.Summary{
		{Turn: 1, NodeID: "a1", Text: "いち", Model: "m"},
		{Turn: 2, NodeID: "aF", Text: "に", Model: "m"},
	}); e != nil {
		t.Fatalf("put summaries: %v", e)
	}
	st := summary.Status{Enabled: true, Sessions: map[domain.SessionKey]summary.SessionStatus{fork.Key(): {State: summary.SessionQueued}}}
	m := Model{width: 160, height: 20, detailSession: &fork, cache: db, summaryStatus: st}

	updated, _ := m.Update(convMsg{c: &c, focusLeaf: "f0:aF", reset: true})

	lead := viewLines(updated.(Model))[0]
	if strings.Contains(lead, summaryMark) {
		t.Errorf("lead=%q counts turns against the tree rather than the fork", lead)
	}
	// What is happening is looked up by session, so it holds for a fork as well.
	if !strings.HasSuffix(strings.TrimRight(lead, " "), "queued") {
		t.Errorf("lead=%q lost the status", lead)
	}
}

// A fork is read from a tree that starts at its root, while its summaries were made
// along its own line and against the ids its own log gives. Its turns show those,
// and not the summary of the inherited turn that once shared their number.
func TestAForkShowsTheSummariesOfItsOwnTurns(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "one", Prompt: "one"}}},
		{ID: "a1", Parent: "u1", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
		{ID: "u2", Parent: "a1", Events: []domain.Event{{Kind: domain.EventUser, Text: "root two", Prompt: "root two"}}},
		{ID: "a2", Parent: "u2", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
		{ID: "f0:uF", Parent: "a1", Events: []domain.Event{{Kind: domain.EventUser, Text: "fork two", Prompt: "fork two"}}},
		{ID: "f0:aF", Parent: "f0:uF", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer"}}},
	})
	c.ActiveLeaf = "a2"
	c.ForkRoots = []string{"f0:uF"}
	db, e := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if e != nil {
		t.Fatalf("open cache: %v", e)
	}
	t.Cleanup(func() { db.Close() })
	root := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "root"}
	fork := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "fork", ParentSessionID: "root", Fingerprint: "fp"}
	if e := db.PutSummaries(context.Background(), fork, []cache.Summary{
		{Turn: 1, NodeID: "a1", Text: "引き継いだターン", Model: "m"},
		{Turn: 2, NodeID: "aF", Text: "フォーク自身のターン", Model: "m"},
	}); e != nil {
		t.Fatalf("put summaries: %v", e)
	}
	origins := map[string]app.NodeOrigin{
		"u1": {Session: root, NodeID: "u1"}, "a1": {Session: root, NodeID: "a1"},
		"u2": {Session: root, NodeID: "u2"}, "a2": {Session: root, NodeID: "a2", ActiveLeaf: true},
		"f0:uF": {Session: fork, NodeID: "uF"}, "f0:aF": {Session: fork, NodeID: "aF", ActiveLeaf: true},
	}
	m := Model{width: 160, height: 20, detailSession: &fork, sessions: []domain.Session{root, fork}, cache: db}
	u, _ := m.Update(convMsg{c: &c, origins: origins, focusLeaf: "f0:aF", reset: true})
	m = u.(Model)

	view := ansi.Strip(m.detailView())
	if !strings.Contains(view, "フォーク自身のターン") {
		t.Errorf("the fork's own turn lost its summary:\n%s", view)
	}
	if strings.Contains(view, "引き継いだターン") {
		t.Errorf("an inherited turn's summary is shown on the fork's own turn:\n%s", view)
	}
}

// A rewound line goes on with the numbers of the line the session took, which is
// the one that was summarized. A turn of the rewound line does not show the summary
// of the turn that shares its number, and that turn still shows it.
func TestARewoundLineDoesNotShowTheSummariesOfTheSessionsLine(t *testing.T) {
	m := rewoundDetail(t)
	m.summaries = map[int]cache.Summary{3: {Turn: 3, NodeID: "p4", Text: "本線のターン3"}}
	if view := ansi.Strip(m.detailView()); strings.Contains(view, "本線のターン3") {
		t.Errorf("the rewound line's #3 shows the summary of the session's #3:\n%s", view)
	}
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if view := ansi.Strip(u.(Model).detailView()); !strings.Contains(view, "本線のターン3") {
		t.Errorf("the session's own #3 lost its summary:\n%s", view)
	}
}

// The same cut as the detail header's, on the line the turn view leads with.
func TestTurnViewHeaderIsCutByWhatIsVisible(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	head := func(width int) string {
		s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "a", Model: "claude-opus-5-5"}
		c := twoTurns()
		m := Model{width: width, height: 20, detailSession: &s}
		updated, _ := m.Update(convMsg{c: &c, reset: true})
		return strings.TrimRight(ansi.Strip(strings.Split(pressEnter(updated.(Model)).View(), "\n")[0]), " ")
	}
	whole := head(200)
	width := runewidth.StringWidth(whole) + 1
	if got := head(width); got != whole {
		t.Errorf("at %d columns:\n got %q\nwant %q", width, got, whole)
	}
}

// A folder row is not a session, so it has no status to speak for.
func TestListStatusLineIsBlankOnAFolderRow(t *testing.T) {
	s := claudeSession("a")
	s.CWD = "/repo"
	st := summary.Status{Enabled: true, Sessions: map[domain.SessionKey]summary.SessionStatus{sessionKey("a"): {State: summary.SessionQueued}}}
	m := Model{view: "folder", width: 120, height: 10, sessions: []domain.Session{s}, summaryStatus: st}
	m.filter()
	if m.rows[m.cursor].Kind != "header" {
		t.Fatalf("the cursor is on a %q row", m.rows[m.cursor].Kind)
	}
	if line := viewLines(m)[1]; strings.TrimSpace(line) != "" {
		t.Errorf("line=%q", line)
	}
}

// A failure that cost nothing is not counted, so there is no count to give. It is
// still a failure, and "failed 0×" would say the opposite.
func TestAFailureWithNoCountSaysItFailed(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	waiting := summary.SessionStatus{State: summary.SessionFailed, LastTried: now, RetryAt: now.Add(time.Hour), LastError: "limit"}
	if got, want := sessionSummaryLine(waiting, now), "§ failed, retry in 1h00m: limit"; got != want {
		t.Errorf("line=%q want %q", got, want)
	}
	ready := summary.SessionStatus{State: summary.SessionQueued, LastTried: now, LastError: "limit"}
	if got, want := sessionSummaryLine(ready, now), "§ queued to retry, failed: limit"; got != want {
		t.Errorf("line=%q want %q", got, want)
	}
	m := Model{summaryStatus: summary.Status{Enabled: true}}
	if note := m.detailSummaryNote(waiting); !strings.Contains(note, "failed") || strings.Contains(note, "×") {
		t.Errorf("note=%q", note)
	}
}

// turnRowFixture is a session of one turn asking "やって", on a screen w columns wide,
// holding the given summaries.
func turnRowFixture(t *testing.T, w int, summaries map[int]cache.Summary) (Model, *domain.Session, detailRow) {
	t.Helper()
	ts := time.Unix(1, 0)
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts, Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts, Events: []domain.Event{{Kind: domain.EventAssistant, Text: "できた"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "x"}
	m := Model{width: w, height: 30, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)
	m.summaries = summaries
	for _, r := range m.detailRows {
		if r.Kind == "turn" {
			return m, &s, r
		}
	}
	t.Fatal("no turn row")
	return m, &s, detailRow{}
}

// A prompt says what was asked, and often little: "コミットして", "進めて". What came of it
// follows in whatever room the row has left, marked as generated, so a list can be read
// without opening each turn and without losing a row to do it.
func TestATurnRowContinuesIntoItsSummary(t *testing.T) {
	m, s, row := turnRowFixture(t, 120, map[int]cache.Summary{1: {Turn: 1, NodeID: "a1", Text: "yキーを追加した"}})
	line := ansi.Strip(m.detailTurnLine(s, row, false, [9]int{}, 0, false))
	if !strings.Contains(line, "やって") {
		t.Errorf("the prompt is gone: %q", line)
	}
	if !strings.Contains(line, "やって  § yキーを追加した") {
		t.Errorf("the summary does not follow the prompt: %q", line)
	}
}

// One row is one line, however the summary was written and however long it is.
func TestATurnRowStaysOneLineOfTheScreen(t *testing.T) {
	long := strings.Repeat("長い要約の一文。", 40) + "\n\n" + strings.Repeat("次の段落。", 40)
	m, s, row := turnRowFixture(t, 80, map[int]cache.Summary{1: {Turn: 1, NodeID: "a1", Text: long}})
	for _, selected := range []bool{false, true} {
		line := m.detailTurnLine(s, row, selected, [9]int{}, 0, false)
		if strings.Contains(line, "\n") {
			t.Errorf("selected=%v: the row spans lines: %q", selected, line)
		}
		if got := lipgloss.Width(line); got > 79 {
			t.Errorf("selected=%v: the row is %d columns wide on a screen of 80", selected, got)
		}
	}
	if line := m.detailTurnLine(s, row, true, [9]int{}, 0, false); lipgloss.Width(line) != 79 {
		t.Errorf("the selected row is %d columns wide, want the highlight across the line", lipgloss.Width(line))
	}
}

// A few columns of a summary say nothing and read as a mistake, so a row without the
// room for it shows the prompt as it always did.
func TestATurnRowLeavesOutASummaryItHasNoRoomFor(t *testing.T) {
	m, s, row := turnRowFixture(t, 120, map[int]cache.Summary{1: {Turn: 1, NodeID: "a1", Text: "yキーを追加した"}})
	wide := ansi.Strip(m.detailTurnLine(s, row, false, [9]int{}, 0, false))
	before := runewidth.StringWidth(wide[:strings.Index(wide, summaryMark)])
	room := func(w int) string {
		m.width = w
		return ansi.Strip(m.detailTurnLine(s, row, false, [9]int{}, 0, false))
	}
	// 1 for the last column every row leaves free, and the gap before the mark.
	if line := room(before + turnSummaryMinWidth + 1); !strings.Contains(line, summaryMark) {
		t.Errorf("a row with room for it left the summary out: %q", line)
	}
	if line := room(before + turnSummaryMinWidth); strings.Contains(line, summaryMark) {
		t.Errorf("a row with too little room showed a piece of the summary: %q", line)
	}
}

// Only that turn's own summary. Key 0 is the session's, which reads as this turn's if it
// is shown here, and a row with none shows what it always did.
func TestATurnRowShowsOnlyItsOwnSummary(t *testing.T) {
	m, s, row := turnRowFixture(t, 120, map[int]cache.Summary{0: {Text: "セッションの要約"}, 2: {Turn: 2, Text: "別のターンの要約"}})
	if line := ansi.Strip(m.detailTurnLine(s, row, false, [9]int{}, 0, false)); strings.Contains(line, summaryMark) {
		t.Errorf("a turn with no summary of its own shows one: %q", line)
	}
	row.TurnIndex = -1
	if got := m.turnSummaryText(row); got != "" {
		t.Errorf("a row with no turn number reads as %q", got)
	}
}

// A word on screen that finds nothing reads as a broken search.
func TestATurnRowsSummaryCanBeSearchedFor(t *testing.T) {
	m, _, row := turnRowFixture(t, 120, map[int]cache.Summary{1: {Turn: 1, NodeID: "a1", Text: "yキーを追加した"}})
	if got := m.detailRowText(row); !strings.Contains(got, "yキーを追加") {
		t.Errorf("the row's searchable text is %q", got)
	}
}

// A session list holds titles and headlines side by side as summaries arrive, and only
// one of the two is what the session said. The headline carries the mark and color that
// generated text has everywhere else, and a title keeps neither.
func TestASessionRowMarksOnlyAHeadlineAsGenerated(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	yellow := lipgloss.NewStyle().Foreground(roleColor("meta")).Render("x")
	yellow = yellow[:strings.Index(yellow, "x")]

	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "with", Title: "最初のプロンプト", Fingerprint: "fp"}
	none := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "without", Title: "別のプロンプト"}
	m := Model{width: 120, headlines: map[domain.SessionKey]cache.Summary{
		s.Key(): {Headline: "要約の見出し", Fingerprint: "fp"},
	}}

	row := m.sessionRow(s, false, 0, "")
	if !strings.Contains(ansi.Strip(row), summaryMark+" 要約の見出し") {
		t.Errorf("the headline is not marked: %q", ansi.Strip(row))
	}
	if !strings.Contains(row, yellow+summaryMark+" 要約の見出し") {
		t.Errorf("the headline is not in the color of generated text: %q", row)
	}

	// A session with none yet keeps its title, as written.
	if row := m.sessionRow(none, false, 0, ""); strings.Contains(row, summaryMark) || strings.Contains(row, yellow+"別のプロンプト") {
		t.Errorf("a title reads as generated: %q", row)
	}

	// `s` shows the titles of them all.
	m.listTitles = true
	if row := m.sessionRow(s, false, 0, ""); strings.Contains(row, summaryMark) || !strings.Contains(ansi.Strip(row), "最初のプロンプト") {
		t.Errorf("a title reads as generated: %q", row)
	}

	// Out of date, it still says so first, as the detail header does.
	m.listTitles = false
	moved := s
	moved.Fingerprint = "fp2"
	moved.UpdatedAt = time.Now()
	h := m.headlines[s.Key()]
	if !h.Stale(moved, m.summaryGrace()) {
		t.Fatal("test setup: the headline does not read as stale")
	}
	m.headlines = map[domain.SessionKey]cache.Summary{moved.Key(): h}
	if row := ansi.Strip(m.sessionRow(moved, false, 0, "")); !strings.Contains(row, summaryMark+" (stale) 要約の見出し") {
		t.Errorf("row=%q", row)
	}
}

// A search hit stands out from the rows around it, but not in the color of generated
// text: the summary after it is in that color, and the prompt is what the session said.
func TestASearchHitsPromptIsNotColoredAsGenerated(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })
	yellow := lipgloss.NewStyle().Foreground(roleColor("meta")).Render("x")
	yellow = yellow[:strings.Index(yellow, "x")]

	m, s, row := turnRowFixture(t, 120, map[int]cache.Summary{1: {Turn: 1, NodeID: "a1", Text: "yキーを追加した"}})
	hit := m.detailTurnLine(s, row, false, [9]int{}, 0, true)
	if strings.Contains(hit, yellow+"やって") {
		t.Errorf("the prompt of a hit is in the color of generated text: %q", hit)
	}
	if hit == m.detailTurnLine(s, row, false, [9]int{}, 0, false) {
		t.Error("a hit looks the same as a row that is not one")
	}
}
