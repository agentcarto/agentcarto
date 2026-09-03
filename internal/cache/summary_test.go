package cache

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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
	// Turn 0 and the turns under it were written by the same call, so they share
	// a timestamp: the summary describes every turn there is.
	cur := Summary{Turn: 0, Text: "セッション全体の要約", Fingerprint: "fp", Created: written, TurnSummarizedAt: written}
	sess := domain.Session{PluginID: "claude", SessionID: "s", Fingerprint: "fp", UpdatedAt: written.Add(-time.Minute)}
	if cur.Stale(sess) {
		t.Error("a summary written with the newest turn it describes is current")
	}

	grown := sess
	grown.Fingerprint = "fp2"
	if !cur.Stale(grown) {
		t.Error("a log that changed under the summary should read as stale")
	}

	behind := cur // MarkExamined kept the fingerprint current
	behind.TurnSummarizedAt = written.Add(time.Minute)
	if !behind.Stale(sess) {
		t.Error("a session summary older than the turns under it should read as stale")
	}

	// A log grows for reasons that add no turn to describe — an agent appending
	// its own bookkeeping to a conversation that ended days ago. Measured against
	// the log, such a session read as stale from that moment on and never stopped:
	// nothing summarizes a session whose every turn is already described.
	quiet := sess
	quiet.UpdatedAt = written.Add(48 * time.Hour)
	if cur.Stale(quiet) {
		t.Error("a log that grew without gaining a turn does not make the summary stale")
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

// A headline rides with the session summary it belongs to.
func TestSummaryHeadlineRoundTrip(t *testing.T) {
	d := openTemp(t)
	s := domain.Session{PluginID: "claude", SessionID: "s", Fingerprint: "fp"}
	if e := d.PutSummaries(context.Background(), s, []Summary{
		{Turn: 0, Text: "セッション全体の要約", Headline: "pull-all.shの作成"},
		{Turn: 1, NodeID: "n1", Text: "ターンの要約"},
	}); e != nil {
		t.Fatalf("put: %v", e)
	}
	got := d.Summaries(context.Background(), s, map[int]string{1: "n1"})
	if got[0].Headline != "pull-all.shの作成" {
		t.Errorf("session headline=%q", got[0].Headline)
	}
	if got[1].Headline != "" {
		t.Errorf("a turn summary has no headline, got %q", got[1].Headline)
	}
}

// MarkExamined records that a session was looked at. It must not cost the
// session the headline it already has — that was paid for.
func TestMarkExaminedKeepsTheHeadline(t *testing.T) {
	d := openTemp(t)
	s := domain.Session{PluginID: "claude", SessionID: "s", Fingerprint: "fp"}
	if e := d.PutSummaries(context.Background(), s, []Summary{{Turn: 0, Text: "要約", Headline: "見出し"}}); e != nil {
		t.Fatalf("put: %v", e)
	}
	grown := s
	grown.Fingerprint = "fp2"
	if e := d.MarkExamined(context.Background(), grown); e != nil {
		t.Fatalf("mark: %v", e)
	}
	got := d.Summaries(context.Background(), grown, nil)[0]
	if got.Headline != "見出し" || got.Text != "要約" {
		t.Errorf("MarkExamined overwrote the summary: %+v", got)
	}
}

// The column was added after the table shipped, and the rows in it were paid
// for: an existing cache is migrated in place rather than rebuilt.
func TestOpenAddsTheHeadlineColumnToAnOlderCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	old, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatalf("open raw: %v", e)
	}
	if _, e := old.Exec("CREATE TABLE summaries (plugin_id TEXT, session_id TEXT, turn_index INTEGER, node_id TEXT NOT NULL, summary TEXT NOT NULL, model TEXT NOT NULL, fingerprint TEXT NOT NULL, created INTEGER NOT NULL, PRIMARY KEY(plugin_id,session_id,turn_index))"); e != nil {
		t.Fatalf("create old schema: %v", e)
	}
	if _, e := old.Exec("INSERT INTO summaries VALUES('claude','s',0,'','古い要約','claude claude-sonnet-5','fp',1)"); e != nil {
		t.Fatalf("seed: %v", e)
	}
	if e := old.Close(); e != nil {
		t.Fatalf("close raw: %v", e)
	}

	d, e := Open(path)
	if e != nil {
		t.Fatalf("open: %v", e)
	}
	defer d.Close()
	s := domain.Session{PluginID: "claude", SessionID: "s", Fingerprint: "fp"}
	got := d.Summaries(context.Background(), s, nil)[0]
	if got.Text != "古い要約" {
		t.Fatalf("the migration lost what was already stored: %+v", got)
	}
	if got.Headline != "" {
		t.Errorf("a row from before the column has no headline, got %q", got.Headline)
	}
	if e := d.PutSummaries(context.Background(), s, []Summary{{Turn: 0, Text: "新しい要約", Headline: "見出し"}}); e != nil {
		t.Fatalf("put after migration: %v", e)
	}
	if got := d.Summaries(context.Background(), s, nil)[0]; got.Headline != "見出し" {
		t.Errorf("headline=%q after writing to the migrated table", got.Headline)
	}
}

