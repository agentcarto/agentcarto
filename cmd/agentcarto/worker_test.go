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
// summarized moments before the turns it is being asked about existed — would
// make that a lie, and leave the reader with nothing.
//
// Nothing else needs such a window: queueOne writes a request only for a turn
// that has no summary, so a session with nothing new to describe is never queued.
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
		PluginID: "claude", SessionID: "grown", Queued: time.Now(), Fingerprint: "fp1",
		Nodes: map[int]string{2: "n2"}, Batches: [][]int{{2}}, Prompts: []string{"doc"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	g := &fakeGenerator{out: "@@TURN 2\n新しいターンの要約\n"}
	var log bytes.Buffer

	if spent := runRequest(ctx, &log, q, d, g, r, time.Hour); !spent {
		t.Fatalf("the request was dropped instead of answered: %s", log.String())
	}
	if g.calls != 1 {
		t.Errorf("the generator was called %d times, want 1", g.calls)
	}
	if got := d.Summaries(ctx, s, map[int]string{2: "n2"}); got[2].Text != "新しいターンの要約" {
		t.Errorf("the new turn was not stored: %q", got[2].Text)
	}
	// The version the summary was made from is stored with it. Without this every
	// summary the worker wrote carried an empty fingerprint, so `show` called it
	// out of date the moment it was written and worthOpening's cheap "nothing
	// changed" check never matched — the session was reparsed on every scan.
	if got := d.Summaries(ctx, s, map[int]string{2: "n2"}); got[2].Fingerprint != "fp1" {
		t.Errorf("fingerprint=%q, want the version the prompts were built from", got[2].Fingerprint)
	}
}

