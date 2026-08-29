// Package summary turns a session into a prompt asking a model what happened in
// each of its turns, and turns the answer back into per-turn text.
//
// It holds no I/O of its own: building the prompt and reading the reply are pure
// functions, and running the agent that answers it is behind Generator. The
// split is what makes the part that decides what a summary says testable without
// an API key, a network, or a subprocess.
package summary

import (
	"fmt"
	"sort"
	"strings"

	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

// System is the instruction the model runs under. It is deliberately about the
// turn's outcome rather than its request: the CLI already prints what the user
// asked for on the line above (the turn headline is the prompt), so a summary
// that paraphrases the request adds nothing to it.
//
// The instruction is in English while the summaries it asks for are not: the
// language they come out in is what language decides. Sessions are had in every
// language, and the reader of a summary is the person who had the session.
func System(language string) string {
	return `You summarize the logs of AI coding agents. The input is one session as Markdown. Each turn sits under a "## Turn N" heading and holds the USER message, the ASSISTANT reply, and the tool calls made. Long tool calls are cut short (a trailing … marks it).

A quote under a ` + transcript.ContextLabel + ` line above a heading is the end of the reply that closed the turn before it, carried over because that turn is not in this input. Read it, but do not summarize it: it gets no @@TURN section of its own.

For each turn, write what was done and what it showed.

Rules:
- A prompt that only answers — "yes", "go ahead", "the first one" — answers what the reply above it asked. Write what was settled and what came of it, not that it was agreed to.
- Do not restate the request. The reader has it already — the line above a summary is the prompt itself. Write what was done, what it showed, and what changed.
- If a cause was found, name it. If something was fixed, say what changed and where. If a question was settled, give the answer.
- Keep file names, function names, identifiers and commit hashes exactly as they appear.
- Length follows the turn, up to about 300 characters in a language written without spaces (Japanese, Chinese) or about 60 words in English — four sentences at the very most, and fewer when the turn did little. When a turn holds more than that, write what changed and drop the blow-by-blow of getting there.
- Write plainly. No polite register, no filler, nothing the reader can see for themselves. Do not name the actors ("the user", "the assistant"); say what happened.
- Write nothing the input does not support. Do not guess how a cut-off tool call continued.
- Also write one summary of the session as a whole. Its length follows the session's own size — the limit above is per turn, not for this.
- Also write one headline for the session: a single line that tells it apart in a list, about 40 characters (8 words in English). Name the one thing the session did rather than listing what it touched.

` + languageRule(language) + `

Answer in exactly this format, with nothing before or after it and no code fence. A line beginning with @@ is a separator; everything from the next line until the following separator is that section's body. Bodies are free text — quotes and punctuation need no escaping.

@@HEADLINE
the session's headline, one line

@@SESSION
the session as a whole

@@TURN 1
what turn 1 did

@@TURN 2
what turn 2 did

Write a @@TURN section for every turn number in the input.

Use no tools. Answer from the input alone.`
}

// languageRule tells the model what language to write in. An empty setting
// follows the session, which is what suits a reader who had it: the summary of a
// Japanese session is read by whoever wrote Japanese into it.
func languageRule(language string) string {
	if language = strings.TrimSpace(language); language != "" {
		return "Write every summary and headline in " + language + ", whatever language the session itself is in."
	}
	return "Write every summary and headline in the language the session itself is written in. Judge it from what was typed into the session, not from this instruction."
}

// NodesByTurn maps each turn's number to the id of its terminal node — what the
// summary store checks a stored summary against before showing it. Turn numbers
// are positions along one branch and a rewind renumbers them; the node id is
// what stays attached to the content.
func NodesByTurn(turns []transcript.Turn) map[int]string {
	out := make(map[int]string, len(turns))
	for _, t := range turns {
		if len(t.Nodes) > 0 {
			out[t.Index+1] = t.Nodes[len(t.Nodes)-1]
		}
	}
	return out
}

// Options are the knobs Prompt has. The zero value is what the summarizer uses.
type Options struct {
	// Turns, when set, limits the prompt to these turn numbers (the numbers
	// transcript.Turns shows, 1-based). It is how a session that grew is
	// summarized without paying for the turns already done. Empty means every
	// turn on the path.
	Turns map[int]bool
}

// Prompt renders the session as the document the model is asked about, and
// reports which turn numbers it carries. A caller needs that list because turns
// holding nothing a reader can see (an injected system message and no reply)
// render to nothing and are silently absent — asking for a summary of turn 4
// and getting none is otherwise indistinguishable from the model skipping it.
func Prompt(s domain.Session, c domain.Conversation, turns []transcript.Turn, o Options) (doc string, asked []int) {
	var want []transcript.Turn
	for _, t := range turns {
		if o.Turns != nil && !o.Turns[t.Index+1] {
			continue
		}
		want = append(want, t)
	}
	if len(want) == 0 {
		return "", nil
	}
	// ToolsBrief is the mode that exists for this reader: the whole conversation,
	// tool calls cut to what says which tool ran on what. TaskReports stays off —
	// a subagent's report is prose the size of a turn, and the summary of a turn
	// that delegated is "delegated X", not the delegate's own writeup.
	o2 := transcript.Options{Tools: transcript.ToolsBrief, SessionTurns: len(turns)}
	// What the document describes is settled before the context is chosen, and it
	// has to be: a turn is preceded by the turn above it in the document, which is
	// not the turn before it in the session if that one renders to nothing.
	// RenderedTurns does not read Context, so asking it first is not circular.
	asked = transcript.RenderedTurns(c, want, s.CWD, o2)
	o2.Context = carriedContext(c, turns, want, asked)
	doc, _ = transcript.Markdown(s, c, want, o2)
	return doc, asked
}

// contextRunes is how much of a preceding reply is carried over. A reply ends
// in what it asks — a proposal, a question, a list to pick from — and that is
// what the turn after it answers, so this is enough for a closing paragraph and
// the choices it offered.
const contextRunes = 800

// maxContextRunesPerDoc bounds what one document spends carrying replies over.
// The turns needing a summary are normally one run, which needs a single block:
// the head of the document, where the session continues from something that is
// not in it. A set left scattered — a model skipped turns and the rest were
// asked about again — would instead put a block above every heading, and a call
// sized against maxCharsPerCall would carry half as much again unasked. Past
// this budget the later turns go without, which is where every turn was before
// any of this existed. Batch reserves the same amount, in bytes.
const maxContextRunesPerDoc = 4 * contextRunes

// carriedContext is what each turn in the document needs from a turn that is
// not in it, keyed by the turn number transcript.Options.Context takes.
//
// A session that grew is summarized turn by turn, so the document is often a
// single turn whose prompt is "はい" or "実装" — a reply to something said in a
// turn that was summarized on an earlier run. Without what that turn closed
// with, nothing in the input says what was agreed to, and the summary comes out
// as "did what was asked". The same gap opens at the head of every batch after
// the first when a long session is split across calls.
//
// Only turns whose predecessor is absent get one: when the previous turn is in
// the document, it is already there to be read, whole. rendered is what the
// document actually describes, which is not the same as what was selected — a
// turn holding only an injected system message is chosen and then renders to
// nothing, and the turn after it is left following a heading that is not there.
func carriedContext(c domain.Conversation, turns, want []transcript.Turn, rendered []int) map[int]string {
	inDoc := make(map[int]bool, len(rendered))
	for _, n := range rendered {
		inDoc[n] = true
	}
	byIndex := make(map[int]transcript.Turn, len(turns))
	for _, t := range turns {
		byIndex[t.Index] = t
	}
	out := map[int]string{}
	spent := 0
	// want is in turn order, so the budget is spent from the front: the earliest
	// boundary is the one the document opens on, and the one a reader arrives at
	// with nothing behind them.
	for _, t := range want {
		prev, ok := byIndex[t.Index-1]
		// t.Index is the number of the turn before t, since numbers are Index+1.
		if inDoc[t.Index] || !ok {
			continue
		}
		reply := closingReply(c, prev)
		if reply == "" {
			continue
		}
		if spent+len([]rune(reply)) > maxContextRunesPerDoc {
			break
		}
		spent += len([]rune(reply))
		out[t.Index+1] = reply
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// closingReply is the last thing the agent said in a turn, cut to its tail.
//
// Empty assistant events are passed over rather than taken as the end of the
// turn: an agent's last event can carry encrypted thinking and no text at all.
// A turn with nothing said in it yields nothing — the search stops at the turn
// boundary rather than reaching further back, since a document with no context
// is what every document had before this existed.
func closingReply(c domain.Conversation, t transcript.Turn) string {
	events := t.Events(c)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != domain.EventAssistant {
			continue
		}
		if text := strings.TrimSpace(events[i].Text); text != "" {
			return tailRunes(text, contextRunes)
		}
	}
	return ""
}

// tailRunes keeps the last n runes of text, marking that it was cut. Everything
// else that shortens text here keeps the head; this keeps the end, because what
// a reply is carried over for — the question it closes with — is at the end of
// it. Runes rather than bytes so that a cut never splits a character in half.
func tailRunes(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return "…" + strings.TrimLeft(string(r[len(r)-n:]), " \t\n")
}

// SessionSystem is the instruction for writing a session's own summary from the
// summaries of its turns.
//
// A long session never has all its turns in front of the model at once — it is
// asked about in batches — so the session summary cannot come from any one of
// them. The last batch's answer describes the last few turns, not the session.
// Reading the turn summaries instead costs a fraction of rereading the session
// and sees all of it.
func SessionSystem(language string) string {
	return `You summarize the logs of AI coding agents. The input is the summaries already written for each turn of one session.

Read them and write what the session as a whole did.

Rules:
- Do not list the turns. Say what it set out to do, what it found, what it produced, and where it ended.
- Keep file names, function names, identifiers and commit hashes exactly as they appear.
- Length follows the session's size. There is no cap, but write nothing redundant and nothing the reader can see for themselves.
- Write plainly. No polite register, no filler. Do not name the actors ("the user", "the assistant"); say what happened.
- Write nothing the input does not support.
- Besides the summary, write one headline for the session: a single line that tells it apart in a list, about 40 characters (8 words in English). Name the one thing the session did rather than listing what it touched.

` + languageRule(language) + `

Answer in exactly this format, with nothing before or after it and no code fence.

@@HEADLINE
the session's headline, one line

@@SESSION
the session as a whole

Use no tools. Answer from the input alone.`
}

// SessionPrompt renders the turn summaries as the document SessionSystem is
// asked about.
func SessionPrompt(s domain.Session, turns map[int]string) string {
	var b strings.Builder
	if title := strings.TrimSpace(s.Title); title != "" {
		b.WriteString("# " + title + "\n\n")
	}
	for _, n := range sortedTurns(turns) {
		fmt.Fprintf(&b, "## Turn %d\n\n%s\n\n", n, strings.TrimSpace(turns[n]))
	}
	return b.String()
}

func sortedTurns(m map[int]string) []int {
	out := make([]int, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}
