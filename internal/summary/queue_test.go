package summary

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func tempQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := OpenQueue(filepath.Join(t.TempDir(), "queue"))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestQueueRoundTrip(t *testing.T) {
	q := tempQueue(t)
	in := Request{
		PluginID: "claude", SessionID: "s1", Queued: time.Unix(100, 0).UTC(),
		Nodes: map[int]string{1: "n1"}, Batches: [][]int{{1, 2}}, Prompts: []string{"doc"},
	}
	if err := q.Add(in); err != nil {
		t.Fatal(err)
	}
	got, bad := q.List()
	if len(bad) != 0 {
		t.Errorf("unreadable files: %v", bad)
	}
	if len(got) != 1 {
		t.Fatalf("listed %d requests, want 1", len(got))
	}
	if got[0].SessionID != "s1" || got[0].Nodes[1] != "n1" || got[0].Prompts[0] != "doc" {
		t.Errorf("read back %+v", got[0])
	}
	if err := q.Done(got[0]); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.List(); len(got) != 0 {
		t.Errorf("the request survived Done: %+v", got)
	}
	// Removing what is not there is not an error: two workers may finish the
	// same request in a race that the lock is meant to prevent but cannot
	// guarantee across machines sharing a directory.
	if err := q.Done(in); err != nil {
		t.Errorf("Done on a missing request: %v", err)
	}
}

