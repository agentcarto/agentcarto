package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/core/domain"
)

// Choosing wrongly costs money, so the choosing is deliberate: newest first,
// nothing still being worked in, and never more than the worker will take.
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
	got := pickForSummary(sessions, now, 0)
	var ids []string
	for _, s := range got {
		ids = append(ids, s.SessionID)
	}
	// just-finished is the most recent of the eligible ones: its last turn is
	// complete, so there is nothing in flight to pay for twice.
	want := []string{"just-finished", "newest", "older", "oldest"}
	if len(ids) != len(want) {
		t.Fatalf("picked %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("picked %v, want %v (newest first)", ids, want)
		}
	}
}

// A first run over a whole machine must spend a bounded amount, not everything
// at once. The cap is the same one the worker stops at, so nothing is queued
// that will only sit there.
func TestPickForSummaryRespectsTheCap(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	var sessions []domain.Session
	for i := range 50 {
		sessions = append(sessions, domain.Session{
			SessionID: string(rune('a' + i%26)),
			UpdatedAt: now.Add(-time.Duration(i+1) * time.Hour),
		})
	}
	if got := pickForSummary(sessions, now, 20); len(got) != 20 {
		t.Fatalf("picked %d, want the cap of 20", len(got))
	}
	// The cap keeps the newest, since those are the ones a reader comes back to.
	got := pickForSummary(sessions, now, 3)
	if len(got) != 3 || !got[0].UpdatedAt.After(got[2].UpdatedAt) {
		t.Fatalf("the cap did not keep the newest: %v", got)
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
		if got := pickForSummary([]domain.Session{s}, now, 0); len(got) != 0 {
			t.Errorf("%s was picked", s.SessionID)
		}
	}
	// Once it has settled, it is picked even mid-turn: at that age the turn was
	// abandoned rather than being written.
	s := domain.Session{SessionID: "settled", UpdatedAt: now.Add(-settleBefore - time.Second), LastKind: domain.EventStream}
	if got := pickForSummary([]domain.Session{s}, now, 0); len(got) != 1 {
		t.Error("a settled session was not picked")
	}
	// A finished turn waits for nothing. Most sessions on a machine are in this
	// state, so making them wait ten minutes delayed nearly everything.
	fresh := domain.Session{SessionID: "finished", UpdatedAt: now.Add(-time.Second), LastKind: domain.EventTurnComplete}
	if got := pickForSummary([]domain.Session{fresh}, now, 0); len(got) != 1 {
		t.Error("a session whose last turn completed was made to wait")
	}
}

func TestPickForSummaryOnNothing(t *testing.T) {
	if got := pickForSummary(nil, time.Now(), 20); len(got) != 0 {
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
