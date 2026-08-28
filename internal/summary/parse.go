package summary

import (
	"fmt"
	"strconv"
	"strings"
)

// Result is what a model came back with: one summary per turn, the session's
// own, and the one-line headline that stands in for it where there is room for
// a line and not a paragraph.
type Result struct {
	Session string
	// Headline is the session in one line. Empty when the model did not write
	// one, which is not an error: what shows it falls back to the summary.
	Headline string
	Turns    map[int]string
}

// Marker prefixes. The reply is not JSON, and that is deliberate: a model
// writing Japanese prose quotes identifiers as it goes ("-", `foo`) and forgets
// to escape the quotes, which makes encoding/json reject the whole document —
// every turn lost for one character, after the call was paid for. Observed on
// the first real run of this prompt against claude-sonnet-5.
//
// Line-anchored markers have no escaping to get wrong, and a malformed one
// costs a single turn instead of the answer.
const (
	sessionMarker  = "@@SESSION"
	headlineMarker = "@@HEADLINE"
	turnMarker     = "@@TURN "
)

// Sections that are not turn numbers. Turn numbers are positive, so the
// negatives are free for the sections that are one of a kind.
const (
	sectionPreamble = -1 // before any marker, or after one that could not be read
	sectionHeadline = -2
)

// Parse reads a model's answer. asked is the turn numbers the prompt carried;
// anything outside it is dropped, since a summary attached to a turn that was
// never described is a number the model made up.
//
// Failures are errors, not empty results: a summary that silently came back
// blank would be stored as "this turn has nothing to say" and never retried.
func Parse(out string, asked []int) (Result, error) {
	if strings.TrimSpace(out) == "" {
		return Result{}, fmt.Errorf("the model returned nothing")
	}
	want := map[int]bool{}
	for _, n := range asked {
		want[n] = true
	}
	res := Result{Turns: map[int]string{}}

	// section is the marker the following lines belong to: 0 for the session
	// summary, a turn number, or one of the negatives above (a headline, or the
	// preamble a model sometimes writes despite being told not to).
	section := sectionPreamble
	var body []string
	flush := func() {
		text := strings.TrimSpace(strings.Join(body, "\n"))
		body = nil
		if text == "" {
			return
		}
		switch {
		case section == 0:
			res.Session = text
		case section == sectionHeadline:
			// One line, whatever the model wrote across: a headline stands where
			// there is room for a line, and a wrapped one would take the room of the
			// summary it stands in for.
			res.Headline = strings.Join(strings.Fields(text), " ")
		case section > 0 && want[section]:
			res.Turns[section] = text
		}
	}
	for _, line := range strings.Split(unfence(strings.TrimSpace(out)), "\n") {
		switch {
		case strings.HasPrefix(line, headlineMarker):
			flush()
			section = sectionHeadline
		case strings.HasPrefix(line, sessionMarker):
			flush()
			section = 0
		case strings.HasPrefix(line, turnMarker):
			n, e := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, turnMarker)))
			if e != nil || n <= 0 {
				// A marker whose number is unreadable takes its own body down
				// with it, and nothing else: the next marker starts a fresh
				// section. This is the whole point of not using JSON.
				//
				// Numbers at or below zero are not turn numbers: zero is the
				// session summary, which has a marker of its own, and the
				// negatives are the section values above. A model that writes
				// "@@TURN -2" must not land in the headline.
				flush()
				section = sectionPreamble
				continue
			}
			flush()
			section = n
		default:
			body = append(body, line)
		}
	}
	flush()

	if len(res.Turns) == 0 && res.Session == "" {
		return Result{}, fmt.Errorf("the model's answer held no @@SESSION or @@TURN section for any of the %d turns asked about (answer began %.80q)", len(asked), strings.TrimSpace(out))
	}
	return res, nil
}

// Missing reports the turns that were asked about and did not come back. A model
// that skips turns is not an error — the caller stores what it got and asks
// again for the rest — but it must not be mistaken for those turns having
// nothing to say.
func (r Result) Missing(asked []int) []int {
	var out []int
	for _, n := range asked {
		if _, ok := r.Turns[n]; !ok {
			out = append(out, n)
		}
	}
	return out
}

// unfence strips a Markdown code fence around an answer. The instruction says
// not to add one, and the stronger models obey, but a smaller one wraps its
// output in ``` anyway — observed with Haiku 4.5 on this prompt. Refusing that
// answer would mean paying for a call and throwing away a usable result.
func unfence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	} else {
		return s
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
