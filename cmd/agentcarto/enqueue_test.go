package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/core/domain"
)

// Choosing wrongly costs money, so the choosing is deliberate: oldest first, and
// only the sessions there is nothing to be had from are left out. The per-run
// budget is not applied here — see TestTheSweepAdvancesPastWhatIsAlreadyDone.
func TestPickForSummary(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	sessions := []domain.Session{
		{SessionID: "running", UpdatedAt: ago(time.Hour), Status: domain.StatusRunning},
		{SessionID: "just-touched", UpdatedAt: ago(time.Minute)},
		{SessionID: "just-finished", UpdatedAt: ago(time.Minute), LastKind: domain.EventTurnComplete},
		{SessionID: "newest", UpdatedAt: ago(time.Hour)},
		{SessionID: "older", UpdatedAt: ago(5 * time.Hour)},
		{SessionID: "oldest", UpdatedAt: ago(30 * 24 * time.Hour)},
		{SessionID: "log-gone", UpdatedAt: ago(2 * time.Hour), LogDeleted: true},
		{SessionID: "empty-fork", UpdatedAt: ago(2 * time.Hour), EmptyFork: true},
	}
	got := pickForSummary(sessions)
	var ids []string
	for _, s := range got {
		ids = append(ids, s.SessionID)
	}
	// Oldest first: those are the ones whose logs are about to be rotated away.
	//
	// running and just-touched are both in the list. What must not be summarized
	// is the turn being written, not the session holding it, and that is decided
	// per turn once the turns are known (summarizableTurns). Holding the session
	// back made its finished turns wait for the unfinished one.
	//
	// log-gone is in the list. Its log is what is gone, not its conversation: the
	// cache keeps that, and it is the session with the most to lose. Whether a
	// copy was actually kept takes a store lookup, which is worthOpening's job.
	//
	// empty-fork is the only one left out: a full-copy fork nobody continued has
	// nothing of its own to describe.
	want := []string{"oldest", "older", "log-gone", "running", "newest", "just-touched", "just-finished"}
	if len(ids) != len(want) {
		t.Fatalf("picked %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("picked %v, want %v (oldest first)", ids, want)
		}
	}
}

// A first run over a whole machine must spend a bounded amount, not everything
// at once — and every later run has to reach further than the last, or the
// backfill never finishes.
//
// Two rules are checked here because they live in the same loop. Half the budget
// goes to each end of the list: the oldest sessions are the ones about to lose
// what they would be summarized from, the newest are the ones being read. And
// the budget counts the sessions a run opens — applied to the candidate list
// before knowing which of them are already summarized, it picks the same ones
// forever: the first max_per_run get done, nothing is queued, and the rest are
// never reached.
func TestTheSweepAdvancesPastWhatIsAlreadyDone(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a, cfg := settledAppN(6)
	cfg.Summary.Agent, cfg.Summary.MaxPerRun = "claude", 2

	queued := func() []string {
		t.Helper()
		q, err := summary.OpenQueue(queueDir())
		if err != nil {
			t.Fatal(err)
		}
		reqs, _ := q.List()
		var ids []string
		for _, r := range reqs {
			ids = append(ids, r.SessionID)
		}
		sort.Strings(ids)
		return ids
	}
	// The fixture's sessions are s0 (newest) … s5 (oldest), and the budget is two:
	// one from each end.
	sessions := scanSessions(ctx, a, cfg, d)
	if got := queued(); len(got) != 2 || got[0] != "s0" || got[1] != "s5" {
		t.Fatalf("the first run queued %v, want one from each end [s0 s5]", got)
	}
	// They are summarized and leave the queue, as a worker would leave them.
	for _, id := range []string{"s0", "s5"} {
		s := domain.Session{PluginID: "claude", SessionID: id}
		for _, x := range sessions {
			if x.SessionID == id {
				s = x
			}
		}
		if err := d.PutSummaries(ctx, s, []cache.Summary{{Turn: 0, Text: "done"}}); err != nil {
			t.Fatal(err)
		}
		q, _ := summary.OpenQueue(queueDir())
		r, ok := q.Find("claude", id)
		if !ok {
			t.Fatalf("%s is not in the queue", id)
		}
		if err := q.Done(r); err != nil {
			t.Fatal(err)
		}
	}
	// The next run must reach the two after them, one end at a time again, and
	// not pick s0 and s5 a second time.
	scanSessions(ctx, a, cfg, d)
	if got := queued(); len(got) != 2 || got[0] != "s1" || got[1] != "s4" {
		t.Fatalf("the second run queued %v, want the next from each end [s1 s4] — the sweep stalled", got)
	}
}