// An incremental call sees the turns it was asked about and nothing else, so the
// session summary it returns describes those turns rather than the session.
// Storing it is what made a long session's summary read as a list of its newest
// turns — f64330cd's said "…（ターン353）…（ターン365・366）" for a 366-turn session.
//
// Within the interval the stored one is left alone, and no second call is made.
func TestAnIncrementalCallDoesNotOverwriteTheSessionSummary(t *testing.T) {
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
	s := domain.Session{PluginID: "claude", SessionID: "long", Fingerprint: "fp1"}
	if err := d.PutSummaries(ctx, s, []cache.Summary{
		{Turn: 0, Text: "セッション全体を語る要約", Model: "m"},
		{Turn: 1, NodeID: "n1", Text: "ターン1", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	// Three turns exist; this request asks about the newest one only.
	r := summary.Request{
		PluginID: "claude", SessionID: "long", Queued: time.Now(), Fingerprint: "fp1",
		Nodes:   map[int]string{1: "n1", 2: "n2", 3: "n3"},
		Batches: [][]int{{3}}, Prompts: []string{"doc"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	g := &fakeGenerator{out: "@@SESSION\n直近のターンだけを見た要約\n\n@@TURN 3\nターン3の要約\n"}
	var log bytes.Buffer

	if spent := runRequest(ctx, &log, q, d, g, r, time.Hour); !spent {
		t.Fatalf("the request was dropped: %s", log.String())
	}
	if g.calls != 1 {
		t.Errorf("the generator was called %d times, want 1 — no session summary was due", g.calls)
	}
	got := d.Summaries(ctx, s, r.Nodes)
	if got[3].Text != "ターン3の要約" {
		t.Errorf("the new turn was not stored: %q", got[3].Text)
	}
	if got[0].Text != "セッション全体を語る要約" {
		t.Errorf("the session summary was overwritten with a partial view: %q", got[0].Text)
	}
}

// Once the interval has passed, the session summary is made again — from the
// turn summaries, not from the call that saw one turn. That is a call of its
// own, which is why it has a pace of its own.
func TestTheSessionSummaryIsRemadeFromTheTurnsWhenItIsDue(t *testing.T) {
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
	s := domain.Session{PluginID: "claude", SessionID: "long", Fingerprint: "fp1"}
	if err := d.PutSummaries(ctx, s, []cache.Summary{
		{Turn: 0, Text: "古いセッション要約", Model: "m"},
		{Turn: 1, NodeID: "n1", Text: "ターン1", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	r := summary.Request{
		PluginID: "claude", SessionID: "long", Queued: time.Now(), Fingerprint: "fp1",
		Nodes:   map[int]string{1: "n1", 2: "n2"},
		Batches: [][]int{{2}}, Prompts: []string{"doc"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	// The answer serves both calls: the turn call parses @@TURN 2, and the
	// session call parses @@SESSION.
	g := &fakeGenerator{out: "@@SESSION\nターン要約から作り直した要約\n\n@@TURN 2\nターン2の要約\n"}
	var log bytes.Buffer

	// An interval of zero: the session summary is always due.
	if spent := runRequest(ctx, &log, q, d, g, r, 0); !spent {
		t.Fatalf("the request was dropped: %s", log.String())
	}
	if g.calls != 2 {
		t.Fatalf("the generator was called %d times, want 2 (the turn, then the session)", g.calls)
	}
	if got := d.Summaries(ctx, s, r.Nodes); got[0].Text != "ターン要約から作り直した要約" {
		t.Errorf("the session summary was not remade: %q", got[0].Text)
	}
}

// A model that answers without describing the turn it was asked about must not
// leave the session looking untouched. Parse drops a missing @@TURN rather than
// failing, so nothing is stored — and with no pace left on queueing, the same
// turn would be wanted, requested and paid for on every scan, which in the TUI
// is every few seconds.
//
// Recording the version answers it: worthOpening sees the log has not moved and
// leaves the session alone until it does.
func TestAnAnswerWithNoTurnStillRecordsTheVersion(t *testing.T) {
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
	s := domain.Session{PluginID: "claude", SessionID: "quiet", Fingerprint: "fp1"}
	// An incremental request: two turns exist, this one asks about the newer.
	r := summary.Request{
		PluginID: "claude", SessionID: "quiet", Queued: time.Now(), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1", 2: "n2"}, Batches: [][]int{{2}}, Prompts: []string{"doc"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	// The model answered, but said nothing about turn 2. Its session summary is
	// not usable either: this call saw one turn of a two-turn session.
	g := &fakeGenerator{out: "@@SESSION\n1ターンしか見ていない話\n"}
	var log bytes.Buffer

	runRequest(ctx, &log, q, d, g, r, time.Hour)

	if worthOpening(ctx, d, q, s) {
		t.Error("the session would be opened, requested and paid for again on the next scan")
	}
	// And the empty answer was not mistaken for a summary.
	if when, ok := d.SummarizedAt(ctx, s); ok {
		t.Errorf("a session with no stored summary reports having been summarized at %s", when)
	}
}

// A request that held a turn back has not answered its version of the log, and
// must not record one. Turn 0 carries that record and worthOpening reads it: a
// session recorded as answered is not opened again until its log moves, and a
// log whose newest turn just ended has no reason to move. The held turn would
// never be summarized.
//
// Both writes to turn 0 have to be skipped, not just the marker: storing a
// session summary stamps the fingerprint too.
func TestAHeldRequestDoesNotRecordTheVersionAsAnswered(t *testing.T) {
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
	s := domain.Session{PluginID: "claude", SessionID: "backlog", Fingerprint: "fpFinal"}
	// Three turns exist. Turns 1-2 were pending when the newest one ended, so the
	// request covers them and holds turn 3 back.
	r := summary.Request{
		PluginID: "claude", SessionID: "backlog", Queued: time.Now(), Fingerprint: "fpFinal", Held: true,
		Nodes:   map[int]string{1: "n1", 2: "n2"},
		Batches: [][]int{{1, 2}}, Prompts: []string{"doc"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	g := &fakeGenerator{out: "@@SESSION\n全体を見たつもりの要約\n\n@@TURN 1\nターン1\n\n@@TURN 2\nターン2\n"}
	var log bytes.Buffer

	if spent := runRequest(ctx, &log, q, d, g, r, 0); !spent {
		t.Fatalf("the request was dropped: %s", log.String())
	}
	// The turns it did cover are stored.
	if got := d.Summaries(ctx, s, r.Nodes); got[1].Text == "" || got[2].Text == "" {
		t.Errorf("the covered turns were not stored: %v", got)
	}
	// But the version is not recorded, so the next scan opens the session and
	// finds turn 3 waiting.
	if !worthOpening(ctx, d, q, s) {
		t.Fatal("the held request recorded the version as answered — turn 3 is lost")
	}
	// And no session summary was written from a run that did not see turn 3.
	if got := d.Summaries(ctx, s, r.Nodes); got[0].Text != "" {
		t.Errorf("a run that held a turn back wrote the session summary: %q", got[0].Text)
	}
}

// A single call that covered every turn already describes the whole session, so
// its answer is stored as it stands and no second call is made. This is a first
// summary of a short session — the common case on a machine being backfilled.
func TestACallThatSawEveryTurnWritesTheSessionSummaryItself(t *testing.T) {
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
	s := domain.Session{PluginID: "claude", SessionID: "short", Fingerprint: "fp1"}
	r := summary.Request{
		PluginID: "claude", SessionID: "short", Queued: time.Now(), Fingerprint: "fp1",
		Nodes:   map[int]string{1: "n1", 2: "n2"},
		Batches: [][]int{{1, 2}}, Prompts: []string{"doc"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	g := &fakeGenerator{out: "@@SESSION\n全体を見た要約\n\n@@TURN 1\nターン1\n\n@@TURN 2\nターン2\n"}
	var log bytes.Buffer

	if spent := runRequest(ctx, &log, q, d, g, r, time.Hour); !spent {
		t.Fatalf("the request was dropped: %s", log.String())
	}
	if g.calls != 1 {
		t.Errorf("the generator was called %d times, want 1 — the one call saw everything", g.calls)
	}
	if got := d.Summaries(ctx, s, r.Nodes); got[0].Text != "全体を見た要約" {
		t.Errorf("session summary=%q, want the answer of the call that saw every turn", got[0].Text)
	}
}