// Several processes share the cache — a TUI starts its summary worker, and both
// open it. On the first open after a column was added they all try to migrate,
// and none of them may fail for it.
func TestConcurrentOpensMigrateWithoutFailing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	old, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatalf("open raw: %v", e)
	}
	// In the journal mode a cache written by any released version is already in:
	// switching to WAL takes an exclusive lock, and three processes racing to do
	// it is not the migration this test is about.
	if _, e := old.Exec("PRAGMA journal_mode=WAL"); e != nil {
		t.Fatalf("wal: %v", e)
	}
	if _, e := old.Exec("CREATE TABLE summaries (plugin_id TEXT, session_id TEXT, turn_index INTEGER, node_id TEXT NOT NULL, summary TEXT NOT NULL, model TEXT NOT NULL, fingerprint TEXT NOT NULL, created INTEGER NOT NULL, PRIMARY KEY(plugin_id,session_id,turn_index))"); e != nil {
		t.Fatalf("create old schema: %v", e)
	}
	if e := old.Close(); e != nil {
		t.Fatalf("close raw: %v", e)
	}

	var wg sync.WaitGroup
	errs := make([]error, 3)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			d, e := Open(path)
			errs[i] = e
			if d != nil {
				d.Close()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("open %d failed: %v", i, e)
		}
	}
}

// The time a session summary is measured against comes back with it: the newest
// of the turn summaries the reader is being shown.
func TestSummariesCarryTheNewestTurnTime(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	s := domain.Session{PluginID: "claude", SessionID: "s", Fingerprint: "fp"}
	if e := d.PutSummaries(ctx, s, []Summary{
		{Turn: 0, Text: "セッション全体の要約"},
		{Turn: 1, NodeID: "n1", Text: "ターン1の要約"},
	}); e != nil {
		t.Fatalf("put: %v", e)
	}
	nodes := map[int]string{1: "n1"}
	if got := d.Summaries(ctx, s, nodes)[0]; !got.TurnSummarizedAt.Equal(got.Created) {
		t.Errorf("turns and session summary written together: %v vs %v", got.TurnSummarizedAt, got.Created)
	} else if got.Stale(s) {
		t.Error("a session summary written with its turns is current")
	}

	// The session summary now predates the turn under it, which is what
	// session_interval leaves behind.
	hourAgo := time.Now().Add(-time.Hour).Unix()
	if _, e := d.db.ExecContext(ctx, "UPDATE summaries SET created=? WHERE turn_index=0", hourAgo); e != nil {
		t.Fatalf("age turn 0: %v", e)
	}
	if got := d.Summaries(ctx, s, nodes)[0]; !got.Stale(s) {
		t.Error("a session summary older than the turn under it should read as stale")
	}
	// A turn whose node moved is withheld from the reader, so it says nothing
	// about how far behind the session summary is.
	if got := d.Summaries(ctx, s, map[int]string{1: "moved"})[0]; got.Stale(s) {
		t.Error("a turn summary that is not shown should not make the session summary stale")
	}
}

// The list judges staleness the same way, from the same times, without knowing
// what the turns of each session are.
func TestHeadlinesCarryTheNewestTurnTime(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	s := domain.Session{PluginID: "claude", SessionID: "s", Fingerprint: "fp"}
	if e := d.PutSummaries(ctx, s, []Summary{
		{Turn: 0, Text: "セッション全体の要約", Headline: "要約機能の実装"},
		{Turn: 1, NodeID: "n1", Text: "ターン1の要約"},
	}); e != nil {
		t.Fatalf("put: %v", e)
	}
	if got := d.Headlines(ctx)[s.Key()]; got.Stale(s) {
		t.Error("a headline written with its turns is current")
	}
	hourAgo := time.Now().Add(-time.Hour).Unix()
	if _, e := d.db.ExecContext(ctx, "UPDATE summaries SET created=? WHERE turn_index=0", hourAgo); e != nil {
		t.Fatalf("age turn 0: %v", e)
	}
	if got := d.Headlines(ctx)[s.Key()]; !got.Stale(s) {
		t.Error("a headline older than the turn under it should read as stale")
	}
}