// The session summary is the one that can describe a session that has since
// gone on: it has no turn to be anchored to, while every per-turn summary is
// held against the id of its terminal node and withheld when that moves. The
// store records the fingerprint it was made from, and show compares it.
func TestAGrownSessionReportsItsSummaryAsStale(t *testing.T) {
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := domain.Session{PluginID: "claude", SessionID: "s1", Fingerprint: "fp1"}
	if err := d.PutSummaries(ctx, s, []cache.Summary{{Turn: 0, Text: "セッション全体", Model: "m"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, stale := storedSummaries(ctx, d, s, nil, 0); stale {
		t.Error("a summary made from this very version of the log reads as stale")
	}
	grown := s
	grown.Fingerprint = "fp2"
	sums, model, stale := storedSummaries(ctx, d, grown, nil, 0)
	if !stale {
		t.Error("a summary made before the newest turns does not report itself as stale")
	}
	// It is still printed: an out-of-date summary of a session beats none, as
	// long as the document says which it is.
	if sums[0] == "" || model == "" {
		t.Errorf("the stale summary was dropped instead of flagged: %v %q", sums, model)
	}
}

// A session whose log is gone can still be summarized, as long as the cache
// kept its conversation — App.Conversation reads a deleted session back from
// there, which is what makes `show` work on one. It is also the session with the
// most to lose: a summary is a kilobyte the cache never evicts, while the
// conversation it came from is a hundred times that and does get evicted.
//
// The one that was never copied is skipped without a parse, since opening it
// would fail every run and hold a place in the per-run budget while doing so.
func TestALogDeletedSessionIsSummarizedFromTheCachedConversation(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	kept := domain.Session{PluginID: "claude", SessionID: "kept", Fingerprint: "fp1", LogDeleted: true}
	lost := domain.Session{PluginID: "claude", SessionID: "lost", Fingerprint: "fp1", LogDeleted: true}
	conv := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Now(), Events: talk("何を決めたか", "こう決めた")},
	})
	if err := d.PutBlob(ctx, kept, app.ConversationArtifactKind, conv); err != nil {
		t.Fatal(err)
	}
	if !worthOpening(ctx, d, q, kept, time.Hour) {
		t.Error("a deleted session whose conversation was kept was ruled out")
	}
	if worthOpening(ctx, d, q, lost, time.Hour) {
		t.Error("a deleted session with no copy would be opened, and fail, on every run")
	}
}

// A session that renders to no document at all — one holding only commands, or
// no turns — is remembered as such. Without that it is opened again on every
// run and holds a place in the per-run budget forever, which at the old end of
// the list stops the sweep as surely as the bug above.
func TestASessionWithNothingToSummarizeIsNotOpenedTwice(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	s := domain.Session{
		PluginID: "claude", AgentType: "claude", SessionID: "empty", Fingerprint: "fp1",
		UpdatedAt: now.Add(-time.Hour), LastKind: domain.EventTurnComplete,
		SourceRef: domain.SessionRef{Source: "/logs/empty.jsonl"},
	}
	a, _ := fixtureApp([]domain.Session{s}, map[string]domain.Conversation{
		"/logs/empty.jsonl": domain.NewConversation(nil),
	})
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	if queued, _ := queueOne(ctx, a, d, q, s, time.Hour); queued {
		t.Fatal("a session with no turns was queued")
	}
	if worthOpening(ctx, d, q, s, time.Hour) {
		t.Error("the session would be opened again on the next run")
	}
	// The record must not show up as a summary: it has no text, and every reader
	// tests for that before printing.
	if sums, model, stale := storedSummaries(ctx, d, s, nil, 0); len(sums) != 0 || model != "" || stale {
		t.Errorf("the blank record reads back as a summary: %v %q stale=%v", sums, model, stale)
	}
	// A log that grew is reconsidered: the fingerprint no longer matches.
	grown := s
	grown.Fingerprint = "fp2"
	if !worthOpening(ctx, d, q, grown, time.Hour) {
		t.Error("a session that grew was still treated as having nothing to summarize")
	}
}

