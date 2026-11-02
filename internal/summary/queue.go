package summary

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// Queue is the handoff between the program a person runs and the worker that
// summarizes. The host writes a request and leaves; the worker picks it up
// whenever it gets to it.
//
// It is a directory of files rather than a pipe or a temporary path because the
// two processes do not overlap in time. A pipe closes when the writer exits —
// which is the normal case here, since the whole point is that the TUI can be
// closed while summarizing continues. Files under /tmp go away on reboot, which
// would silently drop work that was queued and never run. A directory in the
// cache survives both, and doubles as the worker's to-do list: whatever is
// there is what is left to do.
type Queue struct{ dir string }

// Request is one session waiting to be summarized. The prompt travels with it
// rather than being rebuilt by the worker: the host has already parsed the
// conversation, and a second parse in another process would be both slower and
// a chance for the two to disagree about what the turns are.
type Request struct {
	PluginID  string    `json:"plugin_id"`
	SessionID string    `json:"session_id"`
	Queued    time.Time `json:"queued"`
	// Fingerprint is the version of the log the prompts were built from. The
	// worker stores it beside the summaries it makes: a summary is of one version
	// of a session, and the readers that ask whether it predates the newest turns
	// compare against exactly this. Without it every summary the worker wrote
	// read as older than the session it came from.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Held marks a request that left a turn out because it had not settled — the
	// newest turn, which may not have ended (see settleAfterComplete).
	//
	// It is what stops the run answering this request from recording the version
	// as done. Turn 0 carries that record, and a session recorded as done is not
	// opened again until its log moves; a log that just ended has no reason to
	// move, so the held-back turn would never be summarized.
	Held    bool           `json:"held,omitempty"`
	Nodes   map[int]string `json:"nodes"`
	Batches [][]int        `json:"batches"`
	Prompts []string       `json:"prompts"`
	// Answered is how many of Prompts have come back and been stored, and AnsweredAt
	// when the last of them was. Those calls are paid for, so a request that failed
	// or was stopped further on is taken up at the next one.
	Answered   int       `json:"answered,omitempty"`
	AnsweredAt time.Time `json:"answered_at,omitzero"`
	// Attempts and LastTried record failures. Nothing else can: the guard
	// against regenerating asks the store when a session was last summarized,
	// and a session that has never succeeded has nothing there — so without
	// this it would be retried on every scan, which for the kind of failure
	// where the model answers but the answer will not parse means paying every
	// time.
	//
	// LastTried is every failure and Attempts only those that may have cost
	// something, since spending is what the ceiling bounds (see ErrNotBilled).
	Attempts  int       `json:"attempts,omitempty"`
	LastTried time.Time `json:"last_tried,omitempty"`
	// LastError is why the latest attempt failed, on one line. It is kept on the
	// request because the log is written for reading afterwards and holds every
	// session's history: asking why this session has no summary should not mean
	// searching it.
	LastError string `json:"last_error,omitempty"`
}

// maxLastError bounds the reason a request keeps, in runes. An answer that will
// not parse quotes the model and a CLI's diagnostics can run long, and a reason is
// read on a single line.
const maxLastError = 300

// RetryAfter is how long a failed request waits before the worker tries it again.
// MaxAttempts is how many failures that may have cost something it takes to give up,
// and GiveUpFor is how long giving up lasts.
//
// Giving up is not for good. A session that keeps costing money without producing a
// summary should stop doing that, but the cause can be outside the session, and a
// summary never made is lost once the agent rotates the log away. The wait is long
// enough that the spending stops mattering and well short of that rotation.
const (
	RetryAfter  = 6 * time.Hour
	MaxAttempts = 3
	GiveUpFor   = 7 * 24 * time.Hour
)

// Ready reports whether a request should be tried now. A request that failed
// recently waits, and one that has given up is never tried again: it holds no prompts
// to try, and what follows it is a new request (see Superseded).
func (r Request) Ready(now time.Time) bool {
	return !r.GaveUp() && (r.LastTried.IsZero() || now.Sub(r.LastTried) >= RetryAfter)
}

// Accounted is the time up to which this request knows what has been stored for its
// session. Anything newer is something else answering it.
func (r Request) Accounted() time.Time {
	if r.AnsweredAt.After(r.Queued) {
		return r.AnsweredAt
	}
	return r.Queued
}

// GaveUp reports whether a request is a record of having given up.
func (r Request) GaveUp() bool { return r.Attempts >= MaxAttempts }

// Superseded reports whether a record of having given up no longer holds its session
// back, now that the log stands at fingerprint: its wait is over, or the session has
// moved on to a conversation nobody has tried. A request still being tried is never
// superseded. It has its prompts, and queueing the session again would throw away
// what is known about its failures.
func (r Request) Superseded(fingerprint string, now time.Time) bool {
	return r.GaveUp() && (r.Fingerprint != fingerprint || now.Sub(r.LastTried) >= GiveUpFor)
}

