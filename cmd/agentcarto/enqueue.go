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

// EnqueueSummaries queues the sessions that have no summary. It is what makes
// summaries appear without anyone asking: scanning already happens on every
// run, and this rides along.
//
// Nothing here waits, and nothing here generates. A turn that finished is
// queued as soon as a scan sees it — there is no window a session has to sit
// out first. What bounds the spending is that queueOne only writes a request
// when there is a turn without a summary, and that a run opens no more sessions
// than the worker will process.
//
// The pace that does exist is the session summary's, and it is applied where
// that summary is written rather than here (see storeSessionSummary): holding
// the whole session back to pace one of its summaries is what made a finished
// turn wait an hour to be described.
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
	// Half the budget from each end of the list, taken in turn.
	//
	// The oldest sessions are the ones with a deadline: a summary outlives the log
	// an agent rotates away and the conversation the cache evicts, and once both
	// are gone it can no longer be made. The newest are the ones being read. Spent
	// entirely on the old end — which is what this did — a machine with a backlog
	// summarizes its history while the sessions on screen have nothing, and the
	// TUI, unlike `show`, does not generate what it is missing.
	//
	// The turn passes on an open rather than on a candidate, so an end that is all
	// cheap skips gives up none of its half.
	//
	// The budget counts sessions this run opens; the ones it can rule out without
	// opening cost it nothing. Capping the candidate list first — before knowing
	// which of them are already done — stops the sweep dead: the first max_per_run
	// of them get summarized, and every later run picks the same ones, queues
	// nothing, and never reaches the rest.
	cand := pickForSummary(sessions)
	opened, oldEnd := 0, true
	for lo, hi := 0, len(cand)-1; lo <= hi && opened < cfg.Summary.MaxPerRun; {
		s := cand[lo]
		if oldEnd {
			lo++
		} else {
			s, hi = cand[hi], hi-1
		}
		if _, didOpen := queueOne(ctx, a, db, q, s); didOpen {
			opened, oldEnd = opened+1, !oldEnd
		}
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
// The order is oldest first, which is the order of what is about to become
// impossible: a summary outlives everything it was made from — agents rotate
// their own history away after a month or so, Enforce evicts the cached
// conversation when the store is over its size, and Prune drops what is left
// after max_age — while nothing collects summaries, and one is a kilobyte
// against a conversation of a hundred times that.
//
// EnqueueSummaries reads this list from both ends, not just the old one. What
// the order gives it is which end is which; see the split there.
//
// Every session is a candidate, including the one an agent is working in right
// now. What must not be summarized is a turn still being written, not a session
// someone is sitting in, and that is decided per turn where the turns are known
// — see summarizableTurns. Held back by session, as this was, the finished turns
// of the session being read waited for the unfinished one.
//
// Status is not consulted at all any more, which is also what makes the CLI and
// the TUI agree: DetectActive only runs for the TUI and `active`, so from a
// plain command every session looked unattached regardless.
//
// The per-run budget is not applied here. What it has to bound is the number of
// sessions a run opens, and whether a session needs opening is not known until
// the cheap checks in worthOpening have been made — see EnqueueSummaries.
//
// The price of taking a session that is being worked in is that it is opened and
// parsed on every scan while it grows. Its finished turns are described as they
// arrive, which is the point; what is paced is the session's own summary, and
// that is paced where it is written (storeSessionSummary).
func pickForSummary(sessions []domain.Session) []domain.Session {
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

// queueOne writes the request for one session. It reports whether it wrote one,
// and whether deciding took opening the session — which is what a run's budget
// counts, since the sessions it can rule out cheaply cost it nothing.
//
// Parsing the conversation is the expensive part and happens here, in the
// program that already has the plugin running — the worker would otherwise have
// to launch plugins of its own, and the two could disagree about what the turns
// are.
func queueOne(ctx context.Context, a *app.App, db *cache.DB, q *summary.Queue, s domain.Session) (queued, opened bool) {
	if !worthOpening(ctx, db, q, s) {
		return false, false
	}
	conv, err := a.Conversation(ctx, s)
	if err != nil || conv == nil {
		// A plugin that is down reads the same way as a session with nothing in
		// it, so this one is not recorded: it is the answer that might be
		// different tomorrow.
		return false, true
	}
	path := conv.ActivePath()
	turns := transcript.Turns(*conv, path)
	if len(turns) == 0 {
		return markNothingToSummarize(ctx, db, s), true
	}
	open := hasOpenTurn(turns, path, s.LastKind, time.Since(s.UpdatedAt))
	// The nodes of the turns this request can be about, which is every turn but
	// one being written. It is also what tells a run whether a single call
	// covered the session: measured against every turn, a session with a turn in
	// progress would never look covered, and the run would pay for a second call
	// to remake a session summary the first call had just made.
	nodes := summary.NodesByTurn(summarizableTurns(turns, open))
	want := pendingTurns(turns, db.Summaries(ctx, s, nodes), open)
	if len(want) == 0 {
		// Every turn is described already. The log moved — worthOpening let this
		// through — but not in a way that added anything to say, so record the
		// version it moved to rather than opening the session again next run.
		return markNothingToSummarize(ctx, db, s), true
	}
	r := summary.Request{PluginID: s.PluginID, SessionID: s.SessionID, Queued: time.Now(), Fingerprint: s.Fingerprint, Nodes: nodes}
	for _, batch := range summary.Batch(*conv, turns, want, s.CWD) {
		doc, asked := summary.Prompt(s, *conv, turns, summary.Options{Turns: summary.TurnSet(batch)})
		if len(asked) == 0 {
			continue
		}
		r.Batches = append(r.Batches, asked)
		r.Prompts = append(r.Prompts, doc)
	}
	if len(r.Prompts) == 0 {
		return markNothingToSummarize(ctx, db, s), true
	}
	return q.Add(r) == nil, true
}

// markNothingToSummarize records that this session, as it stands, adds nothing
// to summarize — it renders to no document at all, every turn it has is already
// described, or the only turn left is the one being written. It always reports
// false: nothing was queued.
//
// What it writes is the session's current fingerprint against turn 0, which is
// the only cheap way to recognize a session as done: worthOpening reads exactly
// that. Without the record such a session is opened again on every run, holds a
// place in the per-run budget forever, and enough of them at the old end of the
// list stops the sweep from ever reaching the rest.
//
// MarkExamined rather than PutSummaries, for two reasons. Whatever text turn 0
// holds stays: that row is a summary somebody paid for, and a session that grew
// by a turn holding nothing to describe would otherwise have its session summary
// destroyed by the act of noticing that it had nothing to add. And the time the
// session was last summarized stays where it is — see MarkExamined, which a
// session being worked in reaches on every scan.
func markNothingToSummarize(ctx context.Context, db *cache.DB, s domain.Session) bool {
	_ = db.MarkExamined(ctx, s)
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
	_, _ = queueOne(ctx, a, db, q, s)
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
	// spending on it.
	//
	// A call that returns puts the request back below. One that does not — this
	// process killed mid-generation — loses the record of how often the session
	// has failed, and the next scan queues it afresh. That costs a backoff, not a
	// summary.
	_ = q.Done(r)
	fmt.Fprintf(os.Stderr, "agentcarto: summarizing %d turns of this session (about half a minute)…\n", turnsIn(r))
	out, err := gen.Generate(ctx, summary.System, r.Prompts[0])
	if err == nil {
		var res summary.Result
		if res, err = summary.Parse(out, r.Batches[0]); err == nil {
			// The turns, and the session summary only if this call saw the whole
			// session or the interval has passed — the same rule the worker
			// follows, for the same reason (storeSessionSummary).
			if err = storeSummaries(ctx, db, s, summary.Result{Turns: res.Turns}, r.Nodes, gen.Name(), false); err == nil {
				var whole string
				if len(r.Batches[0]) == len(r.Nodes) {
					whole = res.Session
				}
				storeSessionSummary(ctx, db, gen, s, r.Nodes, whole, time.Duration(cfg.Summary.SessionInterval), os.Stderr)
				// The version is answered either way — see the worker, which does
				// the same for the same reason.
				_ = db.MarkExamined(ctx, s)
			}
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