// A session summary the interval deferred must not be lost. The run that stored
// a new turn and left turn 0 alone recorded the version through MarkExamined all
// the same, so the fingerprint says there is nothing to do — and the log of a
// conversation that has ended never moves again. Without asking this second
// question the deferral is permanent.
func TestADeferredSessionSummaryIsAskedForAgain(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	when := time.Now().Add(-time.Hour)
	s := domain.Session{
		PluginID: "claude", AgentType: "claude", SessionID: "deferred", Fingerprint: "fp1",
		UpdatedAt: when, LastKind: domain.EventTurnComplete,
		SourceRef: domain.SessionRef{Source: "/logs/deferred.jsonl"},
	}
	a, _ := fixtureApp([]domain.Session{s}, map[string]domain.Conversation{
		"/logs/deferred.jsonl": domain.NewConversation([]domain.ConvNode{
			{ID: "u1", Timestamp: when, Events: talk("何を決めたか", "こう決めた")},
		}),
	})
	// The session summary first and the turn a second later, which is the shape a
	// run that deferred turn 0 leaves behind. Created is stored to the second, so
	// the two have to fall in different ones.
	if err := d.PutSummaries(ctx, s, []cache.Summary{{Turn: 0, Text: "セッション全体の要約"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := d.PutSummaries(ctx, s, []cache.Summary{{Turn: 1, NodeID: "u1", Text: "ターン1の要約"}}); err != nil {
		t.Fatal(err)
	}

	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	if worthOpening(ctx, d, q, s, time.Hour) {
		t.Error("the interval has not elapsed, so nothing is owed yet and opening the session costs a parse for nothing")
	}
	if !worthOpening(ctx, d, q, s, 0) {
		t.Fatal("a session summary left behind by the turns under it would never be remade")
	}
	if queued, _ := queueOne(ctx, a, d, q, s, 0); !queued {
		t.Fatal("nothing was queued for a session whose own summary is overdue")
	}
	r, ok := q.Find(s.PluginID, s.SessionID)
	if !ok {
		t.Fatal("the request is not in the queue")
	}
	// It asks about no turn: they are all described. What the worker does with such
	// a request is go straight to the session summary.
	if len(r.Prompts) != 0 || len(r.Batches) != 0 {
		t.Errorf("the request carries turn prompts: %d prompts, %d batches", len(r.Prompts), len(r.Batches))
	}
	if r.Held {
		t.Error("a held request writes nothing to turn 0, which is the one thing this asks for")
	}
	if r.Nodes[1] != "u1" {
		t.Errorf("the request must name the turns the summary is built from, got %v", r.Nodes)
	}

	// The other way turn 0 comes to be owed: a session-summary call that failed
	// leaves the turns described and the session not. There is no summary to
	// measure an interval from, so it is owed whatever the interval says.
	blank := domain.Session{PluginID: "claude", SessionID: "never", Fingerprint: "fp1"}
	if err := d.PutSummaries(ctx, blank, []cache.Summary{{Turn: 1, NodeID: "n1", Text: "ターン1の要約"}}); err != nil {
		t.Fatal(err)
	}
	if !sessionSummaryOverdue(ctx, d, blank, time.Hour) {
		t.Error("a session whose turns are described and whose own summary was never written is owed one")
	}
	// And a session nothing was ever stored for is not owed anything: the blank
	// row MarkExamined leaves is a record that there was nothing to describe.
	examined := domain.Session{PluginID: "claude", SessionID: "examined", Fingerprint: "fp1"}
	if err := d.MarkExamined(ctx, examined); err != nil {
		t.Fatal(err)
	}
	if sessionSummaryOverdue(ctx, d, examined, time.Hour) {
		t.Error("a session with nothing stored would be opened and paid for on every scan")
	}
}

// A session being worked in is a candidate like any other. What must not be
// summarized is the turn being written, and that is decided per turn, where the
// turns are known — see TestPendingTurnsLeavesOutTheTurnStillBeingWritten.
//
// Held back by session, as this was, a session someone is working in all day
// never had its finished turns summarized: the very sessions a reader wants.
func TestPickForSummaryTakesSessionsThatAreStillMoving(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	for _, s := range []domain.Session{
		// An agent is in it right now, mid-tool.
		{SessionID: "running", UpdatedAt: now.Add(-time.Second), Status: domain.StatusRunning, LastKind: domain.EventToolCall},
		// Attached, and what it is doing is not a state the status display names.
		{SessionID: "other", UpdatedAt: now.Add(-time.Second), Status: domain.StatusOther, LastKind: domain.EventUser},
		// Being written this very second.
		{SessionID: "seconds-ago", UpdatedAt: now.Add(-time.Second), LastKind: domain.EventStream},
		// Open, and waiting for a prompt.
		{SessionID: "ready", UpdatedAt: now.Add(-time.Second), Status: domain.StatusReady, LastKind: domain.EventTurnComplete},
	} {
		if got := pickForSummary([]domain.Session{s}); len(got) != 1 {
			t.Errorf("%s was left out", s.SessionID)
		}
	}
	// The one exclusion left: a full-copy fork nobody ever continued holds
	// nothing of its own to describe.
	fork := domain.Session{SessionID: "empty-fork", UpdatedAt: now.Add(-time.Hour), EmptyFork: true}
	if got := pickForSummary([]domain.Session{fork}); len(got) != 0 {
		t.Error("a fork that was never continued was picked")
	}
}

// A turn that finished is queued the moment a scan sees it, however recently the
// session was summarized. Waiting was what made a session someone works in all
// day sit an hour behind: the finished turns were held back to pace the session
// summary, which is now paced where it is written (storeSessionSummary).
func TestAFinishedTurnIsQueuedWithoutWaiting(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	src := "/logs/open.jsonl"
	// Two turns; the first is described already, from the version before the
	// second one existed.
	conv := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: now, Events: talk("最初の質問", "最初の答え")},
		{ID: "u2", Parent: "u1", Timestamp: now, Events: talk("次の質問", "次の答え")},
	})
	done := domain.Session{PluginID: "claude", SessionID: "open", Fingerprint: "fp1"}
	if err := d.PutSummaries(ctx, done, []cache.Summary{
		{Turn: 0, Text: "セッション要約", Model: "m"},
		{Turn: 1, NodeID: "u1", Text: "ターン1", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	// The same session as the scan sees it now: one turn newer, and that turn has
	// sat still long enough to be trusted as finished (settleAfterComplete).
	s := domain.Session{
		PluginID: "claude", AgentType: "claude", SessionID: "open", CWD: "/repo/app", Title: "open",
		UpdatedAt: now.Add(-2 * time.Minute), LastKind: domain.EventTurnComplete,
		Fingerprint: "fp2", SourceRef: domain.SessionRef{Source: src},
	}
	a, cfg := fixtureApp([]domain.Session{s}, map[string]domain.Conversation{src: conv})
	cfg.Summary.Agent, cfg.Summary.MaxPerRun = "claude", 4

	EnqueueSummaries(ctx, a, cfg, d, []domain.Session{s})

	r, ok := q.Find("claude", "open")
	if !ok {
		t.Fatal("a finished turn was not queued — the session was summarized moments ago, which is no longer a reason to wait")
	}
	var asked []int
	for _, b := range r.Batches {
		asked = append(asked, b...)
	}
	if len(asked) != 1 || asked[0] != 2 {
		t.Errorf("queued turns %v, want only the new one (2)", asked)
	}
}

func TestPickForSummaryOnNothing(t *testing.T) {
	if got := pickForSummary(nil); len(got) != 0 {
		t.Errorf("picked %v from no sessions", got)
	}
}

// Deciding whether a session needs summarizing takes its conversation, and a
// scan runs every few seconds. The sessions that plainly need nothing have to
// be recognized without parsing, or that answer costs a parse of everything on
// the machine every few seconds.
//
// A nil app panics the moment queueOne reaches the parse, so returning at all
// is the assertion that it did not get there.
func TestQueueOneSkipsWithoutParsing(t *testing.T) {
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := domain.Session{PluginID: "claude", SessionID: "s1", Fingerprint: "fp1"}

	t.Run("already queued", func(t *testing.T) {
		q, err := summary.OpenQueue(filepath.Join(t.TempDir(), "q"))
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Add(summary.Request{PluginID: "claude", SessionID: "s1", Prompts: []string{"doc"}}); err != nil {
			t.Fatal(err)
		}
		if queued, _ := queueOne(ctx, nil, d, q, s, time.Hour); queued {
			t.Error("a session already in the queue was queued again")
		}
	})

	t.Run("summarized and unchanged", func(t *testing.T) {
		q, err := summary.OpenQueue(filepath.Join(t.TempDir(), "q"))
		if err != nil {
			t.Fatal(err)
		}
		if err := d.PutSummaries(ctx, s, []cache.Summary{{Turn: 0, Text: "the session"}}); err != nil {
			t.Fatal(err)
		}
		if queued, _ := queueOne(ctx, nil, d, q, s, time.Hour); queued {
			t.Error("a session whose log has not moved since it was summarized was queued")
		}
	})

	t.Run("summarized but the log grew", func(t *testing.T) {
		q, err := summary.OpenQueue(filepath.Join(t.TempDir(), "q"))
		if err != nil {
			t.Fatal(err)
		}
		grown := s
		grown.SessionID = "s2"
		if err := d.PutSummaries(ctx, grown, []cache.Summary{{Turn: 0, Text: "the session"}}); err != nil {
			t.Fatal(err)
		}
		grown.Fingerprint = "fp2" // the log moved
		// This one has to be parsed to know what changed, so it reaches the app.
		// With a nil app that is a panic, which is the point: recover and report.
		defer func() {
			if recover() == nil {
				t.Error("a session whose log grew was skipped without parsing")
			}
		}()
		_, _ = queueOne(ctx, nil, d, q, grown, time.Hour)
	})
}

// show waits when one call will do and hands over when it will not. The split
// is on the number of calls, since that is what decides whether the wait is
// half a minute or a quarter of an hour.
func TestTurnsIn(t *testing.T) {
	if got := turnsIn(summary.Request{Batches: [][]int{{1, 2, 3}, {4, 5}}}); got != 5 {
		t.Errorf("turnsIn=%d want 5", got)
	}
	if got := turnsIn(summary.Request{}); got != 0 {
		t.Errorf("turnsIn of nothing=%d want 0", got)
	}
}

// A worker takes max_per_run sessions and stops, so the queue is normally left
// holding more than it drained. Those sessions are skipped by queueOne (they
// are already queued), so a scan that adds nothing must still start a worker —
// otherwise the queue stalls with work in it.
func TestAnyReady(t *testing.T) {
	now := time.Now()
	q, err := summary.OpenQueue(filepath.Join(t.TempDir(), "q"))
	if err != nil {
		t.Fatal(err)
	}
	if anyReady(q) {
		t.Error("an empty queue reports work")
	}
	if err := q.Add(summary.Request{PluginID: "claude", SessionID: "waiting", Queued: now}); err != nil {
		t.Fatal(err)
	}
	if !anyReady(q) {
		t.Error("a queued request was not reported as work")
	}

	// A request waiting out a failure is not work: starting a worker for it
	// would spawn a process that looks at it and exits, every few seconds.
	q2, err := summary.OpenQueue(filepath.Join(t.TempDir(), "q2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := q2.Add(summary.Request{PluginID: "claude", SessionID: "failed", Queued: now, Attempts: 1, LastTried: now}); err != nil {
		t.Fatal(err)
	}
	if anyReady(q2) {
		t.Error("a request inside its backoff was reported as work")
	}
}

// settledAppN serves n sessions whose last turn is complete, so that every turn
// they hold is summarizable and what a test sees is the queueing itself. They
// are named s0 (newest) through s<n-1> (oldest), one hour apart.
func settledAppN(n int) (*app.App, config.Config) {
	now := time.Now()
	var sessions []domain.Session
	convs := map[string]domain.Conversation{}
	for i := range n {
		id := fmt.Sprintf("s%d", i)
		src := "/logs/" + id + ".jsonl"
		sessions = append(sessions, domain.Session{
			PluginID: "claude", AgentType: "claude", SessionID: id, CWD: "/repo/app", Title: id,
			UpdatedAt: now.Add(-time.Duration(i+1) * time.Hour), LastKind: domain.EventTurnComplete,
			SourceRef: domain.SessionRef{Source: src},
		})
		convs[src] = domain.NewConversation([]domain.ConvNode{
			{ID: "u1", Timestamp: now, Events: talk(id+" の質問", id+" の答え")},
		})
	}
	return fixtureApp(sessions, convs)
}

func settledApp() (*app.App, config.Config) { return settledAppN(2) }

// summaryFixture points the queue, the lock and the log at a directory of the
// test's own, and stops a worker from actually being spawned — under `go test`
// that would re-execute the test binary.
func summaryFixture(t *testing.T) (dir string, started *int) {
	t.Helper()
	dir = t.TempDir()
	n := 0
	prevRoot, prevSpawn := summaryRoot, spawnWorker
	summaryRoot = dir
	spawnWorker = func() error { n++; return nil }
	t.Cleanup(func() { summaryRoot, spawnWorker = prevRoot, prevSpawn })
	return dir, &n
}

// Summaries have to appear for someone who only ever runs the CLI. Hanging the
// queueing on the TUI alone left the whole feature switched off for anyone
// using `agentcarto list` and `agentcarto show`, which is how the /past-sessions
// skill reads sessions — it shipped that way in v0.15.1.
func TestTheCommandLineQueuesAndStartsAWorker(t *testing.T) {
	_, started := summaryFixture(t)
	a, cfg := settledApp()
	cfg.Summary.Agent, cfg.Summary.MaxPerRun = "claude", 20
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var b bytes.Buffer
	listCmd(context.Background(), a, cfg, d, nil, false, &b)

	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	reqs, bad := q.List()
	if len(bad) > 0 {
		t.Fatalf("unreadable requests: %v", bad)
	}
	if len(reqs) != 2 {
		t.Fatalf("`list` queued %d of the 2 sessions it listed", len(reqs))
	}
	if *started != 1 {
		t.Errorf("a worker was started %d times, want once", *started)
	}
}

// --no-cache says not to touch the store on this run. It reaches the commands
// as a nil cache, and a worker is a process that writes summaries into that
// store — one the flag cannot reach, since it is detached and reads the
// configuration for itself. Splitting queueing from starting made this possible
// for the first time: the old shape returned on a nil cache before it got as
// far as starting anything.
func TestNoCacheStartsNothing(t *testing.T) {
	_, started := summaryFixture(t)
	a, cfg := settledApp()
	cfg.Summary.Agent, cfg.Summary.MaxPerRun = "claude", 20
	// An earlier run, made with the cache, left work behind.
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Add(summary.Request{PluginID: "claude", SessionID: "left-over", Queued: time.Now(), Prompts: []string{"doc"}}); err != nil {
		t.Fatal(err)
	}

	var b bytes.Buffer
	listCmd(context.Background(), a, cfg, nil, nil, false, &b) // nil cache: --no-cache

	if *started != 0 {
		t.Errorf("--no-cache started %d workers", *started)
	}
}

// Nothing is queued and nothing is spawned while the feature is off, which is
// how it ships. A command that quietly spent money on a machine that never
// asked for summaries would be a poor thing to hand anyone.
func TestTheCommandLineQueuesNothingWhenSummariesAreOff(t *testing.T) {
	_, started := summaryFixture(t)
	a, cfg := commandApp() // cfg.Summary.Agent is empty
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var b bytes.Buffer
	listCmd(context.Background(), a, cfg, d, nil, false, &b)

	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	if reqs, _ := q.List(); len(reqs) != 0 {
		t.Errorf("%d sessions were queued with summary.agent empty", len(reqs))
	}
	if *started != 0 {
		t.Error("a worker was started with summary.agent empty")
	}
}

// The scan runs before a command has printed anything, and `show` summarizes the
// session it was asked for itself. A worker started during the scan would be
// picking through the queue while show generates, and the two could land on the
// same session — so queueing and starting are separate, and only the second one
// waits for the output.
func TestEnqueueDoesNotStartAWorker(t *testing.T) {
	_, started := summaryFixture(t)
	a, cfg := settledApp()
	cfg.Summary.Agent, cfg.Summary.MaxPerRun = "claude", 20
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	sessions := scanSessions(context.Background(), a, cfg, d)
	if len(sessions) == 0 {
		t.Fatal("the fixture scanned nothing")
	}
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	if reqs, _ := q.List(); len(reqs) == 0 {
		t.Error("the scan queued nothing")
	}
	if *started != 0 {
		t.Error("the scan started a worker instead of leaving it to the command")
	}
	// And the command's half does start one, on a queue it added nothing to.
	StartSummaryWorker(cfg, d)
	if *started != 1 {
		t.Errorf("a worker was started %d times, want once", *started)
	}
}

// A session too long to summarize inline is handed to the background, and the
// worker for it is the one showCmd defers — not a second one started here.
// Starting both spawns a process that arrives to find the lock taken and exits,
// writing a line to the log for nothing.
func TestShowHandsOffWithoutStartingItsOwnWorker(t *testing.T) {
	_, started := summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	s := domain.Session{PluginID: "claude", SessionID: "long", Fingerprint: "fp1"}
	// Two prompts: more than one call, so this is the hand-off branch and no
	// generation happens here.
	if err := q.Add(summary.Request{
		PluginID: "claude", SessionID: "long", Queued: time.Now(),
		Batches: [][]int{{1}, {2}}, Prompts: []string{"a", "b"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Summary: config.Summary{Agent: "claude", Model: "claude-sonnet-5"}}
	if summarizeForShow(ctx, nil, cfg, d, s, "long") {
		t.Error("a session needing several calls was reported as summarized")
	}
	if *started != 0 {
		t.Errorf("summarizeForShow started %d workers; showCmd's deferred one is the only one", *started)
	}
	if _, ok := q.Find("claude", "long"); !ok {
		t.Error("the request was dropped instead of being left for the worker")
	}
}

// capturingStderr runs f with os.Stderr replaced, and returns what it wrote.
func capturingStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	f()
	os.Stderr = prev
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// A session waiting out a failure is the one session a reader is told nothing
// about: it is not summarized, and nothing is being done about it right now.
// Silence there reads as "this session has nothing to say".
func TestShowSaysWhyASessionIsWaitingOutAFailure(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	s := domain.Session{PluginID: "claude", SessionID: "failing", Fingerprint: "fp1"}
	cfg := config.Config{Summary: config.Summary{Agent: "claude", Model: "claude-sonnet-5"}}
	base := summary.Request{PluginID: "claude", SessionID: "failing", Queued: time.Now(), Batches: [][]int{{1}}, Prompts: []string{"doc"}}

	// Failed once, inside the backoff: it comes back on its own.
	base.Attempts, base.LastTried = 1, time.Now()
	if err := q.Add(base); err != nil {
		t.Fatal(err)
	}
	out := capturingStderr(t, func() { summarizeForShow(ctx, nil, cfg, d, s, "failing") })
	if !strings.Contains(out, "tried again in") || !strings.Contains(out, "agentcarto summarize failing") {
		t.Errorf("a session inside its backoff explains neither the wait nor the way past it: %q", out)
	}

	// Failed often enough to stop: nothing will pick it up, so saying "later"
	// would be a lie.
	base.Attempts = summary.MaxAttempts
	if err := q.Add(base); err != nil {
		t.Fatal(err)
	}
	out = capturingStderr(t, func() { summarizeForShow(ctx, nil, cfg, d, s, "failing") })
	if !strings.Contains(out, "no longer retried") {
		t.Errorf("a session that is done being tried is reported as waiting: %q", out)
	}
}

// An outline with no summaries under any turn is the shape this feature exists
// to replace. A reader who never turned it on cannot tell that from a session
// nothing could be written about.
func TestShowSaysWhenSummariesCouldNotHaveBeenMade(t *testing.T) {
	off := config.Config{}
	if n := summariesOffNotice(off, nil); !strings.Contains(n, "summary.agent") {
		t.Errorf("with the feature off the notice does not say how to turn it on: %q", n)
	}
	on := config.Config{Summary: config.Summary{Agent: "claude"}}
	if n := summariesOffNotice(on, nil); !strings.Contains(n, "--no-cache") {
		t.Errorf("with no cache the notice does not name the reason: %q", n)
	}
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Configured and able to write: this run could have made summaries, so the
	// absence of one is about the session and the paths that know it speak up.
	if n := summariesOffNotice(on, d); n != "" {
		t.Errorf("a run that could have summarized still explained itself: %q", n)
	}
}

// queueOne answers false for two opposite situations, and show has to tell them
// apart: "nothing to summarize" means print what there is, while "already
// queued" means this is precisely the session someone asked to read — waiting
// behind sixty others is not an answer to that.
func TestShowActsOnAQueuedSession(t *testing.T) {
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q, err := summary.OpenQueue(filepath.Join(t.TempDir(), "q"))
	if err != nil {
		t.Fatal(err)
	}
	s := domain.Session{PluginID: "claude", SessionID: "s1", Fingerprint: "fp1"}
	if err := q.Add(summary.Request{PluginID: "claude", SessionID: "s1", Batches: [][]int{{1}}, Prompts: []string{"doc"}}); err != nil {
		t.Fatal(err)
	}
	// queueOne says false here (already queued), which must not end it.
	if queued, _ := queueOne(ctx, nil, d, q, s, time.Hour); queued {
		t.Fatal("a queued session was queued again")
	}
	r, ok := q.Find("claude", "s1")
	if !ok {
		t.Fatal("the request went missing")
	}
	if !r.Ready(time.Now()) {
		t.Error("a fresh request is not ready")
	}
	if len(r.Prompts) != 1 {
		t.Errorf("the request lost its prompt: %+v", r.Prompts)
	}

	// One waiting out a failure is not acted on: retrying here would repeat the
	// failure on every run.
	if err := q.Retry(r, time.Now()); err != nil {
		t.Fatal(err)
	}
	r2, _ := q.Find("claude", "s1")
	if r2.Ready(time.Now()) {
		t.Error("a request inside its backoff is ready")
	}
}

// Noticing that a session has nothing new to summarize must not destroy the
// summary it already has. Turn 0 is upserted to carry the current fingerprint —
// that is the only cheap way to recognize a session as done — and writing a
// blank row there wipes text somebody paid for.
//
// It is reached the ordinary way: a summarized session grows by one turn that
// renders to no document (a command with no reply), so there is nothing to ask
// about and the marker is written.
func TestNoticingThereIsNothingToAddKeepsTheSummary(t *testing.T) {
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := domain.Session{PluginID: "claude", SessionID: "s1", Fingerprint: "fp1"}
	if err := d.PutSummaries(ctx, s, []cache.Summary{{Turn: 0, Text: "一日かけて作った要約", Model: "claude claude-sonnet-5"}}); err != nil {
		t.Fatal(err)
	}
	grown := s
	grown.Fingerprint = "fp2" // the log moved

	markNothingToSummarize(ctx, d, grown)

	got := d.Summaries(ctx, grown, nil)
	if got[0].Text != "一日かけて作った要約" {
		t.Fatalf("the session summary was destroyed by the marker: %q", got[0].Text)
	}
	if got[0].Model != "claude claude-sonnet-5" {
		t.Errorf("the marker lost which model wrote the summary: %q", got[0].Model)
	}
	// And it did what it is for: the session is not opened again at this version.
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	if worthOpening(ctx, d, q, grown, time.Hour) {
		t.Error("the marker did not record the version the log moved to")
	}
	// A session that never had one still gets the blank record.
	fresh := domain.Session{PluginID: "claude", SessionID: "s2", Fingerprint: "fp1"}
	markNothingToSummarize(ctx, d, fresh)
	if worthOpening(ctx, d, q, fresh, time.Hour) {
		t.Error("a session with nothing to summarize would be opened again")
	}
	if sums, _, _ := storedSummaries(ctx, d, fresh, nil, 0); len(sums) != 0 {
		t.Errorf("the blank record reads back as a summary: %v", sums)
	}
	// And it does not read as one to what paces the session summary. A session
	// being worked in reaches the marker often, so a marker that counted as
	// summarizing would push its next session summary away every time anything
	// looked at it.
	if when, ok := d.SummarizedAt(ctx, fresh); ok {
		t.Errorf("noticing there was nothing to add recorded the session as summarized at %s", when)
	}
}

// The turn an agent is writing is not summarizable, so a session whose finished
// turns are all described has nothing to ask about — every scan, for as long as
// someone works in it. That must not count as summarizing it: what paces the
// session summary reads the same time.
func TestASessionWaitingOnAnOpenTurnIsNotHeldOffForAnHour(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	src := "/logs/open.jsonl"
	// One turn, and the agent is still writing it.
	open := domain.Session{
		PluginID: "claude", AgentType: "claude", SessionID: "live", CWD: "/repo/app", Title: "live",
		UpdatedAt: time.Now(), LastKind: domain.EventStream,
		Fingerprint: "fp1", SourceRef: domain.SessionRef{Source: src},
	}
	conv := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Now(), Events: talk("いま聞いていること", "書きかけの答え")},
	})
	a, cfg := fixtureApp([]domain.Session{open}, map[string]domain.Conversation{src: conv})
	cfg.Summary.Agent, cfg.Summary.MaxPerRun = "claude", 4

	EnqueueSummaries(ctx, a, cfg, d, []domain.Session{open})

	if reqs, _ := q.List(); len(reqs) != 0 {
		t.Fatalf("the turn being written was queued: %v", reqs)
	}
	if _, ok := d.SummarizedAt(ctx, open); ok {
		t.Error("a session that could not be summarized was recorded as summarized")
	}
	// And it stays a session worth looking at. Recording the version here would
	// say the log has been answered, and the turn being written would never be
	// summarized once it ended — see
	// TestATurnInsideTheSettleWindowIsNotWrittenOff. The price is a parse per
	// scan while the turn is open.
	if !worthOpening(ctx, d, q, open, time.Hour) {
		t.Error("the session was written off while its turn was still being written")
	}
	// The turn finishes. Nothing holds the session back now — least of all a
	// guard armed by the scan that found the turn unfinished.
	done := open
	done.Fingerprint, done.LastKind = "fp2", domain.EventTurnComplete
	done.UpdatedAt = time.Now().Add(-2 * time.Minute) // and it stayed finished
	EnqueueSummaries(ctx, a, cfg, d, []domain.Session{done})
	if _, queued := q.Find("claude", "live"); !queued {
		t.Error("the finished turn was not queued")
	}
}

// A request carries the nodes of the turns it can be about, which leaves out the
// one being written. That count is how a run tells whether a single call covered
// the session: measured against every turn instead, a session with a turn in
// progress would never look covered, and the run would pay for a second call to
// remake a session summary the first call had just made.
func TestAQueuedRequestLeavesTheOpenTurnOutOfItsNodes(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	src := "/logs/live.jsonl"
	conv := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: now, Events: talk("終わった質問", "終わった答え")},
		{ID: "u2", Parent: "u1", Timestamp: now, Events: talk("いま聞いていること", "書きかけ")},
	})
	s := domain.Session{
		PluginID: "claude", AgentType: "claude", SessionID: "live", CWD: "/repo/app", Title: "live",
		UpdatedAt: now, LastKind: domain.EventStream, // the second turn is being written
		Fingerprint: "fp1", SourceRef: domain.SessionRef{Source: src},
	}
	a, cfg := fixtureApp([]domain.Session{s}, map[string]domain.Conversation{src: conv})
	cfg.Summary.Agent, cfg.Summary.MaxPerRun = "claude", 4

	EnqueueSummaries(ctx, a, cfg, d, []domain.Session{s})

	r, ok := q.Find("claude", "live")
	if !ok {
		t.Fatal("the finished turn was not queued")
	}
	if len(r.Nodes) != 1 || r.Nodes[1] == "" {
		t.Fatalf("nodes=%v, want only the finished turn — the open one is not summarizable", r.Nodes)
	}
	// So a single call asking about that turn covers everything this request is
	// about, and its session summary can be used as it stands.
	asked := 0
	for _, b := range r.Batches {
		asked += len(b)
	}
	if len(r.Prompts) != 1 || asked != len(r.Nodes) {
		t.Errorf("prompts=%d asked=%d nodes=%d, want one call covering every summarizable turn", len(r.Prompts), asked, len(r.Nodes))
	}
}

