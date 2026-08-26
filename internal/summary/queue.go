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
	// Attempts and LastTried record failures. Nothing else can: the guard
	// against regenerating asks the store when a session was last summarized,
	// and a session that has never succeeded has nothing there — so without
	// this it would be retried on every scan, which for the kind of failure
	// where the model answers but the answer will not parse means paying every
	// time.
	Attempts  int       `json:"attempts,omitempty"`
	LastTried time.Time `json:"last_tried,omitempty"`
}

// RetryAfter is how long a failed request waits before the worker tries it
// again, and MaxAttempts is where it gives up. A session that cannot be
// summarized is usually one that will not be — an answer the model keeps
// mangling, a conversation something in the pipeline cannot render — so the
// backoff is generous and the ceiling is low.
const (
	RetryAfter  = 6 * time.Hour
	MaxAttempts = 3
)

// Ready reports whether a request should be tried now. A request that failed
// recently waits; one that has failed too often is done being tried, and Sweep
// removes it.
func (r Request) Ready(now time.Time) bool {
	return r.Attempts < MaxAttempts && (r.LastTried.IsZero() || now.Sub(r.LastTried) >= RetryAfter)
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
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	final := filepath.Join(q.dir, requestName(r.PluginID, r.SessionID))
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

// Retry records that a request failed and puts it back, so the worker leaves it
// alone for a while instead of doing it again on the next scan. Once it has
// failed MaxAttempts times it stops being tried at all, and Sweep takes it.
func (q *Queue) Retry(r Request, now time.Time) error {
	r.Attempts++
	r.LastTried = now
	return q.Add(r)
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

// Sweep removes requests older than maxAge and files that are not requests, and
// reports how many it took. A request that has sat unread for days describes a
// session that has almost certainly changed since; summarizing it would pay for
// a prompt built from a conversation that no longer looks like that.
func (q *Queue) Sweep(maxAge time.Duration, now time.Time) int {
	reqs, bad := q.List()
	n := 0
	for _, p := range bad {
		if os.Remove(p) == nil {
			n++
		}
	}
	for _, r := range reqs {
		// Stale, or tried until there was no point trying again.
		if now.Sub(r.Queued) > maxAge || r.Attempts >= MaxAttempts {
			if q.Done(r) == nil {
				n++
			}
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
