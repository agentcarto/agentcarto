package summary

import (
	"sort"

	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

// maxCharsPerCall bounds one call's input, in bytes. The largest session on the
// machine this was built on renders to 1.4 MB of summarizing input — 920,000
// characters, roughly 550,000 tokens — which fits a one-million-token context
// but costs $2.60 in a single call, all of it lost if the answer is unusable.
// This limit puts a call at about 80,000 tokens, well under the smallest
// context an agent here might run with (Haiku 4.5's 200K), so the choice of
// model never turns a session into one that cannot be summarized at all.
const maxCharsPerCall = 200_000

// maxTurnsPerCall bounds one call's output, which is the binding constraint on
// a long session: a summary runs 300 to 500 characters, so 232 turns would ask
// for roughly 63,000 output tokens against a 64,000 limit. An answer cut at the
// limit loses every turn after the cut, and the call is paid for either way.
const maxTurnsPerCall = 60

// Batch splits the turns to ask about into runs that each fit one call, in turn
// order. Splitting matters beyond staying under the limits: a session that
// fails halfway keeps the batches that finished, so the next run costs only
// what is left rather than starting over.
//
// A turn larger than the limit on its own still gets a batch — a call that may
// fail beats a turn that can never be summarized.
func Batch(c domain.Conversation, turns []transcript.Turn, want map[int]bool, cwd string) [][]int {
	sizes := transcript.TurnSizes(c, turns, cwd, transcript.Options{Tools: transcript.ToolsBrief})
	var asked []int
	for _, t := range turns {
		if n := t.Index + 1; want[n] && sizes[n] > 0 {
			asked = append(asked, n)
		}
	}
	sort.Ints(asked)

	var out [][]int
	var cur []int
	curChars := 0
	for _, n := range asked {
		// Start a new batch when this turn would push the current one past
		// either limit — unless the current batch is empty, since a batch has to
		// hold at least the turn that overflows it.
		if len(cur) > 0 && (len(cur) >= maxTurnsPerCall || curChars+sizes[n] > maxCharsPerCall) {
			out = append(out, cur)
			cur, curChars = nil, 0
		}
		cur = append(cur, n)
		curChars += sizes[n]
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// TurnSet turns a batch back into the form Prompt takes.
func TurnSet(batch []int) map[int]bool {
	out := make(map[int]bool, len(batch))
	for _, n := range batch {
		out[n] = true
	}
	return out
}
