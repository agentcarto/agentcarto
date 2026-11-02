package summary

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

// The record lives beside the requests, and a file there that reads as a request
// is one Sweep throws away and a worker tries to run.
func TestWorkingIsNotARequest(t *testing.T) {
	q := tempQueue(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: now}
	if err := q.MarkWorking(r, 2, 3, now); err != nil {
		t.Fatal(err)
	}

	if reqs, bad := q.List(); len(reqs) != 0 || len(bad) != 0 {
		t.Errorf("List sees the record: requests=%v unreadable=%v", reqs, bad)
	}
	if n := q.Sweep(time.Hour, now); n != 0 {
		t.Errorf("Sweep removed %d files", n)
	}
	got, ok := q.readWorking()
	if !ok {
		t.Fatal("the record did not survive a sweep")
	}
	if got.PluginID != "claude" || got.SessionID != "s1" || got.Call != 2 || got.Calls != 3 || !got.Started.Equal(now) {
		t.Errorf("the record reads back as %+v", got)
	}
}

func TestClearWorkingRemovesTheRecord(t *testing.T) {
	q := tempQueue(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if err := q.MarkWorking(Request{PluginID: "claude", SessionID: "s1"}, 1, 1, now); err != nil {
		t.Fatal(err)
	}
	if err := q.ClearWorking(); err != nil {
		t.Fatal(err)
	}
	if _, ok := q.readWorking(); ok {
		t.Error("the record is still there")
	}
	// A worker clears on the way in and on the way out, and there is usually
	// nothing there the first time.
	if err := q.ClearWorking(); err != nil {
		t.Errorf("clearing with nothing there: %v", err)
	}
}

func TestAnUnreadableWorkingRecordReadsAsNone(t *testing.T) {
	q := tempQueue(t)
	if err := os.WriteFile(filepath.Join(q.dir, workingName), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, ok := q.readWorking(); ok {
		t.Errorf("got %+v", got)
	}
}

// statusFixture is a queue and a lock path that nothing holds yet.
func statusFixture(t *testing.T) (*Queue, string, *StatusReader) {
	t.Helper()
	q := tempQueue(t)
	lock := filepath.Join(t.TempDir(), "summarize.lock")
	return q, lock, NewStatusReader(q, lock)
}

// holdLock takes the worker lock for the length of the test, as a running worker
// holds it.
func holdLock(t *testing.T, path string) {
	t.Helper()
	l, err := TakeLock(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Release() })
}

func TestStatusOfAnEmptyQueueIsIdle(t *testing.T) {
	_, _, sr := statusFixture(t)
	st := sr.Read(time.Now())
	if !st.Enabled {
		t.Error("an empty queue reads as unknown, not idle")
	}
	if st.Running || st.Queued != 0 || st.Failed != 0 || len(st.Sessions) != 0 {
		t.Errorf("status=%+v", st)
	}
}

func TestStatusNamesWhatEachSessionIsWaitingFor(t *testing.T) {
	q, _, sr := statusFixture(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	add := func(r Request) {
		t.Helper()
		r.PluginID, r.Queued = "claude", now.Add(-48*time.Hour)
		if err := q.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	add(Request{SessionID: "fresh"})
	add(Request{SessionID: "backing-off", Attempts: 2, LastTried: now.Add(-time.Hour), LastError: "limit"})
	add(Request{SessionID: "back-off-over", Attempts: 1, LastTried: now.Add(-RetryAfter), LastError: "limit"})
	add(Request{SessionID: "out-of-attempts", Attempts: MaxAttempts, LastTried: now.Add(-30 * 24 * time.Hour), LastError: "limit"})

	st := sr.Read(now)

	if st.Queued != 2 || st.Failed != 2 {
		t.Errorf("queued=%d failed=%d, want 2 and 2", st.Queued, st.Failed)
	}
	get := func(id string) SessionStatus {
		return st.Sessions[domain.SessionKey{PluginID: "claude", SessionID: id}]
	}
	if s := get("fresh"); s.State != SessionQueued || s.Attempts != 0 {
		t.Errorf("fresh=%+v", s)
	}
	if s := get("backing-off"); s.State != SessionFailed || !s.RetryAt.Equal(now.Add(RetryAfter-time.Hour)) || s.LastError != "limit" {
		t.Errorf("backing-off=%+v", s)
	}
	// Ready again, so it is waiting for a worker like any other, and still says it
	// failed before.
	if s := get("back-off-over"); s.State != SessionQueued || s.Attempts != 1 || s.LastError != "limit" {
		t.Errorf("back-off-over=%+v", s)
	}
	if s := get("out-of-attempts"); s.State != SessionFailed || !s.RetryAt.IsZero() {
		t.Errorf("out-of-attempts=%+v", s)
	}
}

func TestStatusCountsTheSessionInHandOnlyAsThat(t *testing.T) {
	cases := []struct {
		name    string
		request Request
	}{
		{"a first try", Request{}},
		{"a retry", Request{Attempts: 1, LastTried: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), LastError: "limit"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, lock, sr := statusFixture(t)
			holdLock(t, lock)
			now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
			r := c.request
			r.PluginID, r.SessionID, r.Queued = "claude", "s1", now
			if err := q.Add(r); err != nil {
				t.Fatal(err)
			}
			if err := q.Add(Request{PluginID: "claude", SessionID: "behind", Queued: now}); err != nil {
				t.Fatal(err)
			}
			if err := q.MarkWorking(r, 2, 3, now); err != nil {
				t.Fatal(err)
			}

			st := sr.Read(now.Add(time.Minute))

			key := domain.SessionKey{PluginID: "claude", SessionID: "s1"}
			if st.Current != key {
				t.Errorf("Current=%+v", st.Current)
			}
			if st.Queued != 1 || st.Failed != 0 {
				t.Errorf("queued=%d failed=%d, want only the session behind it", st.Queued, st.Failed)
			}
			s := st.Sessions[key]
			if s.State != SessionWorking || s.Call != 2 || s.Calls != 3 || !s.CallStarted.Equal(now) {
				t.Errorf("session=%+v", s)
			}
			// A retry in hand still says what went wrong last time, until this call
			// settles it one way or the other.
			if s.Attempts != r.Attempts || s.LastError != r.LastError {
				t.Errorf("attempts=%d lastError=%q, want %d and %q", s.Attempts, s.LastError, r.Attempts, r.LastError)
			}
		})
	}
}

// The request can be gone while the worker is still on that session: `show` takes
// a request out of the queue before generating. The record still says what the
// worker is doing, and a count must not go negative for want of a request.
func TestStatusNamesASessionInHandWithNoRequest(t *testing.T) {
	q, lock, sr := statusFixture(t)
	holdLock(t, lock)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if err := q.MarkWorking(Request{PluginID: "claude", SessionID: "s1"}, 0, 0, now); err != nil {
		t.Fatal(err)
	}
	st := sr.Read(now)
	if s := st.Sessions[st.Current]; s.State != SessionWorking || st.Queued != 0 || st.Failed != 0 {
		t.Errorf("status=%+v session=%+v", st, s)
	}
}

// A worker that was killed takes neither its lock file nor its record with it. Read
// as live, it would say a session is being summarized for as long as nothing else
// starts.
func TestStatusIgnoresTheRecordOfAWorkerThatIsGone(t *testing.T) {
	q, lock, sr := statusFixture(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: now}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkWorking(r, 1, 1, now); err != nil {
		t.Fatal(err)
	}

	check := func(name string) {
		t.Helper()
		st := sr.Read(now)
		if st.Running || st.Current != (domain.SessionKey{}) {
			t.Errorf("%s: running=%v current=%+v", name, st.Running, st.Current)
		}
		if st.Queued != 1 {
			t.Errorf("%s: queued=%d, the request is still waiting", name, st.Queued)
		}
	}
	check("no lock file")
	writeDeadLock(t, lock)
	check("a dead pid")
}

// writeDeadLock leaves the lock file of a worker that is no longer running.
func writeDeadLock(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("99999999 2026-08-24T12:00:00Z\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

// What the reader keeps must not be what it saw once: the worker rewrites requests
// and removes them while the TUI goes on looking.
func TestStatusFollowsTheQueueAsItChanges(t *testing.T) {
	q, _, sr := statusFixture(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: now, Prompts: []string{"doc"}}
	key := domain.SessionKey{PluginID: "claude", SessionID: "s1"}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	if st := sr.Read(now); st.Queued != 1 {
		t.Fatalf("first read: queued=%d", st.Queued)
	}

	if err := q.Retry(r, now, errors.New("limit")); err != nil {
		t.Fatal(err)
	}
	st := sr.Read(now)
	if s := st.Sessions[key]; s.State != SessionFailed || s.LastError != "limit" || st.Queued != 0 {
		t.Errorf("after a failure: status=%+v session=%+v", st, s)
	}

	if err := q.Done(r); err != nil {
		t.Fatal(err)
	}
	if st := sr.Read(now); len(st.Sessions) != 0 || st.Failed != 0 {
		t.Errorf("after the request left: %+v", st)
	}
}

func TestStatusCountsWhatDoesNotReadAsARequest(t *testing.T) {
	q, _, sr := statusFixture(t)
	if err := os.WriteFile(filepath.Join(q.dir, "garbage.json"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second look comes from what the first one kept
		if st := sr.Read(time.Now()); st.Unreadable != 1 || len(st.Sessions) != 0 {
			t.Errorf("status=%+v", st)
		}
	}
}

// The TUI's reads are not serialized: a scan can land while the last one's read is
// still going. Run under -race this is what catches an unguarded map.
func TestStatusCanBeReadConcurrently(t *testing.T) {
	q, _, sr := statusFixture(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if err := q.Add(Request{PluginID: "claude", SessionID: "s1", Queued: now}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sr.Read(now)
		}()
	}
	wg.Wait()
}

func benchmarkQueue(b *testing.B, requests, promptBytes int) *StatusReader {
	b.Helper()
	q, err := OpenQueue(filepath.Join(b.TempDir(), "queue"))
	if err != nil {
		b.Fatal(err)
	}
	prompt := strings.Repeat("x", promptBytes)
	for i := range requests {
		r := Request{PluginID: "claude", SessionID: fmt.Sprintf("s%d", i), Queued: time.Now(), Prompts: []string{prompt}}
		if err := q.Add(r); err != nil {
			b.Fatal(err)
		}
	}
	return NewStatusReader(q, filepath.Join(b.TempDir(), "summarize.lock"))
}

// BenchmarkStatusFirstRead is what a look costs with nothing kept: every request
// decoded, which is also what every look would cost without the reader.
func BenchmarkStatusFirstRead(b *testing.B) {
	sr := benchmarkQueue(b, 500, 200<<10)
	for b.Loop() {
		sr.seen = nil
		sr.Read(time.Now())
	}
}

// BenchmarkStatusLaterRead is what a look costs when nothing in the queue changed.
func BenchmarkStatusLaterRead(b *testing.B) {
	sr := benchmarkQueue(b, 500, 200<<10)
	sr.Read(time.Now())
	for b.Loop() {
		sr.Read(time.Now())
	}
}
