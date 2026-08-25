package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

// summarizeCmd generates the per-turn summaries that show prints, for one
// session, and waits for them. It is the only path that spends money on
// purpose: everything else displays what is already stored.
//
// Only the turns that have none are asked about, so a session that grew since
// it was last summarized costs the new turns rather than all of them.
func summarizeCmd(ctx context.Context, a *app.App, cfg config.Config, db *cache.DB, args []string, w io.Writer) {
	// Like the other commands: the scan this runs queues whatever else needs
	// summarizing, and a worker takes it once this session is done. Without it
	// the requests this command's own scan added would sit until some other
	// command happened to run, and age out of the queue after two days.
	defer StartSummaryWorker(cfg, db)
	fs := flag.NewFlagSet("summarize", flag.ExitOnError)
	source := fs.String("source", "", "the session's log path, when an id is ambiguous")
	force := fs.Bool("force", false, "discard the stored summaries and make them again")
	quiet := fs.Bool("quiet", false, "print only what was generated, not the summaries themselves")
	positional := parseFlags(fs, args)
	if len(positional) > 1 {
		fail(fmt.Errorf("summarize: one session at a time, got %d (%s)", len(positional), strings.Join(positional, " ")))
	}
	ref := strings.Join(positional, "")
	switch {
	case ref == "" && *source == "":
		fail(fmt.Errorf("summarize: name a session (agentcarto summarize 8f3a2b1c) or pass --source"))
	case ref != "" && *source != "":
		fail(fmt.Errorf("summarize: give a session id or --source, not both"))
	}
	if db == nil {
		// --no-cache leaves nowhere to put the result, and paying for a summary
		// that is thrown away on exit is worse than refusing.
		fail(fmt.Errorf("summarize: the cache is where summaries live — run without --no-cache"))
	}
	gen, err := summary.New(cfg.Summary.Agent, cfg.Summary.Model, time.Duration(cfg.Summary.Timeout))
	if err != nil {
		fail(fmt.Errorf("summarize: %w", err))
	}

	sessions := app.FilterSessions(scanSessions(ctx, a, cfg, db), app.SessionFilter{IncludeEmptyForks: true})
	var s domain.Session
	if *source != "" {
		s, err = app.ResolveSource(sessions, *source)
	} else {
		s, err = app.ResolveSession(sessions, ref)
	}
	if err != nil {
		fail(fmt.Errorf("summarize: %w", err))
	}
	conv, err := a.Conversation(ctx, s)
	if err != nil {
		fail(fmt.Errorf("summarize %s: %w", s.SessionID, err))
	}
	if conv == nil {
		fail(fmt.Errorf("summarize %s: the plugin returned no conversation", s.SessionID))
	}
	path := conv.ActivePath()
	turns := transcript.Turns(*conv, path)
	if len(turns) == 0 {
		fail(fmt.Errorf("summarize %s: the session holds no turns", s.SessionID))
	}
	nodes := summary.NodesByTurn(turns)

	// --force asks about every turn, but nothing is dropped until the new
	// summaries are in hand: dropping first would lose what was already paid for
	// whenever the generation fails, and a mistyped model name is enough.
	//
	// A turn still being written is left out even here. Asking for it by name
	// does not stop the store from withholding a summary whose node moved, so
	// --force would buy a summary nobody ever sees.
	open := hasOpenTurn(turns, path, s.LastKind, time.Since(s.UpdatedAt))
	want := map[int]bool{}
	if *force {
		for _, t := range summarizableTurns(turns, open) {
			want[t.Index+1] = true
		}
	} else {
		want = pendingTurns(turns, db.Summaries(ctx, s, nodes), open)
	}
	if len(want) == 0 {
		if open && len(turns) == 1 {
			fmt.Fprintf(w, "%s: its only turn is still being written — summarize it once the agent is done\n", s.SessionID)
			return
		}
		done := len(summarizableTurns(turns, open))
		if open {
			fmt.Fprintf(w, "%s: all %d finished turns are summarized already; the last one is still being written\n", s.SessionID, done)
			return
		}
		fmt.Fprintf(w, "%s: all %d turns are summarized already (use --force to make them again)\n", s.SessionID, done)
		return
	}

	batches := summary.Batch(*conv, turns, want, s.CWD)
	if len(batches) == 0 {
		// Every turn without a summary renders to nothing a reader can see, so
		// there is nothing to ask about. Saying so beats a call that returns
		// nothing and looks like a failure.
		fmt.Fprintf(w, "%s: the %d turns without a summary hold nothing to summarize\n", s.SessionID, len(want))
		return
	}
	if len(batches) > 1 {
		// A long session is asked about in runs: one call's answer has to fit
		// the model's output limit, and an answer cut at the limit loses every
		// turn after the cut while costing the same.
		fmt.Fprintf(w, "%s: %d turns, split across %d calls\n", s.SessionID, len(want), len(batches))
	}

	all := summary.Result{Turns: map[int]string{}}
	var asked []int
	for i, batch := range batches {
		doc, batchAsked := summary.Prompt(s, *conv, turns, summary.Options{Turns: summary.TurnSet(batch)})
		if len(batchAsked) == 0 {
			continue
		}
		asked = append(asked, batchAsked...)

		out, err := gen.Generate(ctx, summary.System, doc)
		if err != nil {
			stopAfter(w, all, i, len(batches), fmt.Errorf("summarize %s: %w", s.SessionID, err))
		}
		res, err := summary.Parse(out, batchAsked)
		if err != nil {
			// The call is already paid for. Handing the answer back is the only
			// way its content is not lost outright.
			fmt.Fprintf(os.Stderr, "--- what %s returned (already paid for) ---\n%s\n---\n", gen.Name(), out)
			stopAfter(w, all, i, len(batches), fmt.Errorf("summarize %s: %w", s.SessionID, err))
		}
		// The last batch's session summary wins: it saw the most recent turns.
		if res.Session != "" {
			all.Session = res.Session
		}
		for n, text := range res.Turns {
			all.Turns[n] = text
		}
		// Each batch is stored before the next one starts, so a run that fails
		// halfway keeps what it paid for. Every batch upserts — even under
		// --force — because replacing here would delete the turns the later
		// batches have not regenerated yet, and a failure after that would leave
		// them with neither an old summary nor a new one.
		if err := storeSummaries(ctx, db, s, res, nodes, gen.Name(), false); err != nil {
			printSummaries(w, res)
			stopAfter(w, all, i, len(batches), fmt.Errorf("summarize %s: %w", s.SessionID, err))
		}
	}

	// The session summary that came back describes only the turns that call saw.
	// That is the whole session when one call asked about all of it, and a
	// fragment otherwise — a split session, or an incremental run that asked
	// about the two turns which had grown since last time. Whenever the store
	// holds more than was asked about, the summary is made again from the turn
	// summaries: all of them, for a fraction of what rereading the session costs.
	stored := db.Summaries(ctx, s, nodes)
	if len(stored) > len(asked)+1 { // +1: turn 0 is in the store but never asked about
		whole := map[int]string{}
		for n, sum := range stored {
			if n != 0 {
				whole[n] = sum.Text
			}
		}
		for n, text := range all.Turns {
			whole[n] = text // the ones just made, in case the read raced the write
		}
		if sum := sessionSummary(ctx, gen, s, whole, w); sum != "" {
			all.Session = sum
		}
	}
	if *force {
		// --force wants the turns it no longer generates gone. That cleanup can
		// only happen once everything is in hand: doing it up front, or per
		// batch, is what would lose summaries when a later call fails.
		//
		// And not at all when a turn was held back. The turn being written was
		// never offered to this run, so it is not in `all`; replacing with `all`
		// would delete the summary it has from before it reopened — one this run
		// cannot remake, and no later run will either while the turn stays open.
		if err := storeSummaries(ctx, db, s, all, nodes, gen.Name(), !open); err != nil {
			printSummaries(w, all)
			fail(fmt.Errorf("summarize %s: %w", s.SessionID, err))
		}
	} else if all.Session != "" {
		if err := storeSummaries(ctx, db, s, summary.Result{Session: all.Session}, nodes, gen.Name(), false); err != nil {
			fail(fmt.Errorf("summarize %s: %w", s.SessionID, err))
		}
	}

	fmt.Fprintf(w, "%s: summarized %d of %d turns with %s\n", s.SessionID, len(all.Turns), len(asked), gen.Name())
	if missing := all.Missing(asked); len(missing) > 0 {
		// Not an error: the turns that did come back are stored, and asking again
		// costs only the ones that did not.
		fmt.Fprintf(w, "  no summary came back for %s — run summarize again for those\n", turnList(missing))
	}
	if all.Session == "" {
		// The session's own summary is rewritten whenever any turn is, so its
		// absence leaves a stale one in place. Missing only covers turns.
		fmt.Fprintln(w, "  no session summary came back — the stored one still describes an earlier state")
	}
	if *quiet {
		return
	}
	printSummaries(w, all)
}