// A session that has no turn summaries at all — one short enough to be described
// by a single answer — has nothing to fall behind, and must not read as stale
// because its log went on growing.
func TestASessionSummaryWithoutTurnsIsNotStale(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	s := domain.Session{PluginID: "claude", SessionID: "s", Fingerprint: "fp"}
	if e := d.PutSummaries(ctx, s, []Summary{{Turn: 0, Text: "セッション全体の要約", Headline: "見出し"}}); e != nil {
		t.Fatalf("put: %v", e)
	}
	grown := s
	grown.UpdatedAt = time.Now().Add(48 * time.Hour)
	if got := d.Summaries(ctx, s, nil)[0]; got.Stale(grown) {
		t.Error("a session summary with no turns under it should not read as stale")
	}
	if got := d.Headlines(ctx)[s.Key()]; got.Stale(grown) {
		t.Error("the same answer is owed to the list")
	}
}

// The session list draws one line per session, so it reads every headline at
// once rather than one per row.
func TestHeadlinesReadsThemAllAtOnce(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	with := domain.Session{PluginID: "claude", SessionID: "with", Fingerprint: "fp"}
	without := domain.Session{PluginID: "claude", SessionID: "without", Fingerprint: "fp"}
	if e := d.PutSummaries(ctx, with, []Summary{
		{Turn: 0, Text: "要約", Headline: "見出し"},
		{Turn: 1, NodeID: "n1", Text: "ターンの要約"},
	}); e != nil {
		t.Fatal(e)
	}
	if e := d.PutSummaries(ctx, without, []Summary{{Turn: 0, Text: "見出しのない要約"}, {Turn: 1, NodeID: "n1", Text: "ターン"}}); e != nil {
		t.Fatal(e)
	}

	got := d.Headlines(ctx)
	if got[with.Key()].Headline != "見出し" {
		t.Errorf("headline for %q = %q", with.SessionID, got[with.Key()].Headline)
	}
	if _, ok := got[without.Key()]; ok {
		t.Errorf("a session with no headline should be absent, got %+v", got[without.Key()])
	}
	if len(got) != 1 {
		t.Errorf("Headlines returned %d entries, want 1: %v", len(got), got)
	}
}

// Headlines reads one row per session out of a table that holds one row per
// turn, and the session list reads it on every scan. Without an index that suits
// it, that is a scan of every turn summary on the machine — so the plan is
// checked rather than left to chance.
func TestHeadlinesReadsThroughItsIndex(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	// A session with a headline among many turn summaries, which is the shape the
	// index exists for.
	s := domain.Session{PluginID: "claude", SessionID: "s", Fingerprint: "fp"}
	sums := []Summary{{Turn: 0, Text: "要約", Headline: "見出し"}}
	for i := 1; i <= 50; i++ {
		sums = append(sums, Summary{Turn: i, NodeID: fmt.Sprintf("n%d", i), Text: "ターンの要約"})
	}
	if e := d.PutSummaries(ctx, s, sums); e != nil {
		t.Fatal(e)
	}

	rows, e := d.db.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT plugin_id,session_id,headline,fingerprint,created FROM summaries WHERE turn_index=0 AND headline <> ''")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if e := rows.Scan(&id, &parent, &notUsed, &detail); e != nil {
			t.Fatal(e)
		}
		plan += detail + "\n"
	}
	if !strings.Contains(plan, "summaries_headline") {
		t.Errorf("the headline read does not use its index — every turn summary is scanned:\n%s", plan)
	}
	if got := d.Headlines(ctx); got[s.Key()].Headline != "見出し" {
		t.Errorf("the index changed what comes back: %+v", got)
	}
}

// The index is built on a column that older caches do not have, so it can only
// be created after the migration that adds it. Opening a cache from before the
// column must build both.
func TestOpenBuildsTheHeadlineIndexOnAnOlderCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	old, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatalf("open raw: %v", e)
	}
	if _, e := old.Exec("CREATE TABLE summaries (plugin_id TEXT, session_id TEXT, turn_index INTEGER, node_id TEXT NOT NULL, summary TEXT NOT NULL, model TEXT NOT NULL, fingerprint TEXT NOT NULL, created INTEGER NOT NULL, PRIMARY KEY(plugin_id,session_id,turn_index))"); e != nil {
		t.Fatalf("create old schema: %v", e)
	}
	if _, e := old.Exec("INSERT INTO summaries VALUES('claude','s',0,'','古い要約','m','fp',1)"); e != nil {
		t.Fatalf("seed: %v", e)
	}
	if e := old.Close(); e != nil {
		t.Fatalf("close raw: %v", e)
	}

	d, e := Open(path)
	if e != nil {
		t.Fatalf("open: %v", e)
	}
	defer d.Close()
	var n int
	if e := d.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='summaries_headline'").Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 1 {
		t.Errorf("the index was not built on a cache that predates the column")
	}
	if got := d.Summaries(context.Background(), domain.Session{PluginID: "claude", SessionID: "s"}, nil)[0].Text; got != "古い要約" {
		t.Errorf("the migration lost what was stored: %q", got)
	}
}
