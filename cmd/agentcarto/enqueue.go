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
	// The budget is spent on sessions this run actually opens, and the sessions
	// it can rule out without opening do not count against it. Capping the
	// candidate list first — before knowing which of them are already done —
	// stops the sweep dead: with the list in a fixed order, once the first
	// max_per_run of them are summarized every later run picks the same ones,
	// queues nothing, and never reaches the rest.
	opened := 0
	for _, s := range pickForSummary(sessions, time.Now()) {
		if !worthOpening(ctx, db, q, s) {
			continue
		}
		if opened >= cfg.Summary.MaxPerRun {
			break
		}
		queueOne(ctx, a, db, q, s)
		opened++
	}
}

// spawnWorker is how a worker is started. It is a variable so that a test can
// drive a command end to end without the test binary re-executing itself.
var spawnWorker = detachWorker

// StartSummaryWorker starts a detached worker if the queue holds work and none
// is running. Commands call it on their way out, once whatever they were asked
// for has been printed.
//
// A run with no cache starts nothing. --no-cache says not to touch the store on
// this run, and a worker is a process that writes summaries into it — one whose
// own cache the flag would not reach, since it is started detached and reads the
// configuration for itself.
func StartSummaryWorker(cfg config.Config, db *cache.DB) {
	if cfg.Summary.Agent == "" || db == nil {
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
	StartSummaryWorker(cfg, db)
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
// Oldest first, because a summary is the one thing that outlives everything it
// was made from. Agents rotate their own history away after a month or so; the
// cache keeps a copy of the conversation, but Enforce evicts those when it is
// over its size and Prune drops what is left after max_age. Nothing collects
// summaries — neither of those touches the table — and a summary is a kilobyte
// against a conversation of a hundred times that. So an old session is the one
// closer to having nothing left to summarize from, and it goes first.
//
// The newest sessions are not left out in the cold by this. `show` summarizes
// the session it was asked for on the spot, so the ones being read get theirs at
// the moment they are read. This background sweep is for the rest, and there the
// order that matters is which will still be summarizable tomorrow.
//
// Idle ones only: a session being worked in right now would be summarized and
// then immediately outgrow it.
//
// The per-run budget is not applied here. What it has to bound is the number of
// sessions a run opens, and whether a session needs opening is not known until
// the cheap checks in worthOpening have been made — see EnqueueSummaries.
//
// One limit worth naming: Status is only filled in by DetectActive, which only
// `active` runs. From the command line every session therefore looks idle, so a
// session someone is sitting in with its last turn complete is queued like any
// other and its session summary is remade after the next prompt. The `tooSoon`
// guard holds that to once an hour, which is the price of not making every
// completed turn wait out settleBefore.
func pickForSummary(sessions []domain.Session, now time.Time) []domain.Session {
	var out []domain.Session
	for _, s := range sessions {
		if s.EmptyFork {
			continue // nothing that was ever continued
		}
		// A session whose log is gone is not excluded here. The cache keeps the
		// conversation, and App.Conversation reads a deleted session back from it,
		// so such a session can still be summarized — and it is the one with the
		// most to lose, since a summary is a kilobyte the cache never evicts while
		// the conversation it came from is a hundred times that and does get
		// evicted. Whether a copy was in fact kept is worthOpening's question: it
		// takes a row lookup, and this function does not read the store.
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
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	return out
}

// worthOpening answers, without opening the session, whether summarizing it
// could add anything.
//
// Deciding it properly takes the conversation, and a scan runs on every command
// and every few seconds in the TUI — so the sessions that plainly need nothing
// have to be recognized without a parse, or that answer costs a parse of
// everything on the machine every time.
func worthOpening(ctx context.Context, db *cache.DB, q *summary.Queue, s domain.Session) bool {
	if _, queued := q.Find(s.PluginID, s.SessionID); queued {
		return false // already waiting; the request holds the prompt already
	}
	// The log is gone, so the cached conversation is the only thing left to read.
	// HasArtifact asks the same question App.Conversation will — same session,
	// same fingerprint, same parser version — without reading the bytes.
	if s.LogDeleted && !db.HasArtifact(ctx, s, app.ConversationArtifactKind) {
		return false
	}
	// Turn 0 carries the fingerprint the session's summaries were made from, and
	// comes back without needing the turns (it is the one row not matched on a
	// node). Same fingerprint means the log has not moved since, so there is
	// nothing new to summarize.
	if stored := db.Summaries(ctx, s, nil); len(stored) > 0 {
		if sum, ok := stored[0]; ok && sum.Fingerprint == s.Fingerprint {
			return false
		}
	}
	return true
}

// queueOne writes the request for one session, and reports whether it did.
// Parsing the conversation is the expensive part and happens here, in the
// program that already has the plugin running — the worker would otherwise have
// to launch plugins of its own, and the two could disagree about what the turns
// are.
func queueOne(ctx context.Context, a *app.App, db *cache.DB, q *summary.Queue, s domain.Session) bool {
	if !worthOpening(ctx, db, q, s) {
		return false
	}
	conv, err := a.Conversation(ctx, s)
	if err != nil || conv == nil {
		// A plugin that is down reads the same way as a session with nothing in
		// it, so this one is not recorded: it is the answer that might be
		// different tomorrow.
		return false
	}
	turns := transcript.Turns(*conv, conv.ActivePath())
	if len(turns) == 0 {
		return markNothingToSummarize(ctx, db, s)
	}
	nodes := summary.NodesByTurn(turns)
	want := pendingTurns(turns, db.Summaries(ctx, s, nodes))
	if len(want) == 0 {
		return false
	}
	r := summary.Request{PluginID: s.PluginID, SessionID: s.SessionID, Queued: time.Now(), Nodes: nodes}
	for _, batch := range summary.Batch(*conv, turns, want, s.CWD) {
		doc, asked := summary.Prompt(s, *conv, turns, summary.Options{Turns: summary.TurnSet(batch)})
		if len(asked) == 0 {
			continue
		}
		r.Batches = append(r.Batches, asked)
		r.Prompts = append(r.Prompts, doc)
	}
	if len(r.Prompts) == 0 {
		return markNothingToSummarize(ctx, db, s)
	}
	return q.Add(r) == nil
}

// markNothingToSummarize records that this session, as it stands, renders to no
// document — it holds only commands, or injected messages, or no turns at all.
// It always reports false: nothing was queued.
//
// Without the record such a session is opened again on every run, since the only
// cheap way to recognize a session as done is the fingerprint on turn 0. It
// would also hold a place in the per-run budget forever, and enough of them at
// the old end of the list would stop the sweep from ever reaching the rest.
//
// The row carries no text, which is what makes it invisible: every reader tests
// a summary for content before printing it, so a blank one shows nowhere. If the
// log grows the fingerprint no longer matches and the session is reconsidered.
func markNothingToSummarize(ctx context.Context, db *cache.DB, s domain.Session) bool {
	_ = db.PutSummaries(ctx, s, []cache.Summary{{Turn: 0}})
	return false
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
		// Waiting out a failure. Trying again here would repeat it on every run —
		// but saying nothing leaves a reader to wonder why the session it just
		// asked for is the one session without a summary.
		if r.Attempts >= summary.MaxAttempts {
			fmt.Fprintf(os.Stderr, "agentcarto: summarizing this session failed %d times and is no longer retried — `agentcarto summarize %s` reports why\n", r.Attempts, ref)
		} else {
			fmt.Fprintf(os.Stderr, "agentcarto: summarizing this session failed; it is tried again in %s, or now with `agentcarto summarize %s`\n",
				(summary.RetryAfter - time.Since(r.LastTried)).Round(time.Minute), ref)
		}
		return false
	}
	if len(r.Prompts) > 1 {
		if _, _, err := summary.LockHolder(lockPath()); err == nil {
			// Already being drained; this session is in the queue for it.
			fmt.Fprintf(os.Stderr, "agentcarto: %d turns to summarize, queued behind a run already going — `agentcarto show %s --turns N` reads a turn now\n", turnsIn(r), ref)
			return false
		}
		// The worker is started on the way out of show, not here: showCmd defers
		// one, and starting a second from inside would spawn a process that
		// takes the lock's refusal and exits, writing a line to the log for
		// nothing.
		fmt.Fprintf(os.Stderr, "agentcarto: %d turns to summarize, running in the background — `agentcarto show %s --turns N` reads a turn now, and the summaries appear here on a later run\n",
			turnsIn(r), ref)
		return false
	}
	gen, err := summary.New(cfg.Summary.Agent, cfg.Summary.Model, time.Duration(cfg.Summary.Timeout))
	if err != nil {
		return false
	}
	// The request leaves the queue before the call, not after. A worker started
	// by an earlier command reads the queue once and then works through it for
	// minutes; taking the request out now is what stops it from generating this
	// same session alongside — it checks each request is still there before
	// spending on it. Nothing is lost if this fails: the request goes back below.
	_ = q.Done(r)
	fmt.Fprintf(os.Stderr, "agentcarto: summarizing %d turns of this session (about half a minute)…\n", turnsIn(r))
	out, err := gen.Generate(ctx, summary.System, r.Prompts[0])
	if err == nil {
		var res summary.Result
		if res, err = summary.Parse(out, r.Batches[0]); err == nil {
			err = storeSummaries(ctx, db, s, res, r.Nodes, gen.Name(), false)
		}
	}
	if err != nil {
		// Back in the queue with the failure recorded, so a worker picks it up
		// later rather than show trying again on every run.
		_ = q.Retry(r, time.Now())
		fmt.Fprintf(os.Stderr, "agentcarto: could not summarize this session: %v\n", err)
		return false
	}
	return true
}

func turnsIn(r summary.Request) int {
	n := 0
	for _, b := range r.Batches {
		n += len(b)
	}
	return n
}
