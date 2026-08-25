package main

import (
	"bytes"
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
	// Five requests queued an hour ago, every one of them summarized since — by
	// another run, or by the `show` of somebody reading that session.
	for i := range 5 {
		s := domain.Session{PluginID: "claude", SessionID: fmt.Sprintf("s%d", i), Fingerprint: "fp1"}
		if err := q.Add(summary.Request{
			PluginID: "claude", SessionID: s.SessionID, Queued: time.Now().Add(-time.Hour),
			Batches: [][]int{{1}}, Prompts: []string{"doc"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := d.PutSummaries(ctx, s, []cache.Summary{{Turn: 0, Text: "既にある要約", Model: "m"}}); err != nil {
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

// fakeGenerator stands in for the agent CLI: it records that it was asked and
// answers in the marker format Parse reads, so a test can tell a request that
// was answered from one that was thrown away.
type fakeGenerator struct {
	out   string
	calls int
}

func (g *fakeGenerator) Generate(context.Context, string, string) (string, error) {
	g.calls++
	return g.out, nil
}
func (g *fakeGenerator) Name() string { return "fake m" }

// A request in the queue can be one somebody asked for by name: `show` hands a
// session needing several calls to the worker and tells the reader the
// summaries will appear on a later run. Dropping it because the session was
// summarized within the hour — before the turns it is being asked about existed
// — would make that a lie, and leave the reader with nothing for an hour.
//
// What holds a session that keeps growing to one summary an hour is
// EnqueueSummaries, which is the only thing that queues without being asked.
func TestARequestQueuedAfterTheLastSummaryIsAnswered(t *testing.T) {
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
	s := domain.Session{PluginID: "claude", SessionID: "grown", Fingerprint: "fp1"}
	// Summarized moments ago, and then grown: turn 2 is what the request asks
	// about, and it did not exist when the stored summary was made.
	if err := d.PutSummaries(ctx, s, []cache.Summary{{Turn: 0, Text: "古いセッション要約", Model: "m"}}); err != nil {
		t.Fatal(err)
	}
	r := summary.Request{
		PluginID: "claude", SessionID: "grown", Queued: time.Now(),
		Nodes: map[int]string{2: "n2"}, Batches: [][]int{{2}}, Prompts: []string{"doc"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	g := &fakeGenerator{out: "@@TURN 2\n新しいターンの要約\n"}
	var log bytes.Buffer

	if spent := runRequest(ctx, &log, q, d, g, r); !spent {
		t.Fatalf("the request was dropped instead of answered: %s", log.String())
	}
	if g.calls != 1 {
		t.Errorf("the generator was called %d times, want 1", g.calls)
	}
	if got := d.Summaries(ctx, s, map[int]string{2: "n2"}); got[2].Text != "新しいターンの要約" {
		t.Errorf("the new turn was not stored: %q", got[2].Text)
	}
}
