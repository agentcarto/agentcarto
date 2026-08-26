package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

	if n := q.Sweep(staleRequest, time.Now()); n > 0 {
		fmt.Fprintf(log, "%s worker: dropped %d stale or unreadable requests\n", stamp(), n)
	}
	gen, err := summary.New(cfg.Summary.Agent, cfg.Summary.Model, time.Duration(cfg.Summary.Timeout))
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
			// Failed recently, or failed often enough to stop. Sweep takes the
			// ones that are done being tried.
			continue
		}
		if done >= cfg.Summary.MaxPerRun {
			fmt.Fprintf(log, "%s worker: stopping at %d sessions (summary.max_per_run); %d still queued\n",
				stamp(), done, len(reqs)-i)
			break
		}
		if ctx.Err() != nil {
			return
		}
		// The queue was read once, minutes ago for a long run. `show` takes a
		// request out before generating that session itself, so one that is gone
		// is one somebody else is paying for right now.
		if _, still := q.Find(r.PluginID, r.SessionID); !still {
			continue
		}
		if runRequest(ctx, log, q, db, gen, r, time.Duration(cfg.Summary.SessionInterval)) {
			done++
		}
	}
	if done > 0 {
		fmt.Fprintf(log, "%s worker: finished %d sessions\n", stamp(), done)
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
func runRequest(ctx context.Context, log io.Writer, q *summary.Queue, db *cache.DB, gen summary.Generator, r summary.Request, sessionEvery time.Duration) bool {
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
	if when, ok := db.SummarizedAt(ctx, s); ok && when.After(r.Queued) {
		fmt.Fprintf(log, "%s %s/%s: summarized %s after this was queued, skipping\n", stamp(), r.PluginID, short8(r.SessionID), when.Sub(r.Queued).Round(time.Second))
		_ = q.Done(r)
		return false
	}

	all := summary.Result{Turns: map[int]string{}}
	// whole is the session summary of a call that saw every turn there is. Only
	// such a call can describe the session; see storeSessionSummary.
	var whole string
	asked := 0
	for i, prompt := range r.Prompts {
		if ctx.Err() != nil {
			return true // leave the request queued: this is not the session's fault
		}
		out, err := gen.Generate(ctx, summary.System, prompt)
		if err != nil {
			// Keep what earlier calls stored, and put the request back with the
			// failure recorded. Dropping it would mean the next scan queues the
			// session again and the same call is paid for again, over and over.
			fmt.Fprintf(log, "%s %s/%s: call %d of %d failed: %v\n", stamp(), r.PluginID, short8(r.SessionID), i+1, len(r.Prompts), err)
			_ = q.Retry(r, time.Now())
			return true
		}
		res, err := summary.Parse(out, r.Batches[i])
		if err != nil {
			fmt.Fprintf(log, "%s %s/%s: call %d of %d: %v\n", stamp(), r.PluginID, short8(r.SessionID), i+1, len(r.Prompts), err)
			_ = q.Retry(r, time.Now())
			return true
		}
		asked += len(r.Batches[i])
		if res.Session != "" && len(r.Prompts) == 1 && asked == len(r.Nodes) {
			whole = res.Session
		}
		for n, text := range res.Turns {
			all.Turns[n] = text
		}
		// The turns only. What this call said about the session waits for the
		// decision below: an answer built from part of a session describes those
		// turns, not the session.
		if err := storeSummaries(ctx, db, s, summary.Result{Turns: res.Turns}, r.Nodes, gen.Name(), false); err != nil {
			fmt.Fprintf(log, "%s %s/%s: storing call %d failed: %v\n", stamp(), r.PluginID, short8(r.SessionID), i+1, err)
			_ = q.Retry(r, time.Now())
			return true
		}
	}

	calls := len(r.Prompts)
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
		return true
	}
	if storeSessionSummary(ctx, db, gen, s, r.Nodes, whole, sessionEvery, log) {
		calls++
	}
	// Recording the version matters most when the answer held nothing. Parse
	// drops a turn the model declined to describe rather than failing — an answer
	// with @@SESSION and no @@TURN parses clean with no turns — and then nothing
	// above writes a row. Without this the turn is still wanted, the request is
	// written again, and a scan every few seconds pays for that call every few
	// seconds.
	_ = db.MarkExamined(ctx, s)

	fmt.Fprintf(log, "%s %s/%s: %d turns in %d calls with %s\n", stamp(), r.PluginID, short8(r.SessionID), len(all.Turns), calls, gen.Name())
	_ = q.Done(r)
	return true
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
