package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/cache"
	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

// showCmd prints a session as Markdown. Without a selection it prints the
// outline only: a session runs to hundreds of kilobytes, and an agent that asks
// for one by id should not have its context filled by the answer. The turn
// numbers in the outline are the ones `search` reports and the TUI shows.
func showCmd(ctx context.Context, a *app.App, cfg config.Config, db *cache.DB, args []string, w io.Writer) {
	// Deferred so it runs after summarizeForShow below rather than beside it: a
	// worker picking through the queue while this generates could land on the
	// same session, and both would pay for it. It is also the only place show
	// starts one, including for a session too long to summarize inline.
	defer StartSummaryWorker(cfg, db)
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	source := fs.String("source", "", "the session's log path, when an id is ambiguous")
	turnSpec := fs.String("turns", "", `which turns to print ("12", "12-14", "3,7,12-14")`)
	last := fs.Int("last", 0, "print the last N turns")
	all := fs.Bool("all", false, "print every turn")
	tools := fs.String("tools", "label", `how much of a tool call to print: "label" (name and one-line argument), "full" (multi-line calls in full), "none"`)
	positional := parseFlags(fs, args)
	if len(positional) > 1 {
		fail(fmt.Errorf("show: one session at a time, got %d (%s)", len(positional), strings.Join(positional, " ")))
	}
	ref := strings.Join(positional, "")
	switch {
	case ref == "" && *source == "":
		fail(fmt.Errorf("show: name a session (agentcarto show 8f3a2b1c) or pass --source"))
	case ref != "" && *source != "":
		// Silently preferring one of them would hide a disagreement between the
		// two, and the id is the part a caller is likely to have got wrong.
		fail(fmt.Errorf("show: give a session id or --source, not both"))
	}
	if *last < 0 {
		fail(fmt.Errorf("show: --last %d: a count of turns cannot be negative", *last))
	}
	selectors := 0
	for _, on := range []bool{*turnSpec != "", *last > 0, *all} {
		if on {
			selectors++
		}
	}
	if selectors > 1 {
		fail(fmt.Errorf("show: use one of --turns, --last, --all"))
	}
	opts, err := toolOptions(*tools)
	if err != nil {
		fail(err)
	}

	sessions := app.FilterSessions(scanSessions(ctx, a, cfg, db), app.SessionFilter{IncludeEmptyForks: true})
	var s domain.Session
	if *source != "" {
		s, err = app.ResolveSource(sessions, *source)
	} else {
		s, err = app.ResolveSession(sessions, ref)
	}
	if err != nil {
		fail(fmt.Errorf("show: %w", err))
	}
	conv, err := a.Conversation(ctx, s)
	if err != nil {
		fail(fmt.Errorf("show %s: %w", s.SessionID, err))
	}
	if conv == nil {
		fail(fmt.Errorf("show %s: the plugin returned no conversation", s.SessionID))
	}
	path := conv.ActivePath()
	turns := transcript.Turns(*conv, path)
	opts.Branches = transcript.Branches(*conv, path)
	opts.Summaries, opts.SummaryModel, opts.SummaryStale = storedSummaries(ctx, db, s, turns)

	// Summarizing comes before printing, not after: whoever ran this asked to
	// read the session, and an outline of "done", "y" and "1" is not that.
	if len(opts.Summaries) == 0 && summarizeForShow(ctx, a, cfg, db, s, ref) {
		opts.Summaries, opts.SummaryModel, opts.SummaryStale = storedSummaries(ctx, db, s, turns)
	}
	if selectors == 0 {
		// An outline with no `↳` under any turn is the shape this feature exists to
		// replace, and a reader who has never turned it on has no way to tell that
		// from a session nothing could be written about. Only the outline says so:
		// whoever asked for one turn asked to read it, not to configure anything.
		if n := summariesOffNotice(cfg, db); n != "" && len(opts.Summaries) == 0 {
			fmt.Fprintln(os.Stderr, n)
		}
		fmt.Fprintln(w, transcript.Outline(s, *conv, turns, opts))
		return
	}
	selected, err := selectTurns(turns, *turnSpec, *last, *all)
	if err != nil {
		fail(fmt.Errorf("show %s: %w", s.SessionID, err))
	}
	opts.SessionTurns = len(turns)
	doc, rendered := transcript.Markdown(s, *conv, selected, opts)
	fmt.Fprintln(w, doc)
	if rendered == 0 {
		// The turns exist — they are in the outline and the TUI lists them — but a
		// transcript shows nothing for a turn that is only a command, or only an
		// injected system message. Saying so beats handing back a bare header.
		fmt.Fprintf(os.Stderr, "agentcarto: %s holds nothing a transcript shows (a command with no reply, or an injected message)\n", selectedTurnsLabel(selected))
	}
}