// A turn that ended a moment ago is not summarized yet — it may not have ended,
// since end_turn is written per block — but it must not be written off either.
//
// Recording the version says "this log has been answered", and worthOpening then
// leaves the session alone until the log moves. A log that just ended has no
// reason to move again: the turn would never be summarized. So while the window
// is open, the session is left as unanswered.
func TestATurnInsideTheSettleWindowIsNotWrittenOff(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	src := "/logs/justended.jsonl"
	conv := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: now, Events: talk("最初の質問", "最初の答え")},
		{ID: "u2", Parent: "u1", Timestamp: now, Events: talk("次の質問", "たった今終わった答え")},
	})
	// Turn 1 is described; turn 2 ended seconds ago and is all that is left.
	done := domain.Session{PluginID: "claude", SessionID: "justended", Fingerprint: "fp1"}
	if err := d.PutSummaries(ctx, done, []cache.Summary{
		{Turn: 0, Text: "セッション要約", Model: "m"},
		{Turn: 1, NodeID: "u1", Text: "ターン1", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	s := domain.Session{
		PluginID: "claude", AgentType: "claude", SessionID: "justended", CWD: "/repo/app", Title: "justended",
		UpdatedAt: now, LastKind: domain.EventTurnComplete, // inside settleAfterComplete
		Fingerprint: "fp2", SourceRef: domain.SessionRef{Source: src},
	}
	a, cfg := fixtureApp([]domain.Session{s}, map[string]domain.Conversation{src: conv})
	cfg.Summary.Agent, cfg.Summary.MaxPerRun = "claude", 4

	EnqueueSummaries(ctx, a, cfg, d, []domain.Session{s})

	if reqs, _ := q.List(); len(reqs) != 0 {
		t.Fatalf("a turn that ended seconds ago was queued: %v", reqs)
	}
	if !worthOpening(ctx, d, q, s, time.Hour) {
		t.Fatal("the session was written off as answered — nothing will look at it again, and the turn is lost")
	}
	// Once the window passes, the same session queues that turn.
	settled := s
	settled.UpdatedAt = now.Add(-2 * time.Minute)
	EnqueueSummaries(ctx, a, cfg, d, []domain.Session{settled})
	if _, ok := q.Find("claude", "justended"); !ok {
		t.Error("the turn was not queued once it had settled")
	}
}

// A session abandoned mid-turn — Ctrl+C, a crash, a laptop closed — is
// summarized in full once it has sat still. Its log never moves again, so
// waiting for the turn to finish means never summarizing it: the fingerprint
// stays put and nothing opens the session a second time.
//
// settleBefore did this for the whole session; the window now applies to the one
// turn it is about, so a session's finished turns never wait for it.
func TestASessionAbandonedMidTurnIsSummarizedOnceItHasSatStill(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	src := "/logs/abandoned.jsonl"
	s := domain.Session{
		PluginID: "claude", AgentType: "claude", SessionID: "abandoned", CWD: "/repo/app", Title: "abandoned",
		// Stopped mid-turn two days ago: nobody is writing this.
		UpdatedAt: time.Now().Add(-48 * time.Hour), LastKind: domain.EventStream,
		Fingerprint: "fp1", SourceRef: domain.SessionRef{Source: src},
	}
	conv := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Now().Add(-48 * time.Hour), Events: talk("中断された作業", "途中で切れた答え")},
	})
	a, cfg := fixtureApp([]domain.Session{s}, map[string]domain.Conversation{src: conv})
	cfg.Summary.Agent, cfg.Summary.MaxPerRun = "claude", 4

	EnqueueSummaries(ctx, a, cfg, d, []domain.Session{s})

	r, ok := q.Find("claude", "abandoned")
	if !ok {
		t.Fatal("a session abandoned mid-turn was never queued — its last turn is lost for good")
	}
	var asked []int
	for _, b := range r.Batches {
		asked = append(asked, b...)
	}
	if len(asked) != 1 || asked[0] != 1 {
		t.Errorf("queued turns %v, want the abandoned turn", asked)
	}
}

