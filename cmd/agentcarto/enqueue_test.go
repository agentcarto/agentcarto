package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/core/domain"
)

// Choosing wrongly costs money, so the choosing is deliberate: oldest first and
// nothing still being worked in. The per-run budget is not applied here — see
// TestTheSweepAdvancesPastWhatIsAlreadyDone.
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
	got := pickForSummary(sessions, now)
	var ids []string
	for _, s := range got {
		ids = append(ids, s.SessionID)
	}
	// Oldest first: those are the ones whose logs are about to be rotated away,
	// and a session with no log cannot be summarized at all. just-finished is
	// eligible despite its age — its last turn is complete, so there is nothing
	// in flight to pay for twice — but it goes last.
	want := []string{"oldest", "older", "newest", "just-finished"}
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
// The order is fixed (oldest first), so a budget applied to the candidate list
// before knowing which of them are already summarized picks the same sessions
// forever: the first max_per_run of them are done, nothing is queued, and the
// rest are never reached. The budget therefore counts the sessions a run opens.
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
	// The fixture's sessions are s0 (newest) … s5 (oldest), so the oldest two are
	// s5 and s4.
	sessions := scanSessions(ctx, a, cfg, d)
	if got := queued(); len(got) != 2 || got[0] != "s4" || got[1] != "s5" {
		t.Fatalf("the first run queued %v, want the oldest two [s4 s5]", got)
	}
	// They are summarized and leave the queue, as a worker would leave them.
	for _, id := range []string{"s4", "s5"} {
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
	// The next run must reach the two after them, not pick s4 and s5 again.
	scanSessions(ctx, a, cfg, d)
	if got := queued(); len(got) != 2 || got[0] != "s2" || got[1] != "s3" {
		t.Fatalf("the second run queued %v, want the next two [s2 s3] — the sweep stalled", got)
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
	if queueOne(ctx, a, d, q, s) {
		t.Fatal("a session with no turns was queued")
	}
	if worthOpening(ctx, d, q, s) {
		t.Error("the session would be opened again on the next run")
	}
	// The record must not show up as a summary: it has no text, and every reader
	// tests for that before printing.
	if sums, model := storedSummaries(ctx, d, s, nil); len(sums) != 0 || model != "" {
		t.Errorf("the blank record reads back as a summary: %v %q", sums, model)
	}
	// A log that grew is reconsidered: the fingerprint no longer matches.
	grown := s
	grown.Fingerprint = "fp2"
	if !worthOpening(ctx, d, q, grown) {
		t.Error("a session that grew was still treated as having nothing to summarize")
	}
}

// A session being worked in right now would be summarized and then immediately
// outgrow it — paid for, and out of date before the call returned.
func TestPickForSummarySkipsWhatIsStillMoving(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	for _, s := range []domain.Session{
		// Running: an agent is in it right now, whatever the log says.
		{SessionID: "running", UpdatedAt: now.Add(-time.Hour), Status: domain.StatusRunning, LastKind: domain.EventTurnComplete},
		// Mid-turn and recent: the turn being written would change under the
		// summary, and the store would withhold what was paid for.
		{SessionID: "seconds-ago", UpdatedAt: now.Add(-time.Second), LastKind: domain.EventStream},
		{SessionID: "just-inside", UpdatedAt: now.Add(-settleBefore + time.Second), LastKind: domain.EventToolCall},
	} {
		if got := pickForSummary([]domain.Session{s}, now); len(got) != 0 {
			t.Errorf("%s was picked", s.SessionID)
		}
	}
	// Once it has settled, it is picked even mid-turn: at that age the turn was
	// abandoned rather than being written.
	s := domain.Session{SessionID: "settled", UpdatedAt: now.Add(-settleBefore - time.Second), LastKind: domain.EventStream}
	if got := pickForSummary([]domain.Session{s}, now); len(got) != 1 {
		t.Error("a settled session was not picked")
	}
	// A finished turn waits for nothing. Most sessions on a machine are in this
	// state, so making them wait ten minutes delayed nearly everything.
	fresh := domain.Session{SessionID: "finished", UpdatedAt: now.Add(-time.Second), LastKind: domain.EventTurnComplete}
	if got := pickForSummary([]domain.Session{fresh}, now); len(got) != 1 {
		t.Error("a session whose last turn completed was made to wait")
	}
}

func TestPickForSummaryOnNothing(t *testing.T) {
	if got := pickForSummary(nil, time.Now()); len(got) != 0 {
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
		if queueOne(ctx, nil, d, q, s) {
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
		if queueOne(ctx, nil, d, q, s) {
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
		queueOne(ctx, nil, d, q, grown)
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

// settledAppN serves n sessions whose last turn is complete, so that none is
// held back by settleBefore and what a test sees is the queueing itself. They
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
	if queueOne(ctx, nil, d, q, s) {
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