// storedSummaries reads the generated summaries a session has, keyed the way a
// document wants them. Nothing is generated here: a reader of show is often an
// agent working through many sessions, and a command that quietly spent money
// per session would be a poor thing to hand one. `agentcarto summarize` is
// where that happens.
func storedSummaries(ctx context.Context, db *cache.DB, s domain.Session, turns []transcript.Turn) (map[int]string, string, bool) {
	if db == nil {
		return nil, "", false // --no-cache: summaries live in the cache
	}
	stored := db.Summaries(ctx, s, summary.NodesByTurn(turns))
	if len(stored) == 0 {
		return nil, "", false
	}
	out := make(map[int]string, len(stored))
	model, stale := "", false
	for n, sum := range stored {
		if strings.TrimSpace(sum.Text) == "" {
			// A blank row is the store's record that this session renders to no
			// document, not a summary. Keeping it out here is what lets the rest
			// of show treat "no summaries" as one thing.
			continue
		}
		out[n] = sum.Text
		if sum.Model != "" {
			// They are normally all from one run; naming any of them is enough
			// for a reader deciding how much to trust what it is reading.
			model = sum.Model
		}
		// Turn 0 is the one that can describe a session that has since gone on:
		// the per-turn summaries are held against the node they were made from and
		// withheld when it moves, so a stale one is never returned at all. What
		// counts as gone on is the store's call, so the TUI header and this
		// document cannot disagree about it.
		if sum.Stale(s) {
			stale = true
		}
	}
	return out, model, stale
}

// summariesOffNotice explains why this run could not have generated summaries at
// all, or returns "" when it could have.
//
// It answers from the configuration alone, so it says nothing about whether this
// particular session has anything to summarize — the paths that know that speak
// for themselves: summarizeForShow names a session gone to the background and
// one waiting out a failure, and a session that renders to no document is one
// the outline already shows in full.
func summariesOffNotice(cfg config.Config, db *cache.DB) string {
	switch {
	case cfg.Summary.Agent == "":
		return "agentcarto: no generated summaries — set summary.agent (claude or codex) in the config to have them written"
	case db == nil:
		return "agentcarto: --no-cache — generated summaries are read from the cache, so none are shown"
	}
	return ""
}

// selectedTurnsLabel names the turns that were asked for, for a message about
// them.
func selectedTurnsLabel(turns []transcript.Turn) string {
	if len(turns) == 1 {
		return fmt.Sprintf("turn %d", turns[0].Index+1)
	}
	ns := make([]int, len(turns))
	for i, t := range turns {
		ns[i] = t.Index + 1
	}
	return "turns " + ints(ns)
}

func toolOptions(mode string) (transcript.Options, error) {
	switch mode {
	case "label":
		return transcript.Options{Tools: transcript.ToolsLabel}, nil
	case "full":
		// The full form is the one a search leads to, so it carries what a search
		// can match: a subagent's report as well as a call's own lines.
		return transcript.Options{Tools: transcript.ToolsFull, TaskReports: true}, nil
	case "none":
		// Task notices and attachments are printed like tool calls, so they go too.
		return transcript.Options{Tools: transcript.ToolsNone}, nil
	}
	return transcript.Options{}, fmt.Errorf("--tools %q: use label, full or none", mode)
}

// selectTurns picks the turns to print. Numbers are the contiguous public
// numbers the outline lists. A named number or range outside that list is an
// error rather than a shorter document, because it usually means the caller
// selected a different session or read a stale outline.
func selectTurns(turns []transcript.Turn, spec string, last int, all bool) ([]transcript.Turn, error) {
	if all {
		return turns, nil
	}
	if last > 0 {
		if last >= len(turns) {
			return turns, nil
		}
		return turns[len(turns)-last:], nil
	}
	sp, err := parseTurnSpec(spec)
	if err != nil {
		return nil, err
	}
	var out []transcript.Turn
	found := map[int]bool{}
	hit := make([]bool, len(sp.ranges))
	for _, t := range turns {
		n := t.Index + 1
		keep := sp.explicit[n]
		for i, r := range sp.ranges {
			if n >= r[0] && n <= r[1] {
				keep, hit[i] = true, true
			}
		}
		if keep {
			out = append(out, t)
			found[n] = true
		}
	}
	var missing []int
	for n := range sp.explicit {
		if !found[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		sort.Ints(missing)
		return nil, fmt.Errorf("no such turn: %s (the session has %s)", ints(missing), turnRange(turns))
	}
	for i, r := range sp.ranges {
		if !hit[i] {
			return nil, fmt.Errorf("no turn between %d and %d (the session has %s)", r[0], r[1], turnRange(turns))
		}
	}
	return out, nil
}

// turnSelection is what --turns asked for: numbers named one by one, and ranges.
// The two are kept apart so errors can name an absent number differently from
// a range that does not overlap the session.
type turnSelection struct {
	explicit map[int]bool
	ranges   [][2]int
}

// parseTurnSpec reads "3,7,12-14" into the numbers and ranges it names.
func parseTurnSpec(spec string) (turnSelection, error) {
	sel := turnSelection{explicit: map[int]bool{}}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		from, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil || from < 1 {
			return turnSelection{}, fmt.Errorf("--turns %q: %q is not a turn number", spec, part)
		}
		if !isRange {
			sel.explicit[from] = true
			continue
		}
		to, err := strconv.Atoi(strings.TrimSpace(hi))
		if err != nil || to < from {
			return turnSelection{}, fmt.Errorf("--turns %q: %q is not a range", spec, part)
		}
		sel.ranges = append(sel.ranges, [2]int{from, to})
	}
	if len(sel.explicit) == 0 && len(sel.ranges) == 0 {
		return turnSelection{}, fmt.Errorf("--turns %q: no turn numbers in it", spec)
	}
	return sel, nil
}

func turnRange(turns []transcript.Turn) string {
	if len(turns) == 0 {
		return "no turns"
	}
	return fmt.Sprintf("turns %d to %d", turns[0].Index+1, turns[len(turns)-1].Index+1)
}

func ints(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}
