package summary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
