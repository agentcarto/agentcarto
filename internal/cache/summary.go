package cache

import (
	"context"
	"strings"
	"time"

	"github.com/agentcarto/core/domain"
)

// Summary is one generated summary line: what happened in a turn, or what the
// session as a whole was about.
//
// Unlike an artifact, a summary is not derived data the host can rebuild for
// free — it was paid for, once, by an API call. That is why summaries live in a
// table of their own rather than under artifacts: artifacts are keyed on the
// session fingerprint, so a session that grew by one line reads back nothing,
// and they are the first thing Enforce evicts when the cache is over budget.
type Summary struct {
	// Turn is 0 for the session's own summary and otherwise the turn number
	// transcript.Turns shows, so a summary can be printed beside the turn a
	// reader is looking at without a second lookup.
	Turn int
	// NodeID is the id of the turn's terminal node, empty for the session
	// summary. Turn numbers are positions along one branch: a rewind or a fork
	// renumbers them, and then turn 7 is no longer the turn that was summarized.
	// The node id is what stays attached to the content.
	NodeID string
	Text   string
	// Model names the agent and model that produced the text ("claude
	// claude-sonnet-5"), shown to the reader and used to decide whether a
	// summary made by a weaker model is worth regenerating.
	Model string
	// Fingerprint is the session fingerprint the row was last written against.
	// For a turn summary that is the version it was made from. For the session
	// summary it is weaker than it looks: MarkExamined upserts the current
	// fingerprint onto turn 0 without rewriting its text, so a fingerprint that
	// matches does not mean the text is current. Stale is what to ask.
	Fingerprint string
	// Created is when the row was written. Zero for a row stored before this
	// field was read back.
	Created time.Time
	// TurnSummarizedAt is when the newest of the session's turn summaries was
	// written, and it is carried by turn 0 alone. It is what tells a session
	// summary that fell behind the turns it is made of from one that is merely
	// older than a log which grew without gaining a turn — see Stale.
	//
	// Zero where the reader did not ask for the turns: worthOpening reads turn 0
	// with no nodes at all, and never asks whether it is stale.
	TurnSummarizedAt time.Time
	// Headline is the session in one line, for where there is room for a line and
	// not a paragraph (the session list, the collapsed detail header). Only turn 0
	// carries one, and only when the model that wrote the summary wrote one too:
	// an older row has none, and what shows it falls back to the summary.
	Headline string
}

// Stale reports whether this session summary describes a session that has since
// gone on. It is only meaningful for turn 0: a turn summary is withheld
// altogether when the node it was made from moves (see Summaries).
//
// Two things can say so, and either is enough. The fingerprint catches a log
// the worker has not looked at since it changed — which is every session being
// written to right now, whose newest turns nothing has been asked about yet. The
// turn summaries catch what the fingerprint cannot: a worker run that summarized
// the new turns but left the session summary alone (summary.session_interval had
// not elapsed) and then called MarkExamined, which brings the fingerprint up to
// date while the text stays behind.
//
// What is deliberately not asked is whether the log is newer than the text. A
// log grows for reasons that add no turn to describe — an agent appending its
// own bookkeeping to a conversation that ended days ago — and the scan that finds
// nothing to summarize records the version through MarkExamined without
// rewriting turn 0. Measured against the log, such a session would read as stale
// from that moment until it is next summarized, which is never: nothing will ask
// about a session whose every turn is already described.
func (s Summary) Stale(sess domain.Session) bool {
	// A row with neither text nor headline is MarkExamined's note that the
	// session was looked at, not a summary. One that carries only a headline is
	// what Headlines reads back, and it goes stale like any other.
	if s.Turn != 0 || (strings.TrimSpace(s.Text) == "" && strings.TrimSpace(s.Headline) == "") {
		return false
	}
	if s.Fingerprint != sess.Fingerprint {
		return true
	}
	// Both times are stored to the second, and a session summary written in the
	// same call as the turns under it shares theirs: only a turn described after
	// this text was written makes it stale.
	return !s.Created.IsZero() && s.Created.Before(s.TurnSummarizedAt)
}

// PutSummaries stores summaries for one session, replacing any row that already
// holds the same turn number. Callers write whole sessions on a first run and
// single turns when a session has grown, so the write is an upsert rather than a
// replace-all: the turns that did not change keep the text they already have.
func (d *DB) PutSummaries(ctx context.Context, s domain.Session, sums []Summary) error {
	return d.putSummaries(ctx, s, sums, false)
}

// ReplaceSummaries stores summaries as the only ones the session has, dropping
// what was there before in the same transaction.
//
// It is what "summarize --force" needs. Dropping first and generating after
// would lose what was already paid for whenever the generation fails — a
// mistyped model name is enough — and dropping outside a transaction would lose
// it whenever the write fails. Here there is no window in which the old
// summaries are gone and the new ones are not yet stored.
func (d *DB) ReplaceSummaries(ctx context.Context, s domain.Session, sums []Summary) error {
	return d.putSummaries(ctx, s, sums, true)
}