// A plugin need not report what its log ends with, and a session whose LastKind
// is unset must still be summarized in full. plugin-copilot reads exported
// VS Code chat files and never sets it; read as "mid-turn", every one of those
// sessions would be queued with its newest turn held back — and nothing would
// ever ask for that turn again, because a static file does not grow.
func TestASessionWhosePluginReportsNoLastKindIsSummarizedInFull(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	src := "/logs/exported.json"
	s := domain.Session{
		PluginID: "claude", AgentType: "claude", SessionID: "exported", CWD: "/repo/app", Title: "exported",
		UpdatedAt: time.Now(), Fingerprint: "fp1", SourceRef: domain.SessionRef{Source: src},
		// LastKind deliberately unset: the plugin does not report one.
	}
	conv := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Now(), Events: talk("書き出されたやりとり", "その答え")},
	})
	a, cfg := fixtureApp([]domain.Session{s}, map[string]domain.Conversation{src: conv})
	cfg.Summary.Agent, cfg.Summary.MaxPerRun = "claude", 4

	EnqueueSummaries(ctx, a, cfg, d, []domain.Session{s})

	r, ok := q.Find("claude", "exported")
	if !ok {
		t.Fatal("a session with no reported last kind was not queued at all")
	}
	var asked []int
	for _, b := range r.Batches {
		asked = append(asked, b...)
	}
	if len(asked) != 1 || asked[0] != 1 {
		t.Errorf("queued turns %v, want the session's one and only turn", asked)
	}
}

