package transcript

import (
	"fmt"
	"github.com/agentcarto/core/domain"
	"strings"
)

// Markdown rendering of a session. What is kept and what is dropped follows the
// TUI's turn view: the prompt and the reply with their bodies, everything else
// as a one-line entry, and nothing at all for thinking, tool output and injected
// system messages — those show up in the turn view as labels like
// "result (120 lines)", which say nothing on their own.
//
// The lines are built from the events, not from the turn view's rendered
// blocks: a block label is sized for one terminal row, and a truncated command
// is worse than useless in a document that is read later.

// TimeFormat is the timestamp form used throughout a document. Unlike the TUI,
// which shows clock times against today, a document is read elsewhere and later,
// so every timestamp carries its date.
const TimeFormat = "2006-01-02 15:04:05"

// ToolMode says how much of a tool call a document carries.
type ToolMode int

const (
	// ToolsFull keeps the call whole: the one-line form on the entry, or a code
	// block under it when the call has line breaks (a heredoc, a patch). What a
	// reader gets is the command they could run again.
	ToolsFull ToolMode = iota
	// ToolsLabel keeps only the one-line form. A session that writes files with
	// heredocs is several times smaller this way, which matters when the reader
	// is an agent paying for every line in its context.
	ToolsLabel
	// ToolsNone drops tool entries entirely, leaving the conversation.
	ToolsNone
	// ToolsBrief keeps the one-line form but cuts it short. It is the one mode
	// that truncates, and it exists for a reader that only needs to know which
	// tool ran on what and will never run the command again — a model being
	// asked what happened in a turn. The plugin folds a heredoc's newlines into
	// spaces to fit a terminal row, so a single file-writing call can otherwise
	// carry tens of thousands of characters into a prompt that is paid for by
	// the token.
	ToolsBrief
)

// briefArgLimit is how much of a tool entry ToolsBrief keeps. Long enough for
// the tool name and what it acted on (a path, a command's first clause), short
// enough that a heredoc cannot dominate the document.
const briefArgLimit = 200

// Options are the choices a document makes that its reader cares about.
type Options struct {
	Tools ToolMode
	// SessionTurns is how many turns the whole session holds, set when the
	// document carries only a selection of them. The header then reads
	// "2 of 42", so an excerpt cannot be mistaken for the whole session.
	SessionTurns int
	// Branches is how many other lines of conversation the session holds
	// (rewinds and abandoned alternatives). A document renders one of them, and
	// saying how many were left out keeps the rest from being invisible.
	Branches int
	// TaskReports carries the prose a subagent came back with. It is what a
	// search matches on inside a task, so a document meant to answer a search
	// (the CLI's --tools full) sets it; an export meant to be read stays with the
	// label alone, which is the size a session was exported at before there was
	// a CLI to search from.
	TaskReports bool
	// Summaries are generated descriptions of what happened, keyed by turn number
	// with 0 for the session's own. A headline says what was asked for, which the
	// reader can already see; these say what came of it, which is the part that
	// decides whether a turn is worth opening. Turns without one are printed the
	// way they were before — this map is normally partial.
	//
	// It arrives as plain strings so that rendering stays independent of where
	// summaries are stored or how they are made.
	Summaries map[int]string
}

// Markdown renders the given turns of a session as one document and reports how
// many turns it holds. Turns that render to nothing (one holding only injected
// system messages) are left out, so the count is what the document contains —
// not the length of turns, and not the session's turn count.
func Markdown(s domain.Session, c domain.Conversation, turns []Turn, o Options) (doc string, rendered int) {
	var body []string
	for _, t := range turns {
		if lines := turnLines(c, t, s.CWD, o); len(lines) > 0 {
			body = append(body, lines...)
			rendered++
		}
	}
	return strings.Join(tightenLists(append(header(s, rendered, o), body...)), "\n"), rendered
}

// RenderedTurns reports which of the given turns a document would carry, by the
// number the turn view shows (Index+1). A turn holding nothing a reader can see
// — an injected system message and no reply — renders to no heading at all, and
// a caller that asked a model about it would wait forever for an answer that
// cannot come. Counting headings in the finished document instead would count
// the ones quoted inside a conversation, which happens whenever a session shows
// this program's own output.
func RenderedTurns(c domain.Conversation, turns []Turn, cwd string, o Options) []int {
	var out []int
	for _, t := range turns {
		if len(turnLines(c, t, cwd, o)) > 0 {
			out = append(out, t.Index+1)
		}
	}
	return out
}

