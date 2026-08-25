package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/core/domain"
)

// The guard is against regenerating in a loop, not against regenerating at all:
// a session that grew must still be summarized again once the window passes.
func TestTooSoon(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		when       time.Time
		summarized bool
		want       bool
	}{
		{"never summarized", time.Time{}, false, false},
		{"just now", now.Add(-time.Minute), true, true},
		{"inside the window", now.Add(-regenerateWithin + time.Minute), true, true},
		{"exactly the window", now.Add(-regenerateWithin), true, false},
		{"past the window", now.Add(-regenerateWithin - time.Minute), true, false},
		{"long ago", now.Add(-30 * 24 * time.Hour), true, false},
		// An unreadable store answers "not summarized" with a zero time. Treating
		// that as recent would stop summarizing entirely; treating it as never is
		// what the guard is for — the caller then pays once, not on every run,
		// because a successful write moves the time forward.
		{"unreadable store", time.Time{}, false, false},
		// A clock that moved backwards (a laptop waking, an NTP step) must not
		// make a summary look like it comes from the future and block forever.
		{"written in the future", now.Add(time.Hour), true, true},
	}
	for _, c := range cases {
		if got := tooSoon(c.when, c.summarized, now); got != c.want {
			t.Errorf("%s: tooSoon=%v want %v", c.name, got, c.want)
		}
	}
}

func TestShort8(t *testing.T) {
	if got := short8("f64330cd-b244-4e5d"); got != "f64330cd" {
		t.Errorf("short8=%q", got)
	}
	if got := short8("abc"); got != "abc" {
		t.Errorf("short8 of a short id=%q", got)
	}
	if got := short8(""); got != "" {
		t.Errorf("short8 of an empty id=%q", got)
	}
}

// max_per_run bounds what a run spends, and what it spends is calls. A request
// whose session another run summarized minutes ago needs none — it costs one
// query — so counting it lets a queue full of those use the whole budget and
// generate nothing, leaving the requests behind them for a run that never
// reaches them.
//
// Nothing is generated here: every request in the queue is skipped, so the
// generator is never called and no money is spent.
func TestSkippedRequestsDoNotUseTheBudget(t *testing.T) {
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
	// Five sessions, all summarized just now, all queued again.
	for i := range 5 {
		s := domain.Session{PluginID: "claude", SessionID: fmt.Sprintf("s%d", i), Fingerprint: "fp1"}
		if err := d.PutSummaries(ctx, s, []cache.Summary{{Turn: 0, Text: "既にある要約", Model: "m"}}); err != nil {
			t.Fatal(err)
		}
		if err := q.Add(summary.Request{
			PluginID: "claude", SessionID: s.SessionID, Queued: time.Now(),
			Batches: [][]int{{1}}, Prompts: []string{"doc"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{Summary: config.Summary{Agent: "claude", Model: "claude-sonnet-5", MaxPerRun: 2}}

	summarizeWorkerCmd(ctx, cfg, d, nil)

	// All five were looked at and cleared. A budget that counted the skips would
	// have stopped after two, leaving three behind for no reason.
	reqs, _ := q.List()
	if len(reqs) != 0 {
		var left []string
		for _, r := range reqs {
			left = append(left, r.SessionID)
		}
		t.Errorf("the run stopped with %v still queued — skips used the budget", left)
	}
}