// OpenQueue returns the queue rooted at dir, creating it if it is not there.
// The directory is private: a request carries a session's text.
func OpenQueue(dir string) (*Queue, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &Queue{dir: dir}, nil
}

// Add writes a request, replacing any earlier one for the same session. The
// write goes to a temporary file first and is renamed into place, so a worker
// reading the directory never sees a half-written request.
func (q *Queue) Add(r Request) error {
	if r.PluginID == "" || r.SessionID == "" {
		return errors.New("summary: a queued request needs a plugin and a session")
	}
	return q.writeJSON(requestName(r.PluginID, r.SessionID), r)
}

// writeJSON puts a private file in the queue's directory in one step, replacing
// what was under that name.
func (q *Queue) writeJSON(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	final := filepath.Join(q.dir, name)
	tmp, err := os.CreateTemp(q.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename succeeds
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), final)
}

// List returns the queued requests, oldest first, skipping any that cannot be
// read as one. A file that is not a request is not an error the worker can act
// on — it removes it and moves on, rather than stopping on it forever.
func (q *Queue) List() ([]Request, []string) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return nil, nil
	}
	var out []Request
	var bad []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(q.dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r Request
		if json.Unmarshal(b, &r) != nil || r.SessionID == "" {
			bad = append(bad, p)
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Queued.Before(out[j].Queued) })
	return out, bad
}

// Advance records how far a request has got. A request no longer in the queue stays
// out: `show` takes one out before generating that session itself, and a worker that
// started on it earlier must not bring it back by reporting its progress.
func (q *Queue) Advance(r Request) error {
	if _, err := os.Stat(filepath.Join(q.dir, requestName(r.PluginID, r.SessionID))); err != nil {
		return nil
	}
	return q.Add(r)
}

// Retry records that a request failed, and why, and puts it back, so the worker
// leaves it alone for a while instead of doing it again on the next scan. Once it
// has failed MaxAttempts times in ways that may have cost something it gives up, and
// what goes back is only the record: the prompts hold a session's text, and nothing
// will read them again.
func (q *Queue) Retry(r Request, now time.Time, reason error) error {
	if !errors.Is(reason, ErrNotBilled) {
		r.Attempts++
	}
	r.LastTried = now
	r.LastError = reasonLine(reason)
	if r.GaveUp() {
		r.Nodes, r.Batches, r.Prompts = nil, nil, nil
	}
	return q.Add(r)
}

// reasonLine renders a failure as the single bounded line LastError holds, with
// nothing in it that would act on the terminal it is drawn on. Sequences go whole
// before single control characters: dropping only the ESC would leave "[31m"
// standing in the text.
func reasonLine(reason error) string {
	if reason == nil {
		return ""
	}
	printable := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(reason.Error()))
	line := strings.Join(strings.Fields(printable), " ")
	if runes := []rune(line); len(runes) > maxLastError {
		return string(runes[:maxLastError]) + "…"
	}
	return line
}

// Done removes a request. It is called for a request that was summarized and
// for one that failed in a way retrying cannot fix — leaving either in place
// would make the worker do it again on every run, spending each time.
func (q *Queue) Done(r Request) error {
	err := os.Remove(filepath.Join(q.dir, requestName(r.PluginID, r.SessionID)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Sweep removes requests older than maxAge, records of giving up whose wait is over,
// and files that are neither, and reports how many it took. A request that has sat
// unread for days describes a session that has almost certainly changed since;
// summarizing it would pay for a prompt built from a conversation that no longer
// looks like that.
//
// A record of giving up is not aged by when it was queued. It holds no prompt to go
// stale, and taking it at maxAge would end its wait early.
func (q *Queue) Sweep(maxAge time.Duration, now time.Time) int {
	reqs, bad := q.List()
	n := 0
	for _, p := range bad {
		if os.Remove(p) == nil {
			n++
		}
	}
	for _, r := range reqs {
		over := now.Sub(r.Queued) > maxAge
		if r.GaveUp() {
			over = now.Sub(r.LastTried) >= GiveUpFor
		}
		if over && q.Done(r) == nil {
			n++
		}
	}
	return n
}

// requestName keys a request by its session, so queueing the same session twice
// replaces rather than duplicates. The id is used as-is except for separators:
// every agent here names sessions with hex and dashes, and anything else would
// be a plugin inventing a format the rest of the program does not expect.
func requestName(plugin, session string) string {
	safe := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
				return r
			default:
				return '_'
			}
		}, s)
	}
	return fmt.Sprintf("%s-%s.json", safe(plugin), safe(session))
}

// Find returns the queued request for a session, if there is one. A caller
// queueing the same session again uses it to carry over what is known about
// earlier failures.
func (q *Queue) Find(plugin, session string) (Request, bool) {
	b, err := os.ReadFile(filepath.Join(q.dir, requestName(plugin, session)))
	if err != nil {
		return Request{}, false
	}
	var r Request
	if json.Unmarshal(b, &r) != nil || r.SessionID == "" {
		return Request{}, false
	}
	return r, true
}