// sessionSummary asks for the session's own summary from the turn summaries.
// A failure here is reported and shrugged off: the turn summaries are stored
// and useful on their own, and losing the run over the line that describes them
// would be worse than leaving that line as it was.
func sessionSummary(ctx context.Context, gen summary.Generator, s domain.Session, turns map[int]string, w io.Writer) string {
	out, err := gen.Generate(ctx, summary.SessionSystem, summary.SessionPrompt(s, turns))
	if err == nil {
		var res summary.Result
		if res, err = summary.Parse(out, nil); err == nil {
			return res.Session
		}
	}
	fmt.Fprintf(w, "  the turn summaries are stored, but the session summary could not be made: %v\n", err)
	return ""
}

// storeSummaries writes one batch's result. replace is set for the first batch
// of a --force run: the old summaries go out in the same transaction the new
// ones come in, so a failure anywhere leaves the stored set intact rather than
// empty.
func storeSummaries(ctx context.Context, db *cache.DB, s domain.Session, res summary.Result, nodes map[int]string, model string, replace bool) error {
	sums := make([]cache.Summary, 0, len(res.Turns)+1)
	if res.Session != "" {
		// Turn 0 is rewritten whenever any turn is, since adding turns is what
		// makes the session's own summary out of date.
		sums = append(sums, cache.Summary{Turn: 0, Text: res.Session, Model: model})
	}
	for n, text := range res.Turns {
		sums = append(sums, cache.Summary{Turn: n, NodeID: nodes[n], Text: text, Model: model})
	}
	if replace {
		return db.ReplaceSummaries(ctx, s, sums)
	}
	return db.PutSummaries(ctx, s, sums)
}