// header renders the session's own metadata. Fields the plugin could not fill
// (a session with no recorded cwd, for instance) are left out rather than shown
// empty.
func header(s domain.Session, turns int, o Options) []string {
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = "(untitled)"
	}
	out := []string{"# " + title, ""}
	add := func(k, v string) {
		if v != "" {
			out = append(out, fmt.Sprintf("- **%s**: %s", k, v))
		}
	}
	add("Agent", s.AgentType)
	add("Session", s.SessionID)
	if s.CWD != "" {
		add("CWD", "`"+s.CWD+"`")
	}
	if !s.StartedAt.IsZero() {
		add("Started", s.StartedAt.Format(TimeFormat))
	}
	if !s.UpdatedAt.IsZero() {
		add("Updated", s.UpdatedAt.Format(TimeFormat))
	}
	count := fmt.Sprintf("%d", turns)
	if o.SessionTurns > turns {
		count = fmt.Sprintf("%d of %d", turns, o.SessionTurns)
	}
	add("Turns", count)
	if s.LogDeleted {
		// The file named above is not there any more. Saying so in the document
		// keeps it from being read as the state of a session that can be continued.
		add("Log", "deleted (this was read from the cache)")
	}
	if o.Branches > 0 {
		add("Other branches", fmt.Sprintf("%d (rewound or abandoned; not shown here)", o.Branches))
	}
	out = append(out, "")
	// The session's own summary reads as prose, so it goes below the metadata
	// list rather than as another field in it.
	if sum := strings.TrimSpace(o.Summaries[0]); sum != "" {
		out = append(out, sum, "")
	}
	return out
}

// turnLines renders one turn: a heading carrying the number the turn view shows,
// the compact notice when the turn sits on a /compact boundary, and the events
// that survive eventLines. A turn left with nothing to show produces no heading
// either.
func turnLines(c domain.Conversation, t Turn, cwd string, o Options) []string {
	events := t.Events(c)
	body := editedFiles(events, cwd)
	for _, e := range events {
		if InFileSection(e) {
			continue // already in the consolidated file section
		}
		lines := eventLines(e, o)
		if len(lines) == 0 {
			continue
		}
		if len(body) > 0 {
			body = append(body, "")
		}
		body = append(body, lines...)
	}
	if len(body) == 0 {
		return nil
	}
	head := fmt.Sprintf("## Turn %d", t.Index+1)
	if ts := TurnTime(events); !ts.IsZero() {
		head += " — " + ts.Format(TimeFormat)
	}
	out := []string{head, ""}
	if t.Compact {
		// The » badge of the turn list. Where the context was compacted matters to
		// someone reading the transcript: the agent stopped seeing what came before.
		out = append(out, "_(context compacted here)_", "")
	}
	// What came of the turn, before the turn itself: a reader who opened this
	// far still has to decide how much of it to read.
	if sum := strings.TrimSpace(o.Summaries[t.Index+1]); sum != "" {
		out = append(out, "> "+strings.ReplaceAll(sum, "\n", "\n> "), "")
	}
	return append(append(out, body...), "")
}

// editedFiles renders the consolidated file section the turn view puts at the
// top of a turn, with the paths left whole (the turn view shortens them to fit
// a row).
func editedFiles(events []domain.Event, cwd string) []string {
	edits := FileEdits(events)
	if len(edits) == 0 {
		return nil
	}
	out := []string{fmt.Sprintf("- Edited files (%d)", len(edits))}
	for _, fe := range edits {
		out = append(out, fmt.Sprintf("  - %s %s (+%d -%d)", fe.Status(), RelCWD(fe.Path, cwd), fe.Added, fe.Removed))
	}
	return out
}

