package main

import (
	"context"
	"os"
	"sort"
	"time"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/platform"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

// detachWorker starts the worker as a process of its own, passing on the
// configuration this one was started with. Without --config the worker reads
// the built-in defaults, where summarizing is off, and does nothing.
func detachWorker() error {
	if configPath != "" {
		return platform.Detach("--config", configPath, "summarize-worker")
	}
	return platform.Detach("summarize-worker")
}

// settleBefore is how long a session must have been idle before it is worth
// summarizing. A session someone is working in right now grows while the
// summary is being made, so what comes back describes a state that no longer
// exists — correctly, since the node ids still match, but at a price paid for
// turns that were about to be joined by more.
const settleBefore = 10 * time.Minute

// QueueSummaries queues the sessions that have no summary and starts a worker
// for them. It is what makes summaries appear without anyone asking: scanning
// already happens on every run, and this rides along.
//
// Nothing here waits, and nothing here generates. The cost of being wrong about
// which sessions are worth summarizing is money, so the choosing is deliberate:
// newest first, nothing still being worked in, and never more in one go than
// the worker will actually process.
func QueueSummaries(ctx context.Context, a *app.App, cfg config.Config, db *cache.DB, sessions []domain.Session) {
	if cfg.Summary.Agent == "" || db == nil || len(sessions) == 0 {
		return
	}
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		return
	}
	queued := 0
	for _, s := range pickForSummary(sessions, time.Now(), cfg.Summary.MaxPerRun) {
		if queueOne(ctx, a, db, q, s) {
			queued++
		}
	}
	if queued == 0 {
		return
	}
	// Detached, so quitting the TUI does not take the work with it. A failure to
	// start is silent: the requests stay queued, and the next run tries again.
	_ = detachWorker()
}

// pickForSummary chooses which sessions to offer the worker.
//
// Newest first, because an older session is one a reader is less likely to come
// back to. Idle ones only: a session being worked in right now would be
// summarized and then immediately outgrow it. And no more than the worker will
// take in one run, so that a first run over a machine's whole history spends a
// bounded amount rather than everything at once.
func pickForSummary(sessions []domain.Session, now time.Time, max int) []domain.Session {
	var out []domain.Session
	for _, s := range sessions {
		if s.LogDeleted || s.EmptyFork {
			continue // nothing to read, or nothing that was ever continued
		}
		if s.Status != "" || now.Sub(s.UpdatedAt) < settleBefore {
			continue // still being worked in
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// queueOne writes the request for one session, and reports whether it did.
// Parsing the conversation is the expensive part and happens here, in the
// program that already has the plugin running — the worker would otherwise have
// to launch plugins of its own, and the two could disagree about what the turns
// are.
func queueOne(ctx context.Context, a *app.App, db *cache.DB, q *summary.Queue, s domain.Session) bool {
	// Both checks below exist to avoid parsing. Deciding whether a session needs
	// summarizing takes its conversation, and a scan runs every few seconds — so
	// the sessions that plainly need nothing have to be recognized without it,
	// or the answer costs a parse of everything on the machine every few
	// seconds.
	if _, queued := q.Find(s.PluginID, s.SessionID); queued {
		return false // already waiting; the request holds the prompt already
	}
	// Turn 0 carries the fingerprint its summaries were made from, and comes
	// back without needing the turns (it is the one row not matched on a node).
	// Same fingerprint means the log has not moved since, so there is nothing
	// new to summarize.
	if stored := db.Summaries(ctx, s, nil); len(stored) > 0 {
		if sum, ok := stored[0]; ok && sum.Fingerprint == s.Fingerprint {
			return false
		}
	}
	conv, err := a.Conversation(ctx, s)
	if err != nil || conv == nil {
		return false
	}
	turns := transcript.Turns(*conv, conv.ActivePath())
	if len(turns) == 0 {
		return false
	}
	nodes := summary.NodesByTurn(turns)
	want := pendingTurns(turns, db.Summaries(ctx, s, nodes))
	if len(want) == 0 {
		return false
	}
	r := summary.Request{PluginID: s.PluginID, SessionID: s.SessionID, Queued: time.Now(), Nodes: nodes}
	// Queueing a session again must not clear the record of it having failed;
	// otherwise every scan would reset the backoff and the retries would be
	// continuous after all.
	if prev, ok := q.Find(s.PluginID, s.SessionID); ok {
		r.Attempts, r.LastTried = prev.Attempts, prev.LastTried
	}
	for _, batch := range summary.Batch(*conv, turns, want, s.CWD) {
		doc, asked := summary.Prompt(s, *conv, turns, summary.Options{Turns: summary.TurnSet(batch)})
		if len(asked) == 0 {
			continue
		}
		r.Batches = append(r.Batches, asked)
		r.Prompts = append(r.Prompts, doc)
	}
	if len(r.Prompts) == 0 {
		return false
	}
	return q.Add(r) == nil
}

// queueOneSession queues a single session and starts a worker, for `show` on a
// session that has no summary. Unlike the scan path this ignores how recently
// the session was touched: someone reading it has said which one they want.
func queueOneSession(ctx context.Context, a *app.App, cfg config.Config, db *cache.DB, s domain.Session) {
	if cfg.Summary.Agent == "" || db == nil {
		return
	}
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		return
	}
	if !queueOne(ctx, a, db, q, s) {
		return
	}
	if err := detachWorker(); err == nil {
		// Only said where a person can see it. The document itself goes to
		// stdout, which is often being read by a program.
		os.Stderr.WriteString("agentcarto: summarizing this session in the background — run show again in a few minutes\n")
	}
}
