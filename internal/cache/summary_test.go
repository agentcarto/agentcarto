package cache

import (
	"context"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
)

func TestSummaryRoundTrip(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1", Fingerprint: "f1"}
	in := []Summary{
		{Turn: 0, Text: "コピー機能を実装しリリース", Model: "claude claude-sonnet-5"},
		{Turn: 1, NodeID: "n1", Text: "yキーを追加", Model: "claude claude-sonnet-5"},
		{Turn: 2, NodeID: "n2", Text: "USER情報も含める", Model: "claude claude-sonnet-5"},
	}
	if e := d.PutSummaries(ctx, s, in); e != nil {
		t.Fatal(e)
	}
	got := d.Summaries(ctx, s, map[int]string{1: "n1", 2: "n2"})
	if len(got) != 3 {
		t.Fatalf("read back %d summaries, want 3: %+v", len(got), got)
	}
	if got[1].Text != "yキーを追加" || got[1].NodeID != "n1" || got[1].Model != "claude claude-sonnet-5" {
		t.Errorf("turn 1 came back as %+v", got[1])
	}
	// The fingerprint the summary was made from comes back, so a reader can tell
	// that a session has grown since it was written.
	if got[1].Fingerprint != "f1" {
		t.Errorf("turn 1 lost the fingerprint it was made from: %+v", got[1])
	}
	// The session summary carries no node and is not matched against one.
	if got[0].Text != "コピー機能を実装しリリース" {
		t.Errorf("session summary came back as %+v", got[0])
	}
}

// A rewind or a fork renumbers turns: turn 2 may now hold a different node than
// the one summarized. That summary must not be shown against it.
func TestSummariesDropsTurnsWhoseNodeMoved(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1", Fingerprint: "f1"}
	if e := d.PutSummaries(ctx, s, []Summary{
		{Turn: 0, Text: "session"},
		{Turn: 1, NodeID: "n1", Text: "one"},
		{Turn: 2, NodeID: "n2", Text: "two"},
	}); e != nil {
		t.Fatal(e)
	}
	got := d.Summaries(ctx, s, map[int]string{1: "n1", 2: "other"})
	if _, ok := got[2]; ok {
		t.Errorf("turn 2 was returned for a node it was not made from: %+v", got[2])
	}
	if got[1].Text != "one" {
		t.Errorf("turn 1 should have survived, got %+v", got[1])
	}
	if got[0].Text != "session" {
		t.Errorf("the session summary should not depend on any node, got %+v", got[0])
	}
	// A turn the caller does not list at all is gone too — the branch on screen
	// is shorter than the one that was summarized.
	if got := d.Summaries(ctx, s, map[int]string{1: "n1"}); len(got) != 2 {
		t.Errorf("read back %+v, want only turn 0 and turn 1", got)
	}
	// A caller listing no turns at all still gets the session summary.
	if got := d.Summaries(ctx, s, nil); len(got) != 1 || got[0].Text != "session" {
		t.Errorf("with no turns listed, read back %+v, want only the session summary", got)
	}
}

// A session that grew is summarized turn by turn: writing the new turns must
// leave the ones already stored alone.
func TestPutSummariesUpsertsPerTurn(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1", Fingerprint: "f1"}
	if e := d.PutSummaries(ctx, s, []Summary{
		{Turn: 0, Text: "old session"},
		{Turn: 1, NodeID: "n1", Text: "one"},
	}); e != nil {
		t.Fatal(e)
	}
	s.Fingerprint = "f2" // the log grew
	if e := d.PutSummaries(ctx, s, []Summary{
		{Turn: 0, Text: "new session"},
		{Turn: 2, NodeID: "n2", Text: "two"},
	}); e != nil {
		t.Fatal(e)
	}
	got := d.Summaries(ctx, s, map[int]string{1: "n1", 2: "n2"})
	if got[1].Text != "one" {
		t.Errorf("turn 1 did not survive the second write: %+v", got[1])
	}
	if got[2].Text != "two" {
		t.Errorf("turn 2 was not written: %+v", got[2])
	}
	// The session summary goes stale as turns are added, so it is rewritten.
	if got[0].Text != "new session" {
		t.Errorf("the session summary was not replaced: %+v", got[0])
	}
}

