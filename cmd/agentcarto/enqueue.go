package main

import (
	"context"
	"fmt"
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

// settleBefore is how long a session whose last turn did not finish has to sit
// still before it is worth summarizing.
//
// The wait is not about the session growing — turns that are already summarized
// stay summarized, and a session that gains a turn is only asked about that
// turn. It is about the turn in progress: summarizing one that is still being
// written pays for a summary whose terminal node is about to change, and the
// store withholds a summary whose node moved. That money buys nothing.
//
// A session whose log ends at a completed turn has no turn in progress, so it
// does not wait at all. On this machine that is 150 of 222 sessions.
const settleBefore = 10 * time.Minute

// EnqueueSummaries queues the sessions that have no summary. It is what makes
// summaries appear without anyone asking: scanning already happens on every
// run, and this rides along.
//
// Nothing here waits, and nothing here generates. The cost of being wrong about
// which sessions are worth summarizing is money, so the choosing is deliberate:
// nothing still being worked in, and never more in one go than the worker will
// actually process.
//
// Starting a worker is StartSummaryWorker's job and deliberately not this one's.
// A scan happens before a command has printed anything, and `show` summarizes
// the session it was asked for itself — a worker started here would be picking
// through the queue while show generates, and the two could land on the same
// session at the same time. Started after the output instead, that window does
// not exist.
func EnqueueSummaries(ctx context.Context, a *app.App, cfg config.Config, db *cache.DB, sessions []domain.Session) {
	if cfg.Summary.Agent == "" || db == nil || len(sessions) == 0 {
		return
	}
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		return
	}
	for _, s := range pickForSummary(sessions, time.Now(), cfg.Summary.MaxPerRun) {
		queueOne(ctx, a, db, q, s)
	}
}

// spawnWorker is how a worker is started. It is a variable so that a test can
// drive a command end to end without the test binary re-executing itself.
var spawnWorker = detachWorker

// StartSummaryWorker starts a detached worker if the queue holds work and none
// is running. Commands call it on their way out, once whatever they were asked
// for has been printed.
func StartSummaryWorker(cfg config.Config) {
	if cfg.Summary.Agent == "" {
		return
	}
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		return
	}
	// Start a worker whenever anything is waiting, not only when this run added
	// to it. A worker takes max_per_run sessions and stops, so the queue is
	// normally left with more than it drained — and the sessions still in it are
	// skipped by queueOne, which would leave nothing to trigger the next run.
	// The queue would stall with work in it.
	if !anyReady(q) {
		return
	}
	// One is already draining the queue. Starting another would spawn a process
	// that takes the lock's refusal and exits — every few seconds, for as long
	// as the first one runs, each spawn writing a line to the log.
	if _, _, err := summary.LockHolder(lockPath()); err == nil {
		return
	}
	// Detached, so quitting the TUI does not take the work with it. A failure to
	// start is silent: the requests stay queued, and the next run tries again.
	_ = spawnWorker()
}

// QueueSummaries queues and then starts a worker, which is what the TUI wants:
// it scans every few seconds and has no "on the way out" to hang the second
// half on.
func QueueSummaries(ctx context.Context, a *app.App, cfg config.Config, db *cache.DB, sessions []domain.Session) {
	EnqueueSummaries(ctx, a, cfg, db, sessions)
	StartSummaryWorker(cfg)
}

// anyReady reports whether the queue holds work a worker would act on now.
// Requests waiting out a failure do not count: starting a worker for those
// would spawn a process that looks at them and exits, every few seconds.
func anyReady(q *summary.Queue) bool {
	reqs, _ := q.List()
	now := time.Now()
	for _, r := range reqs {
		if r.Ready(now) {
			return true
		}
	}
	return false
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
		if s.Status != "" {
			continue // an agent is working in it right now
		}
		// A log that ends at a completed turn has nothing in flight: whatever is
		// there is final, and summarizing it now is not paid for twice.
		if s.LastKind != domain.EventTurnComplete && now.Sub(s.UpdatedAt) < settleBefore {
			continue
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

// summarizeForShow makes the summaries for a session someone just asked to see,
// and reports whether they are ready to print.
//
// It waits when one call will do, which is nearly always: the median session
// summarizes in thirty seconds and the slowest single-call one measured took a
// hundred and five. Whoever ran show wants to read this session — handing back
// an outline of "done", "y" and "1" while the answer is made elsewhere fails
// them at exactly the moment the feature exists for.
//
// A session that needs several calls does go to the background. Those run for
// minutes, which is past what a command should hold anyone for, so it says so
// and names what can be read in the meantime.
func summarizeForShow(ctx context.Context, a *app.App, cfg config.Config, db *cache.DB, s domain.Session, ref string) bool {
	if cfg.Summary.Agent == "" || db == nil {
		return false
	}
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		return false
	}
	// queueOne answers false both for "nothing to do" and for "already queued",
	// and those want opposite things here: a session waiting its turn behind
	// sixty others is exactly the one someone just asked to read. What decides
	// is whether a request exists afterwards, not whether this call created it.
	queueOne(ctx, a, db, q, s)
	r, ok := q.Find(s.PluginID, s.SessionID)
	if !ok {
		return false
	}
	if !r.Ready(time.Now()) {
		// Waiting out a failure. Trying again here would repeat it on every run.
		return false
	}
	if len(r.Prompts) > 1 {
		if _, _, err := summary.LockHolder(lockPath()); err == nil {
			// Already being drained; this session is in the queue for it.
			fmt.Fprintf(os.Stderr, "agentcarto: %d turns to summarize, queued behind a run already going — `agentcarto show %s --turns N` reads a turn now\n", turnsIn(r), ref)
			return false
		}
		if err := spawnWorker(); err != nil {
			return false
		}
		fmt.Fprintf(os.Stderr, "agentcarto: %d turns to summarize, running in the background — `agentcarto show %s --turns N` reads a turn now, and the summaries appear here on a later run\n",
			turnsIn(r), ref)
		return false
	}
	gen, err := summary.New(cfg.Summary.Agent, cfg.Summary.Model, time.Duration(cfg.Summary.Timeout))
	if err != nil {
		return false
	}
	fmt.Fprintf(os.Stderr, "agentcarto: summarizing %d turns of this session (about half a minute)…\n", turnsIn(r))
	out, err := gen.Generate(ctx, summary.System, r.Prompts[0])
	if err == nil {
		var res summary.Result
		if res, err = summary.Parse(out, r.Batches[0]); err == nil {
			err = storeSummaries(ctx, db, s, res, r.Nodes, gen.Name(), false)
		}
	}
	if err != nil {
		// The request stays queued with the failure recorded, so a worker picks
		// it up later rather than show trying again on every run.
		_ = q.Retry(r, time.Now())
		fmt.Fprintf(os.Stderr, "agentcarto: could not summarize this session: %v\n", err)
		return false
	}
	_ = q.Done(r)
	return true
}

func turnsIn(r summary.Request) int {
	n := 0
	for _, b := range r.Batches {
		n += len(b)
	}
	return n
}