// The turn between the two ends passes on an open, not on a candidate. An end
// whose first sessions are already summarized costs a row lookup each to walk
// past, and must not spend its half of the budget doing so — otherwise the half
// meant for the old sessions is quietly handed to the new ones.
func TestTheSplitTurnsOnAnOpenNotACandidate(t *testing.T) {
	_, _ = summaryFixture(t)
	ctx := context.Background()
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a, cfg := settledAppN(10)
	cfg.Summary.Agent, cfg.Summary.MaxPerRun = "claude", 2

	// s6…s9 — the four oldest — are summarized already. The oldest session still
	// worth opening is s5.
	scanned := app.FilterSessions(scanSessions(ctx, a, cfg, d), app.SessionFilter{})
	by := map[string]domain.Session{}
	for _, x := range scanned {
		by[x.SessionID] = x
	}
	for _, id := range []string{"s6", "s7", "s8", "s9"} {
		if err := d.PutSummaries(ctx, by[id], []cache.Summary{{Turn: 0, Text: "done"}}); err != nil {
			t.Fatal(err)
		}
	}
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	reqs, _ := q.List()
	for _, r := range reqs {
		if err := q.Done(r); err != nil {
			t.Fatal(err)
		}
	}

	EnqueueSummaries(ctx, a, cfg, d, scanned)

	reqs, _ = q.List()
	var ids []string
	for _, r := range reqs {
		ids = append(ids, r.SessionID)
	}
	sort.Strings(ids)
	// One from each end: the newest, and the oldest that still needs opening.
	// Turning on a candidate instead would have spent both on the new end, since
	// the four skips at the old end would each have cost it a turn.
	if len(ids) != 2 || ids[0] != "s0" || ids[1] != "s5" {
		t.Fatalf("queued %v, want [s0 s5] — one end each, with the skips costing neither", ids)
	}
}