// stopAfter reports how far a split run got before failing, then exits. What
// was stored stays stored: running summarize again asks only for the turns that
// never came back.
func stopAfter(w io.Writer, all summary.Result, batch, batches int, err error) {
	if batches > 1 {
		fmt.Fprintf(w, "  stored %d turns from %d of %d calls before this failed — summarize again for the rest\n",
			len(all.Turns), batch, batches)
	}
	fail(err)
}

// printSummaries writes what came back, session line first.
func printSummaries(w io.Writer, res summary.Result) {
	if res.Session != "" {
		fmt.Fprintf(w, "\n%s\n", res.Session)
	}
	for _, n := range sortedKeys(res.Turns) {
		fmt.Fprintf(w, "\nTurn %d\n%s\n", n, res.Turns[n])
	}
}

// abandonedAfter is how long a turn has to sit untouched before it counts as
// abandoned rather than in progress.
//
// Nobody is writing a turn whose log stopped moving ten minutes ago: the agent
// was interrupted, or crashed, or the machine went to sleep. Its terminal node
// will never move again, so a summary of it is shown rather than withheld — and
// nothing else will ever ask for that turn, since a log that does not grow keeps
// its fingerprint and the session is never opened again. Waiting ten minutes is
// the cheap side of this trade; waiting forever is not.
const abandonedAfter = 10 * time.Minute

// hasOpenTurn reports whether the last turn in turns is one an agent is still
// writing — one whose terminal node is expected to move again.
//
// Four things have to hold, and anything unknown answers no. The costs are not
// symmetric: a turn wrongly called open is never summarized at all — nothing
// asks for it again — while one wrongly called finished costs a single call
// whose answer the store then withholds.
//
//   - The plugin says what the log ends with. An empty LastKind is not "mid-turn"
//     but "not reported": plugin-copilot reads exported VS Code chat files and
//     leaves it unset, and plugin-grok returns it for a session whose events
//     file is missing.
//   - What it ends with is not a finished turn.
//   - The log moved recently enough that the turn is still being written rather
//     than abandoned — see abandonedAfter.
//   - The turn in question is the one at the end of the branch. transcript.Turns
//     drops a trailing turn holding nothing but a compact summary, and when it
//     does, the last turn it returned is a finished one.
func hasOpenTurn(turns []transcript.Turn, path []string, lastKind domain.EventKind, idleFor time.Duration) bool {
	if lastKind == "" || lastKind == domain.EventTurnComplete {
		return false
	}
	if idleFor >= abandonedAfter {
		return false
	}
	if len(turns) == 0 || len(path) == 0 {
		return false
	}
	last := turns[len(turns)-1].Nodes
	return len(last) > 0 && last[len(last)-1] == path[len(path)-1]
}

// summarizableTurns drops the turn an agent is still writing.
//
// A turn in progress has its terminal node move as the agent writes, and the
// store withholds a summary whose node moved (see cache.Summaries), so paying
// for one buys nothing. Every earlier turn on the branch is final by definition
// — only the last one can be open, which is what hasOpenTurn decides.
func summarizableTurns(turns []transcript.Turn, openLast bool) []transcript.Turn {
	if !openLast || len(turns) == 0 {
		return turns
	}
	return turns[:len(turns)-1]
}

// pendingTurns names the turns that still need a summary: the ones the store
// did not return. The store leaves out both what was never generated and what
// was generated against a different node — a rewind moves a turn number onto
// other content — so a session that was summarized before its branch changed
// asks about the turns that moved, and only those.
//
// The session summary (turn 0) is not a turn here; it is rewritten whenever any
// turn is, since adding turns is what makes it out of date.
func pendingTurns(turns []transcript.Turn, stored map[int]cache.Summary, openLast bool) map[int]bool {
	want := map[int]bool{}
	for _, t := range summarizableTurns(turns, openLast) {
		if _, ok := stored[t.Index+1]; !ok {
			want[t.Index+1] = true
		}
	}
	return want
}

func sortedKeys(m map[int]string) []int {
	out := make([]int, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// turnList renders turn numbers for a message about them ("turn 4", "turns 4, 7").
func turnList(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = fmt.Sprintf("%d", n)
	}
	if len(ns) == 1 {
		return "turn " + parts[0]
	}
	return "turns " + strings.Join(parts, ", ")
}