// A session queued twice is one request. Every scan queues what has no summary,
// so without this a session would accumulate a request per scan.
func TestQueueReplacesBySession(t *testing.T) {
	q := tempQueue(t)
	for i, p := range []string{"first", "second"} {
		if err := q.Add(Request{PluginID: "claude", SessionID: "s1", Queued: time.Unix(int64(i), 0), Prompts: []string{p}}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := q.List()
	if len(got) != 1 || got[0].Prompts[0] != "second" {
		t.Fatalf("listed %+v, want one request holding the later prompt", got)
	}
}

// Different sessions and different agents keep their own requests.
func TestQueueKeepsSessionsApart(t *testing.T) {
	q := tempQueue(t)
	for _, r := range []Request{
		{PluginID: "claude", SessionID: "a"},
		{PluginID: "claude", SessionID: "b"},
		{PluginID: "codex", SessionID: "a"},
	} {
		if err := q.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := q.List(); len(got) != 3 {
		t.Fatalf("listed %d requests, want 3", len(got))
	}
}

// The worker takes the oldest first, so a session queued while a long one is
// being summarized does not jump ahead of it.
func TestQueueListsOldestFirst(t *testing.T) {
	q := tempQueue(t)
	for _, n := range []int64{300, 100, 200} {
		if err := q.Add(Request{PluginID: "claude", SessionID: "s" + time.Unix(n, 0).UTC().Format("05"), Queued: time.Unix(n, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := q.List()
	if len(got) != 3 {
		t.Fatalf("listed %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Queued.Before(got[i-1].Queued) {
			t.Fatalf("out of order: %v", []time.Time{got[0].Queued, got[1].Queued, got[2].Queued})
		}
	}
}

// A file that is not a request must not stop the worker forever. It is reported
// so the caller can remove it.
func TestQueueReportsWhatItCannotRead(t *testing.T) {
	q := tempQueue(t)
	if err := q.Add(Request{PluginID: "claude", SessionID: "good", Queued: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(q.dir, "claude-broken.json")
	if err := os.WriteFile(junk, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	got, bad := q.List()
	if len(got) != 1 || got[0].SessionID != "good" {
		t.Errorf("the readable request did not survive: %+v", got)
	}
	if len(bad) != 1 || bad[0] != junk {
		t.Errorf("bad=%v want the broken file", bad)
	}
}

// A request that has waited days describes a session that has almost certainly
// changed. Summarizing it would pay for a prompt built from a conversation that
// no longer looks like that.
func TestQueueSweepsWhatWentStale(t *testing.T) {
	q := tempQueue(t)
	now := time.Unix(1_000_000, 0)
	if err := q.Add(Request{PluginID: "claude", SessionID: "old", Queued: now.Add(-72 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := q.Add(Request{PluginID: "claude", SessionID: "fresh", Queued: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, "claude-junk.json"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if n := q.Sweep(48*time.Hour, now); n != 2 {
		t.Errorf("Sweep removed %d, want 2 (the stale request and the junk)", n)
	}
	got, bad := q.List()
	if len(got) != 1 || got[0].SessionID != "fresh" {
		t.Errorf("after the sweep: %+v", got)
	}
	if len(bad) != 0 {
		t.Errorf("junk survived: %v", bad)
	}
}

// A request carries a session's text, so neither the directory nor the files
// are readable by anyone else.
func TestQueueIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		// No Unix permission bits there: os.Chmod sets only the read-only flag,
		// and who may read a file is decided by the directory's ACL.
		t.Skip("permission bits are a Unix notion")
	}
	q := tempQueue(t)
	if err := q.Add(Request{PluginID: "claude", SessionID: "s1", Prompts: []string{"secret"}}); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(q.dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := di.Mode().Perm(); p&0o077 != 0 {
		t.Errorf("queue directory is %o, want no group or other access", p)
	}
	fi, err := os.Stat(filepath.Join(q.dir, requestName("claude", "s1")))
	if err != nil {
		t.Fatal(err)
	}
	if p := fi.Mode().Perm(); p&0o077 != 0 {
		t.Errorf("queued request is %o, want no group or other access", p)
	}
}

// Session ids come from plugins. A name that walked out of the directory would
// let one write anywhere the user can.
func TestRequestNameStaysInTheDirectory(t *testing.T) {
	for _, id := range []string{"../escape", "a/b", "..", "with space", "sub/../../x"} {
		got := requestName("claude", id)
		if strings.ContainsAny(got, `/\`) || strings.Contains(got, "..") {
			t.Errorf("requestName(%q) = %q, which leaves the directory", id, got)
		}
	}
	// A real session id is unchanged.
	if got := requestName("claude", "f64330cd-b244-4e5d-ae18-9ae38d1fccf1"); got != "claude-f64330cd-b244-4e5d-ae18-9ae38d1fccf1.json" {
		t.Errorf("requestName mangled a normal id: %q", got)
	}
}

// A session that cannot be summarized must not be retried on every scan. The
// guard in the worker reads the store, and a session that never succeeded has
// nothing there — so the backoff has to live with the request.
func TestRequestReady(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		r    Request
		want bool
	}{
		{"never tried", Request{}, true},
		{"failed just now", Request{Attempts: 1, LastTried: now.Add(-time.Minute)}, false},
		{"failed inside the backoff", Request{Attempts: 1, LastTried: now.Add(-RetryAfter + time.Minute)}, false},
		{"failed before the backoff", Request{Attempts: 1, LastTried: now.Add(-RetryAfter)}, true},
		{"failed long ago", Request{Attempts: 2, LastTried: now.Add(-30 * 24 * time.Hour)}, true},
		{"out of attempts", Request{Attempts: MaxAttempts, LastTried: now.Add(-30 * 24 * time.Hour)}, false},
		{"past the ceiling", Request{Attempts: MaxAttempts + 5, LastTried: time.Time{}}, false},
	}
	for _, c := range cases {
		if got := c.r.Ready(now); got != c.want {
			t.Errorf("%s: Ready=%v want %v", c.name, got, c.want)
		}
	}
}

func TestQueueRetryRecordsTheFailure(t *testing.T) {
	q := tempQueue(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: now.Add(-time.Hour), Prompts: []string{"doc"}}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	got, _ := q.List()
	if !got[0].Ready(now) {
		t.Fatal("a fresh request is not ready")
	}
	if err := q.Retry(got[0], now, errors.New("claude: exit status 1:\n  limit reached")); err != nil {
		t.Fatal(err)
	}
	got, _ = q.List()
	if len(got) != 1 {
		t.Fatalf("Retry did not put the request back: %+v", got)
	}
	if got[0].Attempts != 1 || !got[0].LastTried.Equal(now) {
		t.Errorf("the failure was not recorded: attempts=%d lastTried=%s", got[0].Attempts, got[0].LastTried)
	}
	if want := "claude: exit status 1: limit reached"; got[0].LastError != want {
		t.Errorf("LastError=%q want %q: it is read on one line", got[0].LastError, want)
	}
	if got[0].Ready(now) {
		t.Error("a request that just failed is ready again immediately")
	}
	// The prompt survives, so the retry does not have to rebuild it.
	if len(got[0].Prompts) != 1 || got[0].Prompts[0] != "doc" {
		t.Errorf("Retry lost the prompt: %+v", got[0].Prompts)
	}
}

// A reason quoting a whole answer would otherwise travel with the request and be
// read back on every look at the queue. The cut counts characters, so a reason
// in Japanese is not split in the middle of one.
func TestQueueRetryBoundsTheReason(t *testing.T) {
	q := tempQueue(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: now}
	if err := q.Retry(r, now, errors.New(strings.Repeat("要約", maxLastError))); err != nil {
		t.Fatal(err)
	}
	got, ok := q.Find("claude", "s1")
	if !ok {
		t.Fatal("Retry did not put the request back")
	}
	if !utf8.ValidString(got.LastError) {
		t.Errorf("the cut split a character: %q", got.LastError)
	}
	if n := utf8.RuneCountInString(got.LastError); n > maxLastError+1 {
		t.Errorf("LastError is %d runes, want at most %d and the mark", n, maxLastError)
	}
}

// A reason ends up drawn on a terminal, and a CLI can color what it prints. Left
// in, a color runs on into whatever is drawn after the reason, and a clear-screen
// sequence takes the whole screen with it.
func TestQueueRetryKeepsOnlyPrintableText(t *testing.T) {
	q := tempQueue(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: now}
	reason := errors.New("codex: \x1b[31mnot signed in\x1b[0m\x07\x1b[2J\r\nrun 要約\tagain")
	if err := q.Retry(r, now, reason); err != nil {
		t.Fatal(err)
	}
	got, ok := q.Find("claude", "s1")
	if !ok {
		t.Fatal("Retry did not put the request back")
	}
	if want := "codex: not signed in run 要約 again"; got.LastError != want {
		t.Errorf("LastError=%q want %q", got.LastError, want)
	}
}

// A request that failed for good sits out GiveUpFor, however long ago it was queued:
// the ordinary age limit would have the next scan queue it again within two days,
// which is the loop this exists to end. Only then does it go.
func TestQueueSweepsWhatGaveUpOnceItsWaitIsOver(t *testing.T) {
	q := tempQueue(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	for _, r := range []Request{
		{SessionID: "wait-over", Queued: old, Attempts: MaxAttempts, LastTried: now.Add(-GiveUpFor)},
		{SessionID: "still-waiting", Queued: old, Attempts: MaxAttempts, LastTried: now.Add(-GiveUpFor + time.Hour)},
		{SessionID: "still-trying", Queued: now, Attempts: 1, LastTried: now},
	} {
		r.PluginID = "claude"
		if err := q.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	if n := q.Sweep(48*time.Hour, now); n != 1 {
		t.Errorf("Sweep removed %d, want 1", n)
	}
	if _, ok := q.Find("claude", "wait-over"); ok {
		t.Error("a request past its wait is still there")
	}
	for _, id := range []string{"still-waiting", "still-trying"} {
		if _, ok := q.Find("claude", id); !ok {
			t.Errorf("%s was swept", id)
		}
	}
}

// A failure that cost nothing says nothing about the session. All 452 failures in
// one machine's log were of this kind, and every session they touched went on to
// succeed, so counting them would stop what recovers on its own.
func TestAFailureThatCostNothingIsNotCounted(t *testing.T) {
	q := tempQueue(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: now, Prompts: []string{"p"}, Batches: [][]int{{1}}, Nodes: map[int]string{1: "n"}}
	reason := fmt.Errorf("claude: %w", notBilled{errors.New("limit")})

	for range MaxAttempts + 2 {
		if err := q.Retry(r, now, reason); err != nil {
			t.Fatal(err)
		}
		r, _ = q.Find("claude", "s1")
	}

	if r.Attempts != 0 || r.GaveUp() {
		t.Errorf("attempts=%d gaveUp=%v, want it not counted", r.Attempts, r.GaveUp())
	}
	if !r.LastTried.Equal(now) || !strings.Contains(r.LastError, "limit") {
		t.Errorf("the failure was not recorded: %+v", r)
	}
	if r.Ready(now) || !r.Ready(now.Add(RetryAfter)) {
		t.Error("it does not wait out the backoff like any other failure")
	}
	if len(r.Prompts) != 1 {
		t.Error("the prompts it will need were dropped")
	}
}

// Giving up is on what has cost money. What is left is a record of why: the prompts
// hold a session's text, and nothing will read them again.
func TestARequestThatGivesUpKeepsOnlyTheRecord(t *testing.T) {
	q := tempQueue(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: now, Fingerprint: "fp", Attempts: MaxAttempts - 1, Prompts: []string{"p"}, Batches: [][]int{{1}}, Nodes: map[int]string{1: "n"}}

	if err := q.Retry(r, now, errors.New("the answer would not parse")); err != nil {
		t.Fatal(err)
	}

	got, ok := q.Find("claude", "s1")
	if !ok {
		t.Fatal("the record of giving up is gone")
	}
	if !got.GaveUp() || got.Ready(now.Add(365*24*time.Hour)) {
		t.Errorf("gaveUp=%v, and a record is never ready", got.GaveUp())
	}
	if got.LastError == "" || got.Fingerprint != "fp" {
		t.Errorf("the record lost what it is for: %+v", got)
	}
	if got.Prompts != nil || got.Batches != nil || got.Nodes != nil {
		t.Errorf("the session's text is still in the queue: %+v", got)
	}
}

// What ends a wait: its time being over, or the session having moved on, which makes
// it a different request about a different conversation.
func TestAGaveUpRecordStandsUntilTheWaitOrTheSessionMoves(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	gaveUp := Request{Fingerprint: "fp1", Attempts: MaxAttempts, LastTried: now.Add(-time.Hour)}
	cases := []struct {
		name        string
		r           Request
		fingerprint string
		want        bool
	}{
		{"same version, inside the wait", gaveUp, "fp1", false},
		{"the session moved", gaveUp, "fp2", true},
		{"the wait is over", Request{Fingerprint: "fp1", Attempts: MaxAttempts, LastTried: now.Add(-GiveUpFor)}, "fp1", true},
		{"a request still being tried", Request{Fingerprint: "fp1", Attempts: 1, LastTried: now.Add(-time.Hour)}, "fp2", false},
	}
	for _, c := range cases {
		if got := c.r.Superseded(c.fingerprint, now); got != c.want {
			t.Errorf("%s: Superseded=%v want %v", c.name, got, c.want)
		}
	}
}

// Queueing a session again must not clear what is known about earlier failures,
// or every scan would reset the backoff and retries would be continuous.
func TestQueueFindCarriesFailuresForward(t *testing.T) {
	q := tempQueue(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if err := q.Add(Request{PluginID: "claude", SessionID: "s1", Queued: now, Attempts: 2, LastTried: now}); err != nil {
		t.Fatal(err)
	}
	prev, ok := q.Find("claude", "s1")
	if !ok || prev.Attempts != 2 || !prev.LastTried.Equal(now) {
		t.Fatalf("Find=%+v ok=%v", prev, ok)
	}
	if _, ok := q.Find("claude", "never-queued"); ok {
		t.Error("Find reported a request for a session that was never queued")
	}
}

// show takes a request out of the queue before generating that session itself. A
// worker that started on it earlier must not put it back by saying how far it got.
func TestAdvancingARequestThatWasTakenOutLeavesItOut(t *testing.T) {
	q := tempQueue(t)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: time.Now()}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	r.Answered = 1
	if err := q.Advance(r); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Find("claude", "s1"); got.Answered != 1 {
		t.Errorf("answered=%d, want it recorded", got.Answered)
	}
	if err := q.Done(r); err != nil {
		t.Fatal(err)
	}
	r.Answered = 2
	if err := q.Advance(r); err != nil {
		t.Fatal(err)
	}
	if _, ok := q.Find("claude", "s1"); ok {
		t.Error("the request came back")
	}
}

// One queue for one directory. What it remembers has to outlive a scan to be worth
// anything, and every scan opens the queue again.
func TestOpeningADirectoryTwiceIsTheSameQueue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "q")
	a, err := OpenQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("the second open returned another queue")
	}
}

// A request holds a session's text, and the scan that asks about the queue comes every
// few seconds. So a file is read once, and again only when it changes. Proved by what
// is on disk no longer being readable: an answer can then only have been remembered.
func TestAQueuedRequestIsReadOnceUntilItChanges(t *testing.T) {
	q := tempQueue(t)
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: now, Fingerprint: "fp", Prompts: []string{"text of the session"}}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	if got, ok := q.Peek("claude", "s1"); !ok || got.Fingerprint != "fp" {
		t.Fatalf("Peek=%+v %v", got, ok)
	}
	path := filepath.Join(q.dir, requestName("claude", "s1"))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, info.Size()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	if got, ok := q.Peek("claude", "s1"); !ok || got.Fingerprint != "fp" {
		t.Errorf("Peek read the file again: %+v %v", got, ok)
	}
	if got, bad := q.Headers(); len(got) != 1 || bad != 0 || got[0].Fingerprint != "fp" {
		t.Errorf("Headers read the file again: %+v unreadable=%d", got, bad)
	}

	// Changed, it is read again.
	r.Fingerprint = "fp2"
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	if got, ok := q.Peek("claude", "s1"); !ok || got.Fingerprint != "fp2" {
		t.Errorf("Peek=%+v %v, want the new version", got, ok)
	}
	// Gone, it is gone.
	if err := q.Done(r); err != nil {
		t.Fatal(err)
	}
	if _, ok := q.Peek("claude", "s1"); ok {
		t.Error("Peek found a request that was taken out")
	}
	if got, _ := q.Headers(); len(got) != 0 {
		t.Errorf("Headers=%+v after the request was taken out", got)
	}
}

// What is remembered is what the question needs. A session's text kept in memory for
// every queued request would be its own cost.
func TestHeadersHoldNoPrompts(t *testing.T) {
	q := tempQueue(t)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: time.Now(), Prompts: []string{"text"}, Batches: [][]int{{1}}, Nodes: map[int]string{1: "n"}}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	got, _ := q.Headers()
	if len(got) != 1 || got[0].Prompts != nil || got[0].Batches != nil || got[0].Nodes != nil {
		t.Errorf("Headers=%+v", got)
	}
	peeked, _ := q.Peek("claude", "s1")
	if peeked.Prompts != nil {
		t.Errorf("Peek=%+v", peeked)
	}
	if full, _ := q.Find("claude", "s1"); len(full.Prompts) != 1 {
		t.Errorf("Find=%+v, which has to read the prompts", full)
	}
}

// A file's time moves in steps of milliseconds, so a request written twice in the same
// step, at the same size, looks unchanged by those two alone. Every write here replaces
// the file, and that is what gives it away.
func TestARequestRewrittenAtTheSameSizeAndTimeIsReadAgain(t *testing.T) {
	q := tempQueue(t)
	r := Request{PluginID: "claude", SessionID: "s1", Queued: time.Now(), Fingerprint: "aaa"}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Peek("claude", "s1"); got.Fingerprint != "aaa" {
		t.Fatalf("Peek=%+v", got)
	}
	path := filepath.Join(q.dir, requestName("claude", "s1"))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	r.Fingerprint = "bbb"
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}

	if got, _ := q.Peek("claude", "s1"); got.Fingerprint != "bbb" {
		t.Errorf("Peek=%q, the version the file no longer holds", got.Fingerprint)
	}
}
