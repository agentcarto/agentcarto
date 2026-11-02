package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// failingGenerator stands in for an agent CLI that fails every call.
type failingGenerator struct{ err error }

func (g failingGenerator) Generate(context.Context, string, string) (string, error) {
	return "", g.err
}
func (g failingGenerator) Name() string { return "fake m" }

// A request that failed keeps the reason, whichever call it was. The session's own
// summary is the case easiest to lose: that call fails inside storeSessionSummary,
// and a reason that stopped at the log there would leave the request saying
// nothing about why it is waiting.
func TestAFailedRequestKeepsTheReason(t *testing.T) {
	const reason = "claude: exit status 1: You have hit your limit"
	cases := []struct {
		name    string
		request summary.Request
	}{
		{"a turn call", summary.Request{Nodes: map[int]string{1: "n1"}, Batches: [][]int{{1}}, Prompts: []string{"doc"}}},
		{"the session's own summary", summary.Request{Nodes: map[int]string{1: "n1"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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
			if err := d.PutSummaries(ctx, s, []cache.Summary{{Turn: 1, NodeID: "n1", Text: "ターン1の要約", Model: "m"}}); err != nil {
				t.Fatal(err)
			}
			r := c.request
			r.PluginID, r.SessionID, r.Queued, r.Fingerprint = s.PluginID, s.SessionID, time.Now(), s.Fingerprint
			if err := q.Add(r); err != nil {
				t.Fatal(err)
			}

			var log bytes.Buffer
			runRequest(ctx, &log, q, d, failingGenerator{errors.New(reason)}, r, 0, "")

			back, still := q.Find(s.PluginID, s.SessionID)
			if !still {
				t.Fatalf("the request was dropped: %s", log.String())
			}
			if !strings.Contains(back.LastError, reason) {
				t.Errorf("LastError=%q want it to carry %q", back.LastError, reason)
			}
		})
	}
}

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

	if spent := runRequest(ctx, &log, q, d, g, r, time.Hour, ""); !spent {
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

	if spent := runRequest(ctx, &log, q, d, g, r, time.Hour, ""); !spent {
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
	if spent := runRequest(ctx, &log, q, d, g, r, 0, ""); !spent {
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

	runRequest(ctx, &log, q, d, g, r, time.Hour, "")

	if worthOpening(ctx, d, q, s, time.Hour) {
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

	if spent := runRequest(ctx, &log, q, d, g, r, 0, ""); !spent {
		t.Fatalf("the request was dropped: %s", log.String())
	}
	// The turns it did cover are stored.
	if got := d.Summaries(ctx, s, r.Nodes); got[1].Text == "" || got[2].Text == "" {
		t.Errorf("the covered turns were not stored: %v", got)
	}
	// But the version is not recorded, so the next scan opens the session and
	// finds turn 3 waiting.
	if !worthOpening(ctx, d, q, s, time.Hour) {
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

	if spent := runRequest(ctx, &log, q, d, g, r, time.Hour, ""); !spent {
		t.Fatalf("the request was dropped: %s", log.String())
	}
	if g.calls != 1 {
		t.Errorf("the generator was called %d times, want 1 — the one call saw everything", g.calls)
	}
	if got := d.Summaries(ctx, s, r.Nodes); got[0].Text != "全体を見た要約" {
		t.Errorf("session summary=%q, want the answer of the call that saw every turn", got[0].Text)
	}
}

// The headline is written by the call that writes the session summary, and is
// stored with it. This is the path a short session takes: one call sees every
// turn, so its answer describes the session as a whole.
func TestTheHeadlineIsStoredWithASummaryFromAWholeSessionCall(t *testing.T) {
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
		Nodes: map[int]string{1: "n1"}, Batches: [][]int{{1}}, Prompts: []string{"doc"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	g := &fakeGenerator{out: "@@HEADLINE\npull-all.shの作成\n\n@@SESSION\nスクリプトを作って6リポジトリのpullを確認した話\n\n@@TURN 1\nターン1の要約\n"}
	var log bytes.Buffer

	if spent := runRequest(ctx, &log, q, d, g, r, time.Hour, ""); !spent {
		t.Fatalf("the request was dropped: %s", log.String())
	}
	got := d.Summaries(ctx, s, r.Nodes)
	if got[0].Headline != "pull-all.shの作成" {
		t.Errorf("headline=%q, want the one the call wrote", got[0].Headline)
	}
	if got[0].Text != "スクリプトを作って6リポジトリのpullを確認した話" {
		t.Errorf("the summary the headline belongs to was not stored: %q", got[0].Text)
	}
	if got[1].Headline != "" {
		t.Errorf("a turn summary carries no headline, got %q", got[1].Headline)
	}
}

// The other path: the session summary is remade from the turn summaries once
// the interval has passed. That call writes its own headline, and the stored
// one is replaced along with the paragraph it belongs to.
func TestTheRemadeSessionSummaryBringsItsOwnHeadline(t *testing.T) {
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
		{Turn: 0, Text: "古いセッション要約", Headline: "古い見出し", Model: "m"},
		{Turn: 1, NodeID: "n1", Text: "ターン1", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	r := summary.Request{
		PluginID: "claude", SessionID: "long", Queued: time.Now(), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1", 2: "n2"}, Batches: [][]int{{2}}, Prompts: []string{"doc"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	g := &fakeGenerator{out: "@@HEADLINE\n新しい見出し\n\n@@SESSION\nターン要約から作り直した要約\n\n@@TURN 2\nターン2の要約\n"}
	var log bytes.Buffer

	// An interval of zero: the session summary is always due.
	if spent := runRequest(ctx, &log, q, d, g, r, 0, ""); !spent {
		t.Fatalf("the request was dropped: %s", log.String())
	}
	got := d.Summaries(ctx, s, r.Nodes)
	if got[0].Headline != "新しい見出し" {
		t.Errorf("headline=%q — the remade summary should bring its own", got[0].Headline)
	}
	if got[0].Text != "ターン要約から作り直した要約" {
		t.Errorf("summary=%q", got[0].Text)
	}
}

// A request that carries no turn prompts asks for the session summary alone —
// what queueOne writes for a session whose turns are all described and whose
// turn 0 an interval left behind. The worker has to reach it: the loop it walks
// first is empty, and everything that matters is after it.
func TestARequestWithNoPromptsRemakesTheSessionSummary(t *testing.T) {
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
	s := domain.Session{PluginID: "claude", SessionID: "owed", Fingerprint: "fp1"}
	if err := d.PutSummaries(ctx, s, []cache.Summary{
		{Turn: 0, Text: "取り残されたセッション要約", Model: "m"},
		{Turn: 1, NodeID: "n1", Text: "ターン1の要約", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	r := summary.Request{
		PluginID: "claude", SessionID: "owed", Queued: time.Now(), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	g := &fakeGenerator{out: "@@SESSION\nターン要約から作り直した要約\n"}
	var log bytes.Buffer

	runRequest(ctx, &log, q, d, g, r, 0, "")

	if got := d.Summaries(ctx, s, r.Nodes)[0].Text; got != "ターン要約から作り直した要約" {
		t.Errorf("the session summary was not remade: %q\n%s", got, log.String())
	}
	if g.calls != 1 {
		t.Errorf("one call makes a session summary, got %d", g.calls)
	}
	if _, still := q.Find("claude", "owed"); still {
		t.Error("the answered request stayed in the queue and would be paid for again")
	}
}

// The same request, unanswered, goes back in the queue rather than being
// finished. worthOpening asks for it again the moment a scan runs, so finishing
// it here would put a failing call on a loop that costs money every few seconds;
// the queue's backoff is what paces a retry.
func TestASessionSummaryThatCouldNotBeMadeGoesBackInTheQueue(t *testing.T) {
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
	s := domain.Session{PluginID: "claude", SessionID: "owed", Fingerprint: "fp1"}
	if err := d.PutSummaries(ctx, s, []cache.Summary{
		{Turn: 0, Text: "取り残されたセッション要約", Model: "m"},
		{Turn: 1, NodeID: "n1", Text: "ターン1の要約", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	r := summary.Request{
		PluginID: "claude", SessionID: "owed", Queued: time.Now(), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	// An answer with nothing in it: the model said nothing usable about the session.
	g := &fakeGenerator{out: ""}
	var log bytes.Buffer

	runRequest(ctx, &log, q, d, g, r, 0, "")

	back, still := q.Find("claude", "owed")
	if !still {
		t.Fatalf("the request was dropped, so nothing will ask for this summary again: %s", log.String())
	}
	if back.Attempts != 1 {
		t.Errorf("the failure was not recorded: attempts=%d", back.Attempts)
	}
	// What was already paid for stays as it was.
	if got := d.Summaries(ctx, s, r.Nodes)[0].Text; got != "取り残されたセッション要約" {
		t.Errorf("the stored session summary was lost: %q", got)
	}
}

// observingGenerator records what the status says while each call is being made.
type observingGenerator struct {
	fakeGenerator
	reader *summary.StatusReader
	key    domain.SessionKey
	seen   []string
}

func (g *observingGenerator) Generate(ctx context.Context, system, prompt string) (string, error) {
	st := g.reader.Read(time.Now())
	g.seen = append(g.seen, fmt.Sprintf("%v %s", st.Current == g.key, st.Sessions[g.key].CallLabel()))
	return g.fakeGenerator.Generate(ctx, system, prompt)
}

// One call on a long session runs for minutes, and a queue cannot say which session
// that is: a request leaves only when it is finished. So each call has to be
// announced before it is made, the session's own summary included.
func TestTheStatusFollowsAWorkerThroughItsCalls(t *testing.T) {
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
	lock, err := summary.TakeLock(lockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	r := summary.Request{
		PluginID: "claude", SessionID: "long", Queued: time.Now(), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1", 2: "n2"}, Batches: [][]int{{1}, {2}}, Prompts: []string{"a", "b"},
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	g := &observingGenerator{
		fakeGenerator: fakeGenerator{out: "@@SESSION\nまとめ\n@@TURN 1\nいち\n@@TURN 2\nに"},
		reader:        summary.NewStatusReader(q, lockPath()),
		key:           domain.SessionKey{PluginID: "claude", SessionID: "long"},
	}
	var log bytes.Buffer

	runRequest(ctx, &log, q, d, g, r, 0, "")

	want := []string{"true call 1 of 2", "true call 2 of 2", "true the session's own summary"}
	if fmt.Sprint(g.seen) != fmt.Sprint(want) {
		t.Errorf("the status said %q while the calls ran, want %q\n%s", g.seen, want, log.String())
	}
}

// A worker that was killed leaves its record, and a new one starts with the lock:
// without clearing, the status names the dead worker's session as the new one's.
func TestAWorkerLeavesNoRecordOfWhatItHadInHand(t *testing.T) {
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
	if err := q.MarkWorking(summary.Request{PluginID: "claude", SessionID: "from-a-dead-worker"}, 1, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Summary: config.Summary{Agent: "claude", Model: "claude-sonnet-5", MaxPerRun: 2}}

	summarizeWorkerCmd(ctx, cfg, d, nil)

	// The lock is what makes a record count, so this asks as a worker holding it.
	lock, err := summary.TakeLock(lockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if st := summary.NewStatusReader(q, lockPath()).Read(time.Now()); st.Current != (domain.SessionKey{}) {
		t.Errorf("the record outlived the worker: %+v", st.Current)
	}
}

func TestDoctorSaysWhatTheWorkerIsDoing(t *testing.T) {
	when := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	now := domain.SessionKey{PluginID: "claude", SessionID: "39bd4a6c-aaaa"}
	st := summary.Status{
		Enabled: true, Running: true, PID: 42, Since: when, Current: now, Queued: 12, Failed: 2, Unreadable: 1,
		Sessions: map[domain.SessionKey]summary.SessionStatus{
			now:                                     {State: summary.SessionWorking, Call: 2, Calls: 3, CallStarted: when},
			{PluginID: "codex", SessionID: "older"}: {State: summary.SessionFailed, Attempts: 1, LastTried: when.Add(-2 * time.Hour), LastError: "an old reason"},
			{PluginID: "claude", SessionID: "12fd10c9a"}: {State: summary.SessionFailed, Attempts: 1, LastTried: when.Add(-time.Hour), LastError: "the newest reason"},
		},
	}
	var out bytes.Buffer
	writeSummaryStatus(&out, st, "/queue")
	for _, want := range []string{
		"running as pid 42",
		"kill 42 ends this run",
		"now        claude/39bd4a6c, call 2 of 3, started 12:00:00",
		"queue      14 waiting (2 waiting after a failure) (1 unreadable) — /queue",
		"failure    claude/12fd10c9 at 08-24 11:00: the newest reason",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor does not say %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "an old reason") {
		t.Errorf("doctor printed more than the latest failure:\n%s", out.String())
	}
}

// A queue that cannot be opened says nothing about whether a worker is running, and
// that is what someone looking into a bill came to doctor to learn.
func TestDoctorSaysWhetherAWorkerIsRunningWithoutAQueue(t *testing.T) {
	_, _ = summaryFixture(t)
	if err := os.WriteFile(queueDir(), nil, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := summary.TakeLock(lockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	var out bytes.Buffer

	writeQueueStatus(&out)

	if want := fmt.Sprintf("running as pid %d", os.Getpid()); !strings.Contains(out.String(), want) {
		t.Errorf("doctor does not say %q:\n%s", want, out.String())
	}
	if !strings.Contains(out.String(), "  queue      ") {
		t.Errorf("doctor does not say what is wrong with the queue:\n%s", out.String())
	}
}

func TestDoctorOfAnIdleQueueSaysSo(t *testing.T) {
	var out bytes.Buffer
	writeSummaryStatus(&out, summary.Status{Enabled: true}, "/queue")
	if !strings.Contains(out.String(), "worker     not running") || !strings.Contains(out.String(), "queue      0 waiting — /queue") {
		t.Errorf("doctor said:\n%s", out.String())
	}
	for _, unwanted := range []string{"now  ", "failure"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("doctor mentions %q with nothing to say:\n%s", unwanted, out.String())
		}
	}
}

// A request that failed before reasons were kept has none until it fails again, and
// the line must not read as if one had been cut off.
func TestDoctorLeavesOutAReasonThatWasNotRecorded(t *testing.T) {
	key := domain.SessionKey{PluginID: "claude", SessionID: "oldformat"}
	st := summary.Status{Enabled: true, Failed: 1, Sessions: map[domain.SessionKey]summary.SessionStatus{
		key: {State: summary.SessionFailed, Attempts: 1, LastTried: time.Date(2026, 8, 24, 11, 0, 0, 0, time.Local)},
	}}
	var out bytes.Buffer
	writeSummaryStatus(&out, st, "/queue")
	if want := "  failure    claude/oldforma at 08-24 11:00\n"; !strings.Contains(out.String(), want) {
		t.Errorf("doctor said:\n%s\nwant the line %q", out.String(), want)
	}
}

// Times travel in the files with the offset they were written under, and the
// machine reading them may since have moved. What doctor prints is read against the
// clock on the wall.
func TestDoctorPrintsTimesInLocalTime(t *testing.T) {
	when := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	_, offset := when.Zone()
	elsewhere := time.FixedZone("elsewhere", offset+3*3600)
	now := domain.SessionKey{PluginID: "claude", SessionID: "working"}
	failed := domain.SessionKey{PluginID: "claude", SessionID: "failed"}
	st := summary.Status{
		Enabled: true, Running: true, PID: 42, Since: when.In(elsewhere), Current: now, Failed: 1,
		Sessions: map[domain.SessionKey]summary.SessionStatus{
			now:    {State: summary.SessionWorking, Call: 1, Calls: 1, CallStarted: when.In(elsewhere)},
			failed: {State: summary.SessionFailed, Attempts: 1, LastTried: when.In(elsewhere), LastError: "limit"},
		},
	}
	var out bytes.Buffer
	writeSummaryStatus(&out, st, "/queue")
	for _, want := range []string{"since 12:00:00", "started 12:00:00", "at 08-24 12:00: limit"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor does not say %q:\n%s", want, out.String())
		}
	}
}

// A failure's backoff ending is not the failure being resolved. doctor starts no
// worker, so a person asking in the morning why last night's run left nothing finds
// the requests ready and untried, and they are what the question is about.
func TestDoctorStillReportsAFailureOnceItsBackoffIsOver(t *testing.T) {
	when := time.Date(2026, 8, 24, 11, 0, 0, 0, time.Local)
	st := summary.Status{Enabled: true, Queued: 2, Sessions: map[domain.SessionKey]summary.SessionStatus{
		{PluginID: "claude", SessionID: "ready-again"}: {State: summary.SessionQueued, Attempts: 1, LastTried: when, LastError: "limit"},
		{PluginID: "claude", SessionID: "never-tried"}: {State: summary.SessionQueued},
	}}
	var out bytes.Buffer
	writeSummaryStatus(&out, st, "/queue")
	for _, want := range []string{"queue      2 waiting (1 waiting after a failure)", "failure    claude/ready-ag at 08-24 11:00: limit"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor does not say %q:\n%s", want, out.String())
		}
	}
}

// The session in hand is being tried right now, so its last failure is not what is
// wrong at the moment.
func TestDoctorDoesNotReportASessionBeingRetriedAsFailed(t *testing.T) {
	now := domain.SessionKey{PluginID: "claude", SessionID: "working"}
	st := summary.Status{Enabled: true, Running: true, PID: 42, Current: now, Sessions: map[domain.SessionKey]summary.SessionStatus{
		now: {State: summary.SessionWorking, Attempts: 1, LastError: "limit", Call: 1, Calls: 1},
	}}
	var out bytes.Buffer
	writeSummaryStatus(&out, st, "/queue")
	if strings.Contains(out.String(), "failure") {
		t.Errorf("doctor said:\n%s", out.String())
	}
}

// stoppingGenerator is a worker stopped in the middle of a call: the context ends,
// and the call comes back with whatever it has by then.
type stoppingGenerator struct {
	stop  context.CancelFunc
	out   string
	err   error
	calls int
}

// Generate ends the context, then answers as configured.
func (g *stoppingGenerator) Generate(context.Context, string, string) (string, error) {
	g.calls++
	g.stop()
	return g.out, g.err
}

// Name is what the summaries are stored as written by.
func (g *stoppingGenerator) Name() string { return "fake m" }

// stoppedFixture opens a store and a queue holding r.
func stoppedFixture(t *testing.T, r summary.Request) (*cache.DB, *summary.Queue) {
	t.Helper()
	_, _ = summaryFixture(t)
	d, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Add(r); err != nil {
		t.Fatal(err)
	}
	return d, q
}

// Being stopped says nothing about the session. Counted as a failure it would show
// as one, wait out the backoff, and after three stops never be tried again.
func TestAStoppedCallIsNotAFailure(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "stopped", Queued: time.Now(), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1"}, Batches: [][]int{{1}}, Prompts: []string{"a"},
	}
	d, q := stoppedFixture(t, r)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	g := &stoppingGenerator{stop: stop, err: errors.New("claude: cancelled")}
	var log bytes.Buffer

	runRequest(ctx, &log, q, d, g, r, 0, "")

	back, still := q.Find("claude", "stopped")
	if !still {
		t.Fatalf("the request was dropped, so nothing will ask for this summary again: %s", log.String())
	}
	if back.Attempts != 0 || back.LastError != "" {
		t.Errorf("being stopped was recorded as a failure: attempts=%d reason=%q", back.Attempts, back.LastError)
	}
}

// The same for the call that makes the session's own summary, which reports its
// failure a different way.
func TestAStoppedSessionSummaryIsNotAFailure(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "stopped", Queued: time.Now().Add(time.Minute), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1"},
	}
	d, q := stoppedFixture(t, r)
	s := domain.Session{PluginID: "claude", SessionID: "stopped", Fingerprint: "fp1"}
	if err := d.PutSummaries(context.Background(), s, []cache.Summary{{Turn: 1, NodeID: "n1", Text: "ターン1の要約", Model: "m"}}); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	g := &stoppingGenerator{stop: stop, err: errors.New("claude: cancelled")}
	var log bytes.Buffer

	runRequest(ctx, &log, q, d, g, r, 0, "")

	if g.calls != 1 {
		t.Fatalf("the session summary was not asked for: %d calls\n%s", g.calls, log.String())
	}
	if strings.Contains(log.String(), "could not be made") {
		t.Errorf("the log calls being stopped a failure:\n%s", log.String())
	}
	back, still := q.Find("claude", "stopped")
	if !still {
		t.Fatalf("the request was dropped, so nothing will ask for this summary again: %s", log.String())
	}
	if back.Attempts != 0 || back.LastError != "" {
		t.Errorf("being stopped was recorded as a failure: attempts=%d reason=%q", back.Attempts, back.LastError)
	}
}

// A stopped worker is one that was told to stop spending.
func TestAStoppedWorkerMakesNoFurtherCall(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "stopped", Queued: time.Now().Add(-time.Hour), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1", 2: "n2"}, Batches: [][]int{{1}, {2}}, Prompts: []string{"a", "b"},
	}
	d, q := stoppedFixture(t, r)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	g := &stoppingGenerator{stop: stop, out: "@@TURN 1\nいち"}
	var log bytes.Buffer

	runRequest(ctx, &log, q, d, g, r, 0, "")

	if g.calls != 1 {
		t.Errorf("the worker went on to make %d calls\n%s", g.calls, log.String())
	}
}

// An answer that came back before the stop was paid for. With it stored there is
// nothing left of the request for another worker to pay for.
func TestAnAnswerThatBeatTheStopIsKept(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "stopped", Queued: time.Now().Add(-time.Hour), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1"}, Batches: [][]int{{1}}, Prompts: []string{"a"},
	}
	d, q := stoppedFixture(t, r)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	g := &stoppingGenerator{stop: stop, out: "@@SESSION\nまとめ\n@@TURN 1\nいち"}
	var log bytes.Buffer

	runRequest(ctx, &log, q, d, g, r, 0, "")

	s := domain.Session{PluginID: "claude", SessionID: "stopped", Fingerprint: "fp1"}
	stored := d.Summaries(context.Background(), s, r.Nodes)
	if stored[1].Text != "いち" || stored[0].Text != "まとめ" {
		t.Fatalf("the paid-for answer was lost: %+v\n%s", stored, log.String())
	}
	if _, left := q.Find("claude", "stopped"); left {
		t.Errorf("the answered request stayed in the queue:\n%s", log.String())
	}
}

// scriptedGenerator answers each call with the next step, and records what it was
// asked. Past the last step it fails, which a test reads as one call too many.
type scriptedGenerator struct {
	steps   []func() (string, error)
	prompts []string
}

// Generate records the prompt and takes the next step.
func (g *scriptedGenerator) Generate(_ context.Context, _, prompt string) (string, error) {
	g.prompts = append(g.prompts, prompt)
	if len(g.prompts) > len(g.steps) {
		return "", errors.New("one call too many")
	}
	return g.steps[len(g.prompts)-1]()
}

// Name is what the summaries are stored as written by.
func (g *scriptedGenerator) Name() string { return "fake m" }

// answering is a step that returns out.
func answering(out string) func() (string, error) {
	return func() (string, error) { return out, nil }
}

// failing is a step that fails with err.
func failing(err error) func() (string, error) {
	return func() (string, error) { return "", err }
}

// A call that came back has been paid for, so a request that failed further on picks
// up where it stopped: its later calls are what is left to ask, and its failures stay
// on record. Asked to start again, it would pay twice. Dropped by the check for a
// session summarized since it was queued, which its own first call satisfies, it
// would lose its record of failing and, for a session whose log has gone, the turns
// only its prompts can still describe.
func TestAFailedRequestPicksUpAtTheCallThatFailed(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "long", Queued: time.Now().Add(-time.Hour), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1", 2: "n2"}, Batches: [][]int{{1}, {2}}, Prompts: []string{"first", "second"},
	}
	d, q := stoppedFixture(t, r)
	var log bytes.Buffer

	runRequest(context.Background(), &log, q, d, &scriptedGenerator{steps: []func() (string, error){
		answering("@@TURN 1\nいち"), failing(errors.New("the answer was cut off")),
	}}, r, 0, "")

	back, still := q.Find("claude", "long")
	if !still {
		t.Fatalf("the request was dropped:\n%s", log.String())
	}
	if back.Answered != 1 || back.Attempts != 1 {
		t.Fatalf("answered=%d attempts=%d, want 1 and 1", back.Answered, back.Attempts)
	}

	g := &scriptedGenerator{steps: []func() (string, error){
		answering("@@TURN 2\nに"), answering("@@SESSION\nまとめ"),
	}}
	runRequest(context.Background(), &log, q, d, g, back, 0, "")

	if len(g.prompts) == 0 || g.prompts[0] != "second" {
		t.Errorf("the retry asked %q first, want the call that failed\n%s", g.prompts, log.String())
	}
	s := domain.Session{PluginID: "claude", SessionID: "long", Fingerprint: "fp1"}
	stored := d.Summaries(context.Background(), s, r.Nodes)
	if stored[1].Text != "いち" || stored[2].Text != "に" {
		t.Errorf("stored=%+v\n%s", stored, log.String())
	}
	if _, left := q.Find("claude", "long"); left {
		t.Errorf("the finished request stayed in the queue:\n%s", log.String())
	}
}

// Stopped partway it picks up the same way, and costs only the call that was stopped.
func TestAStoppedRequestPicksUpAtTheCallThatWasStopped(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "long", Queued: time.Now().Add(-time.Hour), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1", 2: "n2"}, Batches: [][]int{{1}, {2}}, Prompts: []string{"first", "second"},
	}
	d, q := stoppedFixture(t, r)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var log bytes.Buffer

	runRequest(ctx, &log, q, d, &scriptedGenerator{steps: []func() (string, error){
		answering("@@TURN 1\nいち"),
		func() (string, error) { stop(); return "", errors.New("claude: cancelled") },
	}}, r, 0, "")

	back, still := q.Find("claude", "long")
	if !still || back.Answered != 1 || back.Attempts != 0 {
		t.Fatalf("still=%v answered=%d attempts=%d, want the request there, 1 and 0\n%s", still, back.Answered, back.Attempts, log.String())
	}
	g := &scriptedGenerator{steps: []func() (string, error){answering("@@TURN 2\nに"), answering("@@SESSION\nまとめ")}}
	runRequest(context.Background(), &log, q, d, g, back, 0, "")
	if len(g.prompts) == 0 || g.prompts[0] != "second" {
		t.Errorf("the next worker asked %q first, want the call that was stopped\n%s", g.prompts, log.String())
	}
}

// What the check at the top is for still works on a request partly done: something
// else answering the session after the last call this request knows about.
func TestAPartlyDoneRequestIsStillAnsweredBySomethingElse(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "long", Queued: time.Now().Add(-2 * time.Hour), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1", 2: "n2"}, Batches: [][]int{{1}, {2}}, Prompts: []string{"first", "second"},
		Answered: 1, AnsweredAt: time.Now().Add(-time.Hour),
	}
	d, q := stoppedFixture(t, r)
	s := domain.Session{PluginID: "claude", SessionID: "long", Fingerprint: "fp1"}
	if err := d.PutSummaries(context.Background(), s, []cache.Summary{{Turn: 2, NodeID: "n2", Text: "に", Model: "m"}}); err != nil {
		t.Fatal(err)
	}
	g := &scriptedGenerator{}
	var log bytes.Buffer

	runRequest(context.Background(), &log, q, d, g, r, 0, "")

	if len(g.prompts) != 0 {
		t.Errorf("it paid for %q, which something else had answered\n%s", g.prompts, log.String())
	}
	if _, left := q.Find("claude", "long"); left {
		t.Errorf("the answered request stayed in the queue:\n%s", log.String())
	}
}

// A failure that cost nothing still answers the question doctor is asked.
func TestDoctorReportsAFailureThatWasNotCounted(t *testing.T) {
	st := summary.Status{Enabled: true, Failed: 1, Sessions: map[domain.SessionKey]summary.SessionStatus{
		{PluginID: "claude", SessionID: "uncounted"}: {State: summary.SessionFailed, LastTried: time.Now(), LastError: "limit"},
	}}
	var out bytes.Buffer
	writeSummaryStatus(&out, st, "/queue")
	if !strings.Contains(out.String(), "1 waiting after a failure") || !strings.Contains(out.String(), ": limit") {
		t.Errorf("doctor does not report it:\n%s", out.String())
	}
}

// A session summary that is not due has not failed. Nothing was tried, and the request
// has nothing left to wait for: the scan that asked for it asks again once it is due.
func TestASessionSummaryNotDueIsNotAFailure(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "paced", Queued: time.Now().Add(time.Minute), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1"},
	}
	d, q := stoppedFixture(t, r)
	s := domain.Session{PluginID: "claude", SessionID: "paced", Fingerprint: "fp1"}
	if err := d.PutSummaries(context.Background(), s, []cache.Summary{
		{Turn: 0, Text: "まとめ", Model: "m"},
		{Turn: 1, NodeID: "n1", Text: "いち", Model: "m"},
	}); err != nil {
		t.Fatal(err)
	}
	g := &scriptedGenerator{}
	var log bytes.Buffer

	runRequest(context.Background(), &log, q, d, g, r, time.Hour, "")

	if len(g.prompts) != 0 {
		t.Errorf("it made a call: %q", g.prompts)
	}
	if back, left := q.Find("claude", "paced"); left {
		t.Errorf("the request stayed, reading as attempts=%d reason=%q", back.Attempts, back.LastError)
	}
}

// A signal to the whole group reaches the CLI as well as the worker, and the CLI's
// exit can be seen before the worker's context ends. That is still a stop.
func TestACLIThatDiesOfTheSameSignalFirstIsNotAFailure(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "stopped", Queued: time.Now(), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1"}, Batches: [][]int{{1}}, Prompts: []string{"a"},
	}
	d, q := stoppedFixture(t, r)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	g := &scriptedGenerator{steps: []func() (string, error){func() (string, error) {
		time.AfterFunc(20*time.Millisecond, stop)
		return "", errors.New("claude: signal: terminated")
	}}}
	var log bytes.Buffer

	runRequest(ctx, &log, q, d, g, r, 0, "")

	back, still := q.Find("claude", "stopped")
	if !still {
		t.Fatalf("the request was dropped:\n%s", log.String())
	}
	if back.Attempts != 0 || back.LastError != "" {
		t.Errorf("being stopped was recorded as a failure: attempts=%d reason=%q", back.Attempts, back.LastError)
	}
}

// Announced only when it is about to happen. A call that saw the whole session has
// answered for the session already, and saying "the session's own summary" there
// names work that does not exist.
func TestNoCallIsAnnouncedForASessionSummaryAlreadyInHand(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "short", Queued: time.Now().Add(-time.Hour), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1"}, Batches: [][]int{{1}}, Prompts: []string{"a"},
	}
	d, q := stoppedFixture(t, r)
	lock, err := summary.TakeLock(lockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	var log bytes.Buffer

	runRequest(context.Background(), &log, q, d, &fakeGenerator{out: "@@SESSION\nまとめ\n@@TURN 1\nいち"}, r, 0, "")

	st := summary.NewStatusReader(q, lockPath()).Read(time.Now())
	key := domain.SessionKey{PluginID: "claude", SessionID: "short"}
	if st.Current == key && st.Sessions[key].CallLabel() == "the session's own summary" {
		t.Errorf("the status named a call that was never made:\n%s", log.String())
	}
}

// With no turn summary to build from, no call is made. That says nothing about spending,
// and counting it would have a session give up for what cost nothing.
func TestASessionSummaryWithNothingToBuildFromIsNotCounted(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "empty", Queued: time.Now(), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1"},
	}
	d, q := stoppedFixture(t, r)
	g := &scriptedGenerator{}
	var log bytes.Buffer

	runRequest(context.Background(), &log, q, d, g, r, 0, "")

	if len(g.prompts) != 0 {
		t.Errorf("it made a call: %q", g.prompts)
	}
	back, still := q.Find("claude", "empty")
	if !still {
		t.Fatalf("the request was dropped:\n%s", log.String())
	}
	if back.Attempts != 0 || back.LastTried.IsZero() {
		t.Errorf("attempts=%d lastTried=%v, want a failure recorded and not counted", back.Attempts, back.LastTried)
	}
}