func TestSummariesOfUnsummarizedSession(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "none"}
	if got := d.Summaries(context.Background(), s, map[int]string{1: "n1"}); len(got) != 0 {
		t.Fatalf("an unsummarized session returned %+v", got)
	}
	if e := d.PutSummaries(ctx, s, nil); e != nil {
		t.Fatalf("writing no summaries should be a no-op, got %v", e)
	}
}

// Summaries belong to one session: another session's rows must not leak in.
func TestSummariesAreScopedToOneSession(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	a := domain.Session{PluginID: "p", SessionID: "a"}
	b := domain.Session{PluginID: "p", SessionID: "b"}
	if e := d.PutSummaries(ctx, a, []Summary{{Turn: 1, NodeID: "n1", Text: "a1"}}); e != nil {
		t.Fatal(e)
	}
	if e := d.PutSummaries(ctx, b, []Summary{{Turn: 1, NodeID: "n1", Text: "b1"}}); e != nil {
		t.Fatal(e)
	}
	if got := d.Summaries(ctx, a, map[int]string{1: "n1"}); got[1].Text != "a1" {
		t.Errorf("session a read back %+v", got[1])
	}
	if got := d.Summaries(ctx, b, map[int]string{1: "n1"}); got[1].Text != "b1" {
		t.Errorf("session b read back %+v", got[1])
	}
}

func TestDropSummaries(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1"}
	if e := d.PutSummaries(ctx, s, []Summary{
		{Turn: 0, Text: "session"},
		{Turn: 1, NodeID: "n1", Text: "one"},
	}); e != nil {
		t.Fatal(e)
	}
	if e := d.DropSummaries(ctx, s); e != nil {
		t.Fatal(e)
	}
	if got := d.Summaries(ctx, s, map[int]string{1: "n1"}); len(got) != 0 {
		t.Fatalf("summaries survived the drop: %+v", got)
	}
}

// --force replaces what a session has. Turns that no longer generate a summary
// must not keep the text they had.
func TestReplaceSummariesDropsWhatIsNotRewritten(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1"}
	if e := d.PutSummaries(ctx, s, []Summary{
		{Turn: 0, Text: "old session"},
		{Turn: 1, NodeID: "n1", Text: "one"},
		{Turn: 2, NodeID: "n2", Text: "two"},
	}); e != nil {
		t.Fatal(e)
	}
	if e := d.ReplaceSummaries(ctx, s, []Summary{
		{Turn: 0, Text: "new session"},
		{Turn: 1, NodeID: "n1", Text: "one again"},
	}); e != nil {
		t.Fatal(e)
	}
	got := d.Summaries(ctx, s, map[int]string{1: "n1", 2: "n2"})
	if got[1].Text != "one again" || got[0].Text != "new session" {
		t.Errorf("the replacement did not land: %+v", got)
	}
	if _, ok := got[2]; ok {
		t.Errorf("turn 2 survived a replace that did not rewrite it: %+v", got[2])
	}
}

// Replacing with nothing must not empty the table: a generation that produced
// no usable summary would otherwise destroy what was already paid for.
func TestReplaceSummariesWithNothingKeepsWhatIsStored(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1"}
	if e := d.PutSummaries(ctx, s, []Summary{{Turn: 1, NodeID: "n1", Text: "one"}}); e != nil {
		t.Fatal(e)
	}
	if e := d.ReplaceSummaries(ctx, s, nil); e != nil {
		t.Fatal(e)
	}
	if got := d.Summaries(ctx, s, map[int]string{1: "n1"}); got[1].Text != "one" {
		t.Fatalf("an empty replace emptied the table: %+v", got)
	}
}

// The guard against regenerating in a loop asks a narrower question than
// Summaries, which folds a failed read into "nothing is stored".
func TestSummarizedAt(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1"}
	if _, ok := d.SummarizedAt(ctx, s); ok {
		t.Error("an unsummarized session reports a time")
	}
	before := time.Now().Add(-2 * time.Second)
	if e := d.PutSummaries(ctx, s, []Summary{{Turn: 1, NodeID: "n1", Text: "one"}}); e != nil {
		t.Fatal(e)
	}
	got, ok := d.SummarizedAt(ctx, s)
	if !ok {
		t.Fatal("a summarized session reports no time")
	}
	if got.Before(before) {
		t.Errorf("SummarizedAt=%s, before the write", got)
	}
	// Another session's rows do not count.
	if _, ok := d.SummarizedAt(ctx, domain.Session{PluginID: "p", SessionID: "other"}); ok {
		t.Error("a different session reports a time")
	}
}