// eventLines renders one event, or nil for the events a document leaves out.
func eventLines(e domain.Event, o Options) []string {
	switch e.Kind {
	case domain.EventUser:
		if e.Prompt == "" {
			return nil // injected system message, not something the user typed
		}
		return bodySection("USER", e.Text)
	case domain.EventQueued:
		return bodySection("USER (queued)", e.Text)
	case domain.EventAssistant:
		return bodySection("ASSISTANT", e.Text)
	case domain.EventToolCall:
		name := e.ToolName
		if name == "" {
			name = "tool"
		}
		return toolEntry(name, e.ToolArg, e.ToolDetail, o)
	case domain.EventTask:
		// A task's ToolDetail is the finished subagent's report. It is not tool
		// output in the sense the rest of this file means it — "result (120
		// lines)" says nothing, a report is prose the agent went and got — and it
		// is what a search matches on, so a document asked for with TaskReports
		// carries it. Everything else keeps the label alone.
		entry := toolEntry("TASK", e.ToolArg, "", o)
		if !o.TaskReports || len(entry) == 0 {
			return entry
		}
		if report := bodySection("TASK report", e.ToolDetail); len(report) > 0 {
			return append(append(entry, ""), report...)
		}
		return entry
	case domain.EventAttachment:
		// Likewise the attachment's Text is the injected file content.
		return toolEntry("attachment", e.ToolArg, "", o)
	}
	return nil
}

// bodySection renders a labeled block of text, or nil when there is no text: an
// assistant event can carry none (encrypted thinking, for instance), and an
// empty **ASSISTANT** heading would stand for nothing.
func bodySection(label, text string) []string {
	body := strings.TrimRight(text, "\n")
	if strings.TrimSpace(body) == "" {
		return nil
	}
	return append([]string{"**" + label + "**", ""}, strings.Split(body, "\n")...)
}

func listEntry(text string) []string {
	if text = strings.TrimSpace(text); text != "" {
		return []string{"- " + text}
	}
	return nil
}

// clipRunes cuts text to at most n runes, marking that it was cut. Counting
// runes rather than bytes keeps a multibyte character from being split in half
// — the text is a command line, and half a character in the middle of a path is
// worse than the ellipsis.
func clipRunes(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return strings.TrimRight(string(r[:n]), " ") + "…"
}

// toolEntry renders a tool call as a list entry. ToolArg is the plugin's
// one-line form of the call and stays on the entry's own line. ToolDetail is
// the same call with its line breaks intact (a heredoc, a patch, a payload):
// under ToolsFull, a detail with more than one line goes into a code block
// under the entry instead, because the one-line form of a script is unreadable
// — the plugin folded every newline into a space to fit a terminal row.
//
// Nothing is truncated except under ToolsBrief. A command cut mid-path says
// nothing to a reader who might run it again; ToolsBrief has a reader who will
// not.
func toolEntry(name, arg, detail string, o Options) []string {
	if o.Tools == ToolsNone {
		return nil
	}
	detail = strings.TrimRight(detail, "\n")
	if o.Tools == ToolsBrief {
		return listEntry(clipRunes(strings.TrimSpace(name+" "+strings.TrimSpace(arg)), briefArgLimit))
	}
	if o.Tools == ToolsLabel || !strings.Contains(detail, "\n") {
		return listEntry(strings.TrimSpace(name + " " + strings.TrimSpace(arg)))
	}
	lines := strings.Split(detail, "\n")
	fence := "  " + fenceFor(lines)
	// Two spaces line the block up under the entry's text ("- " is two columns
	// wide), which is what keeps it part of the list item.
	out := []string{"- " + name, "", fence}
	for _, ln := range lines {
		out = append(out, strings.TrimRight("  "+ln, " "))
	}
	return append(out, fence)
}

// fenceFor returns a code fence longer than any backtick run inside lines, so
// that an argument holding fences of its own (a Markdown file being written,
// say) cannot break out of the block.
func fenceFor(lines []string) string {
	longest := 2
	for _, ln := range lines {
		run := 0
		for _, r := range ln {
			if r == '`' {
				run++
				if run > longest {
					longest = run
				}
				continue
			}
			run = 0
		}
	}
	return strings.Repeat("`", longest+1)
}

// tightenLists drops the blank line between consecutive list entries, which the
// per-event spacing would otherwise leave behind. Without this a run of tool
// calls renders as a loose list, one paragraph per entry.
func tightenLists(lines []string) []string {
	isEntry := func(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), "- ") }
	out := make([]string, 0, len(lines))
	inFence := false
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			// A tool argument's code block can hold blank lines between lines that
			// start with "- "; those belong to the command, not to the document.
			inFence = !inFence
		}
		if !inFence && ln == "" && i > 0 && i+1 < len(lines) && isEntry(lines[i-1]) && isEntry(lines[i+1]) {
			continue
		}
		out = append(out, ln)
	}
	return out
}