func (d *DB) putSummaries(ctx context.Context, s domain.Session, sums []Summary, replace bool) error {
	if len(sums) == 0 {
		// Even a replace does nothing here: emptying the table because a
		// generation produced nothing would throw away usable summaries.
		return nil
	}
	tx, e := d.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	if replace {
		if _, e = tx.ExecContext(ctx, "DELETE FROM summaries WHERE plugin_id=? AND session_id=?", s.PluginID, s.SessionID); e != nil {
			return e
		}
	}
	now := time.Now().Unix()
	for _, sum := range sums {
		// Columns are named rather than positional: the table gains one now and
		// then (headline did), and a positional insert breaks silently when it does.
		if _, e = tx.ExecContext(ctx,
			"INSERT INTO summaries (plugin_id,session_id,turn_index,node_id,summary,model,fingerprint,created,headline) VALUES(?,?,?,?,?,?,?,?,?)"+
				" ON CONFLICT(plugin_id,session_id,turn_index) DO UPDATE SET node_id=excluded.node_id,summary=excluded.summary,model=excluded.model,fingerprint=excluded.fingerprint,created=excluded.created,headline=excluded.headline",
			s.PluginID, s.SessionID, sum.Turn, sum.NodeID, sum.Text, sum.Model, s.Fingerprint, now, sum.Headline); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// Summaries returns the summaries stored for a session, keyed by turn number.
//
// nodeByTurn names, for every turn the caller is about to show, the id of that
// turn's terminal node. A turn's summary comes back only when the stored node id
// is the one named there: showing a summary against a turn it was not made from
// is worse than showing none, and unlike an artifact this table is deliberately
// not gated on the session fingerprint (a session that grew by a turn must keep
// the summaries of the turns that did not change). The session summary, turn 0,
// carries no node and always comes back.
//
// Turns absent from the result are the ones that still need generating — there
// is no separate query for that.
func (d *DB) Summaries(ctx context.Context, s domain.Session, nodeByTurn map[int]string) map[int]Summary {
	rows, e := d.db.QueryContext(ctx, "SELECT turn_index,node_id,summary,model,fingerprint,created,headline FROM summaries WHERE plugin_id=? AND session_id=?", s.PluginID, s.SessionID)
	if e != nil {
		return nil
	}
	defer rows.Close()
	out := map[int]Summary{}
	var newestTurn time.Time
	for rows.Next() {
		var sum Summary
		var created int64
		if e := rows.Scan(&sum.Turn, &sum.NodeID, &sum.Text, &sum.Model, &sum.Fingerprint, &created, &sum.Headline); e != nil {
			return nil
		}
		if created > 0 {
			sum.Created = time.Unix(created, 0)
		}
		if sum.Turn != 0 {
			if node, ok := nodeByTurn[sum.Turn]; !ok || node != sum.NodeID {
				continue
			}
			// The rows that survive the node check are the turns this session is
			// currently described by, and only those say anything about how far
			// behind turn 0 is: one made against a node that moved describes
			// content the reader is not being shown.
			if sum.Created.After(newestTurn) {
				newestTurn = sum.Created
			}
		}
		out[sum.Turn] = sum
	}
	if rows.Err() != nil {
		return nil
	}
	// After the loop rather than in it: the rows arrive in no particular order,
	// so turn 0 can be read before the turns it has to be measured against.
	if sum, ok := out[0]; ok {
		sum.TurnSummarizedAt = newestTurn
		out[0] = sum
	}
	return out
}

// SummaryTexts returns, for every session that has any, all of its summary text
// joined into one string.
//
// It answers one question — could this query be in this session's summaries —
// for every session at once, because a search asks it of everything on the
// machine and one row lookup each would be a query per session. The text is not
// keyed by turn: what turn a match is in only matters once a session has been
// chosen, and Summaries answers that for the one session, with the node checks
// this cannot do.
func (d *DB) SummaryTexts(ctx context.Context) map[domain.SessionKey]string {
	rows, e := d.db.QueryContext(ctx, "SELECT plugin_id,session_id,summary FROM summaries WHERE summary <> ''")
	if e != nil {
		return nil
	}
	defer rows.Close()
	out := map[domain.SessionKey]string{}
	for rows.Next() {
		var k domain.SessionKey
		var text string
		if rows.Scan(&k.PluginID, &k.SessionID, &text) != nil {
			return nil
		}
		if prev := out[k]; prev != "" {
			out[k] = prev + "\n" + text
		} else {
			out[k] = text
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// Headlines returns every session's one-line headline, for the list that shows
// one line per session.
//
// One query rather than one per session: the list draws every session there is,
// and a lookup per row would be a query per row on every keystroke. Sessions
// without a headline are absent, and the caller falls back to what it showed
// before (the title).
//
// Whole rows rather than the text alone, so a caller can ask Stale. The list is
// where a reader chooses what to open, and a headline that describes a session
// as it was this morning has to say so there too — not only once the session is
// open.
//
// The turn time Stale needs comes from a subquery, which is where this differs
// from Summaries: with no turns to name, the newest turn summary is taken as
// stored rather than as shown, so a row whose node has moved still counts. The
// two answers part only for a session whose branch changed under its summaries,
// and there the fingerprint has moved as well and says stale first.
func (d *DB) Headlines(ctx context.Context) map[domain.SessionKey]Summary {
	rows, e := d.db.QueryContext(ctx,
		"SELECT s.plugin_id,s.session_id,s.headline,s.fingerprint,s.created,"+
			"COALESCE((SELECT MAX(t.created) FROM summaries t WHERE t.plugin_id=s.plugin_id AND t.session_id=s.session_id AND t.turn_index<>0),0)"+
			" FROM summaries s WHERE s.turn_index=0 AND s.headline <> ''")
	if e != nil {
		return nil
	}
	defer rows.Close()
	out := map[domain.SessionKey]Summary{}
	for rows.Next() {
		var k domain.SessionKey
		sum := Summary{Turn: 0}
		var created, turns int64
		if rows.Scan(&k.PluginID, &k.SessionID, &sum.Headline, &sum.Fingerprint, &created, &turns) != nil {
			return nil
		}
		if created > 0 {
			sum.Created = time.Unix(created, 0)
		}
		if turns > 0 {
			sum.TurnSummarizedAt = time.Unix(turns, 0)
		}
		out[k] = sum
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// DropSummaries removes every summary of one session. It is for the reader who
// wants a session summarized again from scratch — with a better model, or after
// the prompt changed — since PutSummaries alone would leave the turns that are
// no longer generated behind.
func (d *DB) DropSummaries(ctx context.Context, s domain.Session) error {
	_, e := d.db.ExecContext(ctx, "DELETE FROM summaries WHERE plugin_id=? AND session_id=?", s.PluginID, s.SessionID)
	return e
}

// MarkExamined records the fingerprint a session was last looked at, without
// claiming anything was summarized.
//
// It is for the session a scan opened and found nothing to ask about: every turn
// it holds is described already, or none of them renders to a document.
// Recording the fingerprint is what stops the next scan from opening it again
// for the same answer.
//
// Recording it through PutSummaries would also stamp created, and the time a
// session was last summarized would come to mean "last looked at". What paces
// the session summary reads such a time (SessionSummarizedAt), so a session
// examined often enough would push its own next summary further away every time
// anything looked at it, rather than measuring from the last summary somebody
// paid for.
//
// Turn 0 is the row it writes: worthOpening reads exactly that one, and the
// per-turn rows keep the fingerprint each summary was actually made from. Where
// there is no row yet, a blank one is created with created=0, which SummarizedAt
// reads as never summarized — which is the truth.
func (d *DB) MarkExamined(ctx context.Context, s domain.Session) error {
	// The update touches the fingerprint alone, so a session that already has a
	// summary keeps its text and its headline.
	_, e := d.db.ExecContext(ctx,
		"INSERT INTO summaries (plugin_id,session_id,turn_index,node_id,summary,model,fingerprint,created,headline) VALUES(?,?,0,'','','',?,0,'')"+
			" ON CONFLICT(plugin_id,session_id,turn_index) DO UPDATE SET fingerprint=excluded.fingerprint",
		s.PluginID, s.SessionID, s.Fingerprint)
	return e
}

// SessionSummarizedAt reports when the session's own summary — turn 0 — was last
// written, and whether it ever was.
//
// It is the narrower question SummarizedAt cannot answer: that one reads
// MAX(created) across every row, so it moves whenever a turn summary is stored.
// What paces the session summary has to look at the session summary alone.
//
// A blank turn-0 record (MarkExamined, created=0) is not a summary and answers
// false, which is what puts the next session summary at the first opportunity
// rather than an interval after a scan happened to look.
func (d *DB) SessionSummarizedAt(ctx context.Context, s domain.Session) (time.Time, bool) {
	var unix int64
	e := d.db.QueryRowContext(ctx,
		"SELECT created FROM summaries WHERE plugin_id=? AND session_id=? AND turn_index=0 AND summary <> ''",
		s.PluginID, s.SessionID).Scan(&unix)
	if e != nil || unix == 0 {
		return time.Time{}, false
	}
	return time.Unix(unix, 0), true
}

// SummarizedAt reports when anything was last stored for a session — a turn
// summary or the session's own — and whether anything ever was.
//
// The worker reads it to tell whether a request it is holding was already
// answered by someone else while it waited. It asks a narrower question than
// Summaries, which folds a failed read into "nothing is stored"; here a failed
// read answers false rather than a misleading zero.
//
// For the session's own summary alone, see SessionSummarizedAt.
func (d *DB) SummarizedAt(ctx context.Context, s domain.Session) (time.Time, bool) {
	var unix int64
	e := d.db.QueryRowContext(ctx, "SELECT MAX(created) FROM summaries WHERE plugin_id=? AND session_id=?", s.PluginID, s.SessionID).Scan(&unix)
	if e != nil || unix == 0 {
		return time.Time{}, false
	}
	return time.Unix(unix, 0), true
}
