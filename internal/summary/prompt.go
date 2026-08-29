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

For each turn, write what was done and what it showed.

Rules:
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
	doc, _ = transcript.Markdown(s, c, want, o2)
	return doc, transcript.RenderedTurns(c, want, s.CWD, o2)
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
