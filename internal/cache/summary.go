package cache

import (
	"context"
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
	// Fingerprint is the session fingerprint the summary was made from. The
	// session may have grown since — this table is deliberately not gated on it
	// — so a reader that wants to say "this summary predates the last few turns"
	// compares it against the session's own fingerprint.
	Fingerprint string
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
		if _, e = tx.ExecContext(ctx,
			"INSERT INTO summaries VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(plugin_id,session_id,turn_index) DO UPDATE SET node_id=excluded.node_id,summary=excluded.summary,model=excluded.model,fingerprint=excluded.fingerprint,created=excluded.created",
			s.PluginID, s.SessionID, sum.Turn, sum.NodeID, sum.Text, sum.Model, s.Fingerprint, now); e != nil {
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
	rows, e := d.db.QueryContext(ctx, "SELECT turn_index,node_id,summary,model,fingerprint FROM summaries WHERE plugin_id=? AND session_id=?", s.PluginID, s.SessionID)
	if e != nil {
		return nil
	}
	defer rows.Close()
	out := map[int]Summary{}
	for rows.Next() {
		var sum Summary
		if e := rows.Scan(&sum.Turn, &sum.NodeID, &sum.Text, &sum.Model, &sum.Fingerprint); e != nil {
			return nil
		}
		if sum.Turn != 0 {
			if node, ok := nodeByTurn[sum.Turn]; !ok || node != sum.NodeID {
				continue
			}
		}
		out[sum.Turn] = sum
	}
	if rows.Err() != nil {
		return nil
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
// It is for the session a scan opened and found nothing to ask about — every
// finished turn already described, and the one still being written not
// summarizable yet. Recording the fingerprint is what stops the next scan from
// opening it again for the same answer.
//
// Recording it through PutSummaries would also stamp created, and SummarizedAt
// would come to mean "last looked at" rather than "last summarized". The hourly
// guard reads that time, so a session being worked in — which reaches this on
// every scan, since its newest turn is always the open one — would hold itself
// off for an hour each time it was examined, and its finished turns would never
// be summarized.
//
// Turn 0 is the row it writes: worthOpening reads exactly that one, and the
// per-turn rows keep the fingerprint each summary was actually made from. Where
// there is no row yet, a blank one is created with created=0, which SummarizedAt
// reads as never summarized — which is the truth.
func (d *DB) MarkExamined(ctx context.Context, s domain.Session) error {
	_, e := d.db.ExecContext(ctx,
		"INSERT INTO summaries VALUES(?,?,0,'','','',?,0) ON CONFLICT(plugin_id,session_id,turn_index) DO UPDATE SET fingerprint=excluded.fingerprint",
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
