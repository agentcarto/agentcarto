package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/core/domain"
)

// staleRequest is how long a queued request stays worth doing. Past this the
// session has almost certainly changed, and summarizing it would pay for a
// prompt built from a conversation that no longer looks like that.
const staleRequest = 48 * time.Hour

// summarizeWorkerCmd drains the summary queue. It is not in the help: the host
// starts it, detached, and a person has `agentcarto summarize` for doing one
// session by hand.
//
// Nothing about it is interactive. Its only output is a log file, because the
// process that started it is expected to be gone by the time anything goes
// wrong — that separation is the whole reason it exists. Summarizing a long
// session takes fifteen minutes, and a TUI closed at minute two would otherwise
// take the work down with it, having already paid for what it had done.
func summarizeWorkerCmd(ctx context.Context, cfg config.Config, db *cache.DB, args []string) {
	fs := flag.NewFlagSet("summarize-worker", flag.ExitOnError)
	front := fs.Bool("front", false, "log to stderr as well, and do not detach")
	parseFlags(fs, args)

	log, closeLog := openWorkerLog(*front)
	defer closeLog()

	// Kill is how `doctor` says to end a run, and a machine going down sends the same
	// signal. Left to the default, the process ends on the spot: the lock and the call
	// it is making both outlive it, and the call goes on being paid for.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Summary.Agent == "" {
		// Not an error: the host only queues work when this is set, so reaching
		// here means the setting changed after something was queued.
		fmt.Fprintf(log, "%s worker: summaries are switched off (summary.agent is empty)\n", stamp())
		return
	}
	if db == nil {
		fmt.Fprintf(log, "%s worker: no cache, nowhere to store summaries\n", stamp())
		return
	}
	q, err := summary.OpenQueue(queueDir())
	if err != nil {
		fmt.Fprintf(log, "%s worker: %v\n", stamp(), err)
		return
	}
	lock, err := summary.TakeLock(lockPath())
	if err != nil {
		// The common case, and not a failure: another worker is draining the
		// same queue and will reach whatever this one would have done.
		fmt.Fprintf(log, "%s worker: %v\n", stamp(), err)
		return
	}
	defer lock.Release()
	// A worker that was killed leaves its record behind, and it would be read as
	// this worker's until the first call.
	_ = q.ClearWorking()
	defer func() { _ = q.ClearWorking() }()

	if n := q.Sweep(staleRequest, time.Now()); n > 0 {
		fmt.Fprintf(log, "%s worker: dropped %d stale or unreadable requests\n", stamp(), n)
	}
	// --front runs under a terminal, so its calls stay where the terminal can reach them.
	newGenerator := summary.NewDetached
	if *front {
		newGenerator = summary.New
	}
	gen, err := newGenerator(cfg.Summary.Agent, cfg.Summary.Model, time.Duration(cfg.Summary.Timeout))
	if err != nil {
		fmt.Fprintf(log, "%s worker: %v\n", stamp(), err)
		return
	}

	// max_per_run bounds what one run spends, and what it spends is calls. A
	// request that turns out to need none — one whose session another run
	// summarized minutes ago — costs a single query, so counting it would let a
	// queue full of those use the whole budget and generate nothing, while the
	// requests behind them wait for a run that never reaches them.
	reqs, _ := q.List()
	done := 0
	for i, r := range reqs {
		if !r.Ready(time.Now()) {
			// Failed recently, or a record of having given up.
			continue
		}
		if done >= cfg.Summary.MaxPerRun {
			fmt.Fprintf(log, "%s worker: stopping at %d sessions (summary.max_per_run); %d still queued\n",
				stamp(), done, len(reqs)-i)
			break
		}
		if ctx.Err() != nil {
			break
		}
		// The queue was read once, minutes ago for a long run. `show` takes a
		// request out before generating that session itself, so one that is gone
		// is one somebody else is paying for right now, and one that came back has
		// failed there and is waiting out what that earned it.
		if now, still := q.Peek(r.PluginID, r.SessionID); !still || !now.Ready(time.Now()) {
			continue
		}
		if runRequest(ctx, log, q, db, gen, r, time.Duration(cfg.Summary.SessionInterval), cfg.Summary.Language) {
			done++
		}
	}
	if ctx.Err() != nil {
		fmt.Fprintf(log, "%s worker: stopped by a signal; what it had not finished is asked for again\n", stamp())
		return
	}
	if done > 0 {
		fmt.Fprintf(log, "%s worker: finished %d sessions\n", stamp(), done)
	}
}

