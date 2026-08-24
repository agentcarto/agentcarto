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

// regenerateWithin is the shortest gap between summarizing the same session
// twice. The store reports a summary as missing when it cannot be read at all —
// a corrupt database reads as "nothing is summarized" — and without this the
// worker would regenerate the same session on every run, spending each time.
const regenerateWithin = time.Hour

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

	reqs, _ := q.List()
	done := 0
	for _, r := range reqs {
		if done >= cfg.Summary.MaxPerRun {
			fmt.Fprintf(log, "%s worker: stopping at %d sessions (summary.max_per_run); %d still queued\n",
				stamp(), done, len(reqs)-done)
			break
		}
		if ctx.Err() != nil {
			return
		}
		runRequest(ctx, log, q, db, gen, r)
		done++
	}
	if done > 0 {
		fmt.Fprintf(log, "%s worker: finished %d sessions\n", stamp(), done)
	}
}

// runRequest summarizes one queued session. The request leaves the queue in
// every case that retrying cannot improve — a summarized session, and a session
// whose generation failed for a reason that will fail again — because a request
// that stays costs another call on the next run.
func runRequest(ctx context.Context, log io.Writer, q *summary.Queue, db *cache.DB, gen summary.Generator, r summary.Request) {
	s := domain.Session{PluginID: r.PluginID, SessionID: r.SessionID}
	when, ok := db.SummarizedAt(ctx, s)
	if tooSoon(when, ok, time.Now()) {
		fmt.Fprintf(log, "%s %s/%s: summarized %s ago, skipping\n", stamp(), r.PluginID, short8(r.SessionID), time.Since(when).Round(time.Minute))
		_ = q.Done(r)
		return
	}

	all := summary.Result{Turns: map[int]string{}}
	for i, prompt := range r.Prompts {
		if ctx.Err() != nil {
			return // leave the request queued: this is not the session's fault
		}
		out, err := gen.Generate(ctx, summary.System, prompt)
		if err != nil {
			// Stop this session but keep what earlier calls stored. The request
			// is dropped rather than retried: whatever failed (not signed in, an
			// unknown model, a session too large for the model) fails the same
			// way next time, and the host queues it again when it is next seen.
			fmt.Fprintf(log, "%s %s/%s: call %d of %d failed: %v\n", stamp(), r.PluginID, short8(r.SessionID), i+1, len(r.Prompts), err)
			break
		}
		res, err := summary.Parse(out, r.Batches[i])
		if err != nil {
			fmt.Fprintf(log, "%s %s/%s: call %d of %d: %v\n", stamp(), r.PluginID, short8(r.SessionID), i+1, len(r.Prompts), err)
			break
		}
		if res.Session != "" {
			all.Session = res.Session
		}
		for n, text := range res.Turns {
			all.Turns[n] = text
		}
		if err := storeSummaries(ctx, db, s, res, r.Nodes, gen.Name(), false); err != nil {
			fmt.Fprintf(log, "%s %s/%s: storing call %d failed: %v\n", stamp(), r.PluginID, short8(r.SessionID), i+1, err)
			break
		}
	}

	if len(r.Prompts) > 1 && len(all.Turns) > 0 {
		// No single call saw the whole session, so none of their session
		// summaries describes it.
		out, err := gen.Generate(ctx, summary.SessionSystem, summary.SessionPrompt(s, all.Turns))
		if err == nil {
			var res summary.Result
			if res, err = summary.Parse(out, nil); err == nil && res.Session != "" {
				err = storeSummaries(ctx, db, s, summary.Result{Session: res.Session}, r.Nodes, gen.Name(), false)
			}
		}
		if err != nil {
			fmt.Fprintf(log, "%s %s/%s: turns stored, session summary failed: %v\n", stamp(), r.PluginID, short8(r.SessionID), err)
		}
	}

	fmt.Fprintf(log, "%s %s/%s: %d turns in %d calls with %s\n", stamp(), r.PluginID, short8(r.SessionID), len(all.Turns), len(r.Prompts), gen.Name())
	_ = q.Done(r)
}

// tooSoon reports whether a session was summarized recently enough that doing
// it again would be waste rather than an update.
//
// The guard exists because Summaries folds an unreadable store into "nothing is
// summarized": a database that cannot be read would otherwise look like an
// unsummarized session on every run and be paid for every time. A session that
// has genuinely grown is still summarized again once the window passes, and
// only its new turns are asked about.
func tooSoon(when time.Time, summarized bool, now time.Time) bool {
	return summarized && now.Sub(when) < regenerateWithin
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

func summaryDir() string { return filepath.Dir(cache.Path()) }
func queueDir() string   { return filepath.Join(summaryDir(), "summarize-queue") }
func lockPath() string   { return filepath.Join(summaryDir(), "summarize.lock") }
func logPath() string    { return filepath.Join(summaryDir(), "summarize.log") }
