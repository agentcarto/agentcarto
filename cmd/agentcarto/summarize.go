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
	turns := transcript.Turns(*conv, conv.ActivePath())
	if len(turns) == 0 {
		fail(fmt.Errorf("summarize %s: the session holds no turns", s.SessionID))
	}
	nodes := summary.NodesByTurn(turns)

	// --force asks about every turn, but nothing is dropped until the new
	// summaries are in hand: dropping first would lose what was already paid for
	// whenever the generation fails, and a mistyped model name is enough.
	want := map[int]bool{}
	if *force {
		for _, t := range turns {
			want[t.Index+1] = true
		}
	} else {
		want = pendingTurns(turns, db.Summaries(ctx, s, nodes))
	}
	if len(want) == 0 {
		fmt.Fprintf(w, "%s: all %d turns are summarized already (use --force to make them again)\n", s.SessionID, len(turns))
		return
	}

	doc, asked := summary.Prompt(s, *conv, turns, summary.Options{Turns: want})
	if len(asked) == 0 {
		// Every turn without a summary renders to nothing a reader can see, so
		// there is nothing to ask about. Saying so beats a call that returns
		// nothing and looks like a failure.
		fmt.Fprintf(w, "%s: the %d turns without a summary hold nothing to summarize\n", s.SessionID, len(want))
		return
	}

	out, err := gen.Generate(ctx, summary.System, doc)
	if err != nil {
		fail(fmt.Errorf("summarize %s: %w", s.SessionID, err))
	}
	res, err := summary.Parse(out, asked)
	if err != nil {
		// The call is already paid for. Handing the answer back is the only way
		// its content is not lost outright.
		fmt.Fprintf(os.Stderr, "--- what %s returned (already paid for) ---\n%s\n---\n", gen.Name(), out)
		fail(fmt.Errorf("summarize %s: %w", s.SessionID, err))
	}

	sums := make([]cache.Summary, 0, len(res.Turns)+1)
	if res.Session != "" {
		// Turn 0 is rewritten whenever any turn is, since adding turns is what
		// makes the session's own summary out of date.
		sums = append(sums, cache.Summary{Turn: 0, Text: res.Session, Model: gen.Name()})
	}
	for n, text := range res.Turns {
		sums = append(sums, cache.Summary{Turn: n, NodeID: nodes[n], Text: text, Model: gen.Name()})
	}
	store := db.PutSummaries
	if *force {
		// Replace rather than upsert, so that turns which no longer generate a
		// summary do not keep the text they had. The drop happens inside the
		// same transaction as the write.
		store = db.ReplaceSummaries
	}
	if err := store(ctx, s, sums); err != nil {
		// Print what was generated before giving up: it was paid for, and this
		// is the last place it exists.
		printSummaries(w, res)
		fail(fmt.Errorf("summarize %s: %w", s.SessionID, err))
	}

	fmt.Fprintf(w, "%s: summarized %d of %d turns with %s\n", s.SessionID, len(res.Turns), len(asked), gen.Name())
	if missing := res.Missing(asked); len(missing) > 0 {
		// Not an error: the turns that did come back are stored, and asking again
		// costs only the ones that did not.
		fmt.Fprintf(w, "  no summary came back for %s — run summarize again for those\n", turnList(missing))
	}
	if res.Session == "" {
		// The session's own summary is rewritten whenever any turn is, so its
		// absence leaves a stale one in place. Missing only covers turns.
		fmt.Fprintln(w, "  no session summary came back — the stored one still describes an earlier state")
	}
	if *quiet {
		return
	}
	printSummaries(w, res)
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

// pendingTurns names the turns that still need a summary: the ones the store
// did not return. The store leaves out both what was never generated and what
// was generated against a different node — a rewind moves a turn number onto
// other content — so a session that was summarized before its branch changed
// asks about the turns that moved, and only those.
//
// The session summary (turn 0) is not a turn here; it is rewritten whenever any
// turn is, since adding turns is what makes it out of date.
func pendingTurns(turns []transcript.Turn, stored map[int]cache.Summary) map[int]bool {
	want := map[int]bool{}
	for _, t := range turns {
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