// stopGrace is how long a failed call waits to learn whether it failed because the
// worker is being stopped. Delivering a signal to this process takes far less, and
// only a failure ever waits.
const stopGrace = 250 * time.Millisecond

// beingStopped reports whether the worker is being stopped, given that a call has
// just failed. Going by ctx alone would be a race: a signal sent to the whole group,
// as a machine going down does, reaches the CLI too, and its exit can be seen first.
// That would record the session as having failed for something it had no part in.
func beingStopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(stopGrace):
		return false
	}
}

// runRequest summarizes one queued session, and reports whether it spent a call
// on it — which is what the run's budget counts. A failed call counts: it was
// made, and may well have been billed.
//
// The request leaves the queue in every case that retrying cannot improve — a
// summarized session, and a session whose generation failed for a reason that
// will fail again — because a request that stays costs another call on the next
// run.
func runRequest(ctx context.Context, log io.Writer, q *summary.Queue, db *cache.DB, gen summary.Generator, r summary.Request, sessionEvery time.Duration, language string) bool {
	// The fingerprint travels with the request rather than being looked up here.
	// It has to be the version the prompts were built from — the session may have
	// grown while the request waited — and it is what every reader compares
	// against to say whether a summary predates the newest turns.
	s := domain.Session{PluginID: r.PluginID, SessionID: r.SessionID, Fingerprint: r.Fingerprint}
	// Summarized after this request was written, so what it asked for has been
	// answered — by a `show` that generated the session itself, or by a run that
	// reached it first.
	//
	// The question is deliberately "since this was queued" and not "recently
	// enough": a request here can be one somebody asked for by name. `show`
	// leaves a session needing several calls to the worker and tells the reader
	// the summaries will appear, and a window applied here would drop that
	// request unsummarized and make that a lie. Nothing else needs a window:
	// queueOne writes a request only for a turn that has no summary, so a session
	// with nothing new to describe is never queued in the first place.
	if when, ok := db.SummarizedAt(ctx, s); ok && when.After(r.Accounted()) {
		fmt.Fprintf(log, "%s %s/%s: summarized %s after this was queued, skipping\n", stamp(), r.PluginID, short8(r.SessionID), when.Sub(r.Accounted()).Round(time.Second))
		_ = q.Done(r)
		return false
	}

	all := summary.Result{Turns: map[int]string{}}
	// whole is the session summary — and the headline written beside it — of a
	// call that saw every turn there is. Only such a call can describe the
	// session; see storeSessionSummary.
	var whole summary.Result
	asked, calls := 0, 0
	for i, prompt := range r.Prompts {
		if i < r.Answered {
			continue
		}
		if ctx.Err() != nil {
			return true // leave the request queued: this is not the session's fault
		}
		// Failing to say so costs a status line, which is no reason to give up a
		// call the queue is waiting on.
		_ = q.MarkWorking(r, i+1, len(r.Prompts), time.Now())
		out, err := gen.Generate(ctx, summary.System(language), prompt)
		calls++
		if err != nil && beingStopped(ctx) {
			// Being stopped says nothing about the session. As a failure it would wait
			// out the backoff, and three stops would have it give up.
			fmt.Fprintf(log, "%s %s/%s: call %d of %d stopped with the worker\n", stamp(), r.PluginID, short8(r.SessionID), i+1, len(r.Prompts))
			return true
		}
		if err != nil {
			// Keep what earlier calls stored, and put the request back with the
			// failure recorded. Dropping it would mean the next scan queues the
			// session again and the same call is paid for again, over and over.
			fmt.Fprintf(log, "%s %s/%s: call %d of %d failed: %v\n", stamp(), r.PluginID, short8(r.SessionID), i+1, len(r.Prompts), err)
			_ = q.Retry(r, time.Now(), err)
			return true
		}
		res, err := summary.Parse(out, r.Batches[i])
		if err != nil {
			fmt.Fprintf(log, "%s %s/%s: call %d of %d: %v\n", stamp(), r.PluginID, short8(r.SessionID), i+1, len(r.Prompts), err)
			_ = q.Retry(r, time.Now(), err)
			return true
		}
		asked += len(r.Batches[i])
		if res.Session != "" && len(r.Prompts) == 1 && asked == len(r.Nodes) {
			whole = summary.Result{Session: res.Session, Headline: res.Headline}
		}
		for n, text := range res.Turns {
			all.Turns[n] = text
		}
		// The turns only. What this call said about the session waits for the
		// decision below: an answer built from part of a session describes those
		// turns, not the session.
		if err := storeSummaries(ctx, db, s, summary.Result{Turns: res.Turns}, r.Nodes, gen.Name(), false); err != nil {
			fmt.Fprintf(log, "%s %s/%s: storing call %d failed: %v\n", stamp(), r.PluginID, short8(r.SessionID), i+1, err)
			_ = q.Retry(r, time.Now(), err)
			return true
		}
		// After the store: an answer recorded as in hand and then lost would never
		// be asked for again.
		r.Answered, r.AnsweredAt = i+1, time.Now()
		_ = q.Advance(r)
	}

	// Turn 0 is where a session records the version it has answered, and
	// worthOpening reads exactly that: a session recorded as answered is not
	// opened again until its log moves. Both writes below land on turn 0 — the
	// session summary carries the fingerprint with it, and the marker is nothing
	// but the fingerprint.
	//
	// So neither happens for a request that held a turn back. That turn still
	// needs asking about, and the log it is in has just ended — it has no reason
	// to move again, and nothing would ever look at the session a second time.
	// The session summary waits for the run that does answer the whole version.
	if r.Held {
		fmt.Fprintf(log, "%s %s/%s: %d turns in %d calls with %s (a turn is still settling)\n",
			stamp(), r.PluginID, short8(r.SessionID), len(all.Turns), calls, gen.Name())
		_ = q.Done(r)
		return calls > 0
	}
	spent, unwritten := storeSessionSummary(ctx, db, gen, s, r.Nodes, whole, sessionEvery, language, log, func() {
		// Failing to say so costs a status line, as it does for the turns' calls.
		_ = q.MarkWorking(r, 0, 0, time.Now())
	})
	if spent {
		calls++
	}
	// A request that held nothing else has nothing left to wait for. The scan asks
	// again once the summary is due, and not before: sessionSummaryOverdue goes by the
	// same interval.
	if errors.Is(unwritten, errSessionSummaryNotDue) && len(r.Prompts) == 0 {
		_ = q.Done(r)
		return calls > 0
	}
	// Stopped with the session's summary still owed, so the request stays, and the
	// next worker has only that call to make. Once it is written the stop changes
	// nothing: the request has been answered. Not being due is the ordinary outcome
	// of a run, and not a failure to wait on.
	if unwritten != nil && !errors.Is(unwritten, errSessionSummaryNotDue) && beingStopped(ctx) {
		return calls > 0
	}
	// A request with no turn prompts existed only to remake a session summary that
	// had been left behind (sessionSummaryOverdue). Finishing it unwritten would
	// have the next scan ask for the same thing within seconds, and a call that
	// keeps failing would be paid for at that rate; the queue's backoff is what
	// paces a retry.
	if unwritten != nil && len(r.Prompts) == 0 {
		fmt.Fprintf(log, "%s %s/%s: the session summary is still owed: %v\n", stamp(), r.PluginID, short8(r.SessionID), unwritten)
		_ = q.Retry(r, time.Now(), unwritten)
		return calls > 0
	}
	// Recording the version matters most when the answer held nothing. Parse
	// drops a turn the model declined to describe rather than failing — an answer
	// with @@SESSION and no @@TURN parses clean with no turns — and then nothing
	// above writes a row. Without this the turn is still wanted, the request is
	// written again, and a scan every few seconds pays for that call every few
	// seconds.
	_ = db.MarkExamined(context.WithoutCancel(ctx), s)

	fmt.Fprintf(log, "%s %s/%s: %d turns in %d calls with %s\n", stamp(), r.PluginID, short8(r.SessionID), len(all.Turns), calls, gen.Name())
	_ = q.Done(r)
	return calls > 0
}

// openWorkerLog appends to the worker's log, which is the only place its
// failures surface. It is private: the errors it records carry whatever the
// agent's CLI printed, which is diagnostic output from a program handling the
// user's credentials.
func openWorkerLog(front bool) (io.Writer, func()) {
	f, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return os.Stderr, func() {}
	}
	if front {
		return io.MultiWriter(f, os.Stderr), func() { f.Close() }
	}
	return f, func() { f.Close() }
}

func stamp() string { return time.Now().Format("2006-01-02 15:04:05") }

func short8(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// summaryRoot is where the queue, the lock and the log live. It is the cache's
// own directory, except in tests, which point it at somewhere they can look at
// afterwards rather than at the machine's real queue.
var summaryRoot string

func summaryDir() string {
	if summaryRoot != "" {
		return summaryRoot
	}
	return filepath.Dir(cache.Path())
}
func queueDir() string { return filepath.Join(summaryDir(), "summarize-queue") }
func lockPath() string { return filepath.Join(summaryDir(), "summarize.lock") }
func logPath() string  { return filepath.Join(summaryDir(), "summarize.log") }
