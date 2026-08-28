package cache

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/agentcarto/internal/transcript"
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

func TestSummariesRejectsOldNumbersAfterCompactRenumbering(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "compact", Fingerprint: "f1"}
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Events: []domain.Event{{Kind: domain.EventUser, Text: "one", Prompt: "one"}}},
		{ID: "a1", Parent: "u1", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer one"}}},
		{ID: "compact", Parent: "a1", Events: []domain.Event{{Kind: domain.EventUser, Text: "summary", RawType: domain.RawCompactSummary}}},
		{ID: "u2", Parent: "compact", Events: []domain.Event{{Kind: domain.EventUser, Text: "two", Prompt: "two"}}},
		{ID: "a2", Parent: "u2", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer two"}}},
		{ID: "u3", Parent: "a2", Events: []domain.Event{{Kind: domain.EventUser, Text: "three", Prompt: "three"}}},
		{ID: "a3", Parent: "u3", Events: []domain.Event{{Kind: domain.EventAssistant, Text: "answer three"}}},
	})
	// These rows use the old public numbering: the compact-only boundary consumed
	// turn 2, so the two turns after it were stored as turns 3 and 4.
	if err := d.PutSummaries(ctx, s, []Summary{
		{Turn: 1, NodeID: "a1", Text: "old one"},
		{Turn: 3, NodeID: "a2", Text: "old two"},
		{Turn: 4, NodeID: "a3", Text: "old three"},
	}); err != nil {
		t.Fatal(err)
	}
	turns := transcript.Turns(c, c.ActivePath())
	got := d.Summaries(ctx, s, summary.NodesByTurn(turns))
	if len(got) != 1 || got[1].Text != "old one" {
		t.Fatalf("old compact numbering leaked onto renumbered turns: %#v", got)
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

// What paces the session summary has to look at the session summary alone.
// SummarizedAt reads MAX(created) over every row, so a turn summary stored a
// moment ago makes a session summary from yesterday look fresh.
func TestSessionSummarizedAtIgnoresTurnSummaries(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1", Fingerprint: "fp1"}
	if _, ok := d.SessionSummarizedAt(ctx, s); ok {
		t.Error("a session with nothing stored reports a session summary")
	}
	if e := d.PutSummaries(ctx, s, []Summary{{Turn: 0, Text: "セッション全体", Model: "m"}}); e != nil {
		t.Fatal(e)
	}
	// Move the session summary back a day, then store a turn summary now.
	dayAgo := time.Now().Add(-24 * time.Hour).Unix()
	if _, e := d.db.ExecContext(ctx, "UPDATE summaries SET created=? WHERE turn_index=0", dayAgo); e != nil {
		t.Fatal(e)
	}
	if e := d.PutSummaries(ctx, s, []Summary{{Turn: 3, NodeID: "n3", Text: "ターン3", Model: "m"}}); e != nil {
		t.Fatal(e)
	}
	when, ok := d.SessionSummarizedAt(ctx, s)
	if !ok || when.Unix() != dayAgo {
		t.Errorf("SessionSummarizedAt=%v (%s), want the day-old session summary", ok, when)
	}
	if when, _ := d.SummarizedAt(ctx, s); when.Unix() == dayAgo {
		t.Error("SummarizedAt was expected to move with the turn summary; the two now answer the same question")
	}
}

// A blank turn-0 record is the store's "looked at, nothing to make" marker, not
// a summary. Counting it would put the first session summary an interval after
// a scan happened to look at the session rather than at the first opportunity.
func TestSessionSummarizedAtIgnoresTheBlankRecord(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1", Fingerprint: "fp1"}
	if e := d.MarkExamined(ctx, s); e != nil {
		t.Fatal(e)
	}
	if when, ok := d.SessionSummarizedAt(ctx, s); ok {
		t.Errorf("the examined marker reads as a session summary written at %s", when)
	}
}

// MarkExamined says "looked at, nothing to make", which is a different fact from
// "summarized". It has to record the version it looked at without touching the
// summary that is there or the time one was last made: what paces the session
// summary reads that time, and a session being worked in is examined often.
func TestMarkExaminedRecordsTheVersionWithoutClaimingASummary(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1", Fingerprint: "fp1"}
	if e := d.PutSummaries(ctx, s, []Summary{{Turn: 0, Text: "セッション全体", Model: "m"}}); e != nil {
		t.Fatal(e)
	}
	// Summarized an hour ago, so that a write of `created` here is visible rather
	// than landing in the same second.
	hourAgo := time.Now().Add(-time.Hour).Unix()
	if _, e := d.db.ExecContext(ctx, "UPDATE summaries SET created=? WHERE plugin_id='p' AND session_id='s1'", hourAgo); e != nil {
		t.Fatal(e)
	}
	grown := s
	grown.Fingerprint = "fp2"

	if e := d.MarkExamined(ctx, grown); e != nil {
		t.Fatal(e)
	}

	got := d.Summaries(ctx, grown, nil)
	if got[0].Text != "セッション全体" || got[0].Model != "m" {
		t.Errorf("the summary was overwritten: %q by %q", got[0].Text, got[0].Model)
	}
	if got[0].Fingerprint != "fp2" {
		t.Errorf("fingerprint=%q, want the version that was examined", got[0].Fingerprint)
	}
	when, ok := d.SummarizedAt(ctx, grown)
	if !ok || when.Unix() != hourAgo {
		t.Errorf("SummarizedAt=%v (%s), want the hour-old time it was actually summarized", ok, when)
	}
}

// A session that was never summarized is marked with a blank row, and that row
// must not make it look summarized — neither to a reader nor to the hourly
// guard. created stays 0, which SummarizedAt reads as "never".
func TestMarkExaminedOnASessionWithNoSummaries(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	s := domain.Session{PluginID: "p", SessionID: "s1", Fingerprint: "fp1"}

	if e := d.MarkExamined(ctx, s); e != nil {
		t.Fatal(e)
	}

	if got := d.Summaries(ctx, s, nil); got[0].Text != "" {
		t.Errorf("the blank record holds text: %q", got[0].Text)
	}
	if got := d.Summaries(ctx, s, nil); got[0].Fingerprint != "fp1" {
		t.Errorf("fingerprint=%q, want the version that was examined", got[0].Fingerprint)
	}
	if when, ok := d.SummarizedAt(ctx, s); ok {
		t.Errorf("a session that was only examined reports having been summarized at %s", when)
	}
	if _, ok := d.SummaryTexts(ctx)[s.Key()]; ok {
		t.Error("the blank record came back as searchable text")
	}
}

// A search asks "could this query be in this session's summaries" of everything
// on the machine, so it is answered for every session in one query rather than
// one lookup each. Blank rows — the store's record that a session renders to no
// document — are not text and do not come back.
func TestSummaryTexts(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	one := domain.Session{PluginID: "claude", SessionID: "s1", Fingerprint: "fp1"}
	two := domain.Session{PluginID: "codex", SessionID: "s2", Fingerprint: "fp1"}
	blank := domain.Session{PluginID: "claude", SessionID: "s3", Fingerprint: "fp1"}
	if err := d.PutSummaries(ctx, one, []Summary{
		{Turn: 0, Text: "セッション全体"},
		{Turn: 1, NodeID: "n1", Text: "パレット生成"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.PutSummaries(ctx, two, []Summary{{Turn: 0, Text: "べつの話"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.PutSummaries(ctx, blank, []Summary{{Turn: 0}}); err != nil {
		t.Fatal(err)
	}

	got := d.SummaryTexts(ctx)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want the two that hold text: %v", len(got), got)
	}
	// Every summary of a session is in one string: what a match is in only
	// matters once the session has been chosen.
	text := got[one.Key()]
	if !strings.Contains(text, "セッション全体") || !strings.Contains(text, "パレット生成") {
		t.Errorf("a session's summaries are not all searchable: %q", text)
	}
	if _, ok := got[blank.Key()]; ok {
		t.Error("a blank record came back as searchable text")
	}
	// Sessions of different plugins that share an id stay apart.
	if got[two.Key()] != "べつの話" {
		t.Errorf("codex/s2 = %q", got[two.Key()])
	}
}

// The session summary goes stale two ways, and the second is the one the
// fingerprint cannot see: a worker run that summarized new turns, left the
// session summary alone because summary.session_interval had not elapsed, and
// then called MarkExamined — which brings turn 0's fingerprint up to date while
// its text stays behind.
func TestSessionSummaryStale(t *testing.T) {
	written := time.Now().Add(-time.Hour)
	cur := Summary{Turn: 0, Text: "セッション全体の要約", Fingerprint: "fp", Created: written}
	sess := domain.Session{PluginID: "claude", SessionID: "s", Fingerprint: "fp", UpdatedAt: written.Add(-time.Minute)}
	if cur.Stale(sess) {
		t.Error("a summary written after the session's last write is current")
	}

	grown := sess
	grown.Fingerprint = "fp2"
	if !cur.Stale(grown) {
		t.Error("a log that changed under the summary should read as stale")
	}

	examined := sess // MarkExamined kept the fingerprint current
	examined.UpdatedAt = written.Add(time.Minute)
	if !cur.Stale(examined) {
		t.Error("a session written to after the summary should read as stale even with a matching fingerprint")
	}

	blank := Summary{Turn: 0, Text: "  ", Fingerprint: "old", Created: written}
	if blank.Stale(grown) {
		t.Error("MarkExamined's blank row is not a summary and cannot be stale")
	}
	turn := Summary{Turn: 3, Text: "ターンの要約", Fingerprint: "old", Created: written}
	if turn.Stale(grown) {
		t.Error("a turn summary is withheld when its node moves, so it is never stale")
	}
}

// Summaries reads back when a row was written, which is what tells a stale
// session summary from a current one.
func TestSummariesCarryTheirWriteTime(t *testing.T) {
	d := openTemp(t)
	s := domain.Session{PluginID: "claude", SessionID: "s", Fingerprint: "fp"}
	if e := d.PutSummaries(context.Background(), s, []Summary{{Turn: 0, Text: "要約"}}); e != nil {
		t.Fatalf("put: %v", e)
	}
	got := d.Summaries(context.Background(), s, nil)[0]
	if got.Created.IsZero() {
		t.Fatal("a summary read back without its write time cannot be judged stale")
	}
	if time.Since(got.Created) > time.Minute {
		t.Fatalf("write time is not the time it was written: %v", got.Created)
	}
}
