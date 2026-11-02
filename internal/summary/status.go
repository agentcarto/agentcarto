package summary

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agentcarto/core/domain"
)

// workingName is the file in the queue's directory that says what the worker has
// in hand. It does not end in .json, which is what keeps it out of List: a file
// there that is not a request gets deleted by Sweep.
const workingName = ".working"

// Working is the session a worker is generating for right now. A request says
// nothing about this, since it leaves the queue only when it is finished, and one
// call on a long session can run for minutes.
type Working struct {
	PluginID  string `json:"plugin_id"`
	SessionID string `json:"session_id"`
	// Call and Calls count the turn prompts of the request. Call is 0 for the call
	// that makes the session's own summary, which comes after them and is not
	// always made.
	Call  int `json:"call,omitempty"`
	Calls int `json:"calls,omitempty"`
	// Started is when the current call began.
	Started time.Time `json:"started"`
}

// MarkWorking records what the worker is about to generate. Nothing but a reader
// of the status depends on it.
func (q *Queue) MarkWorking(r Request, call, calls int, now time.Time) error {
	return q.writeJSON(workingName, Working{PluginID: r.PluginID, SessionID: r.SessionID, Call: call, Calls: calls, Started: now})
}

// ClearWorking removes the record of what a worker has in hand.
func (q *Queue) ClearWorking() error {
	err := os.Remove(filepath.Join(q.dir, workingName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// readWorking returns the record of what a worker has in hand, if there is one.
// An unreadable record reads as none: it only ever decides what a status line
// says, and the next call replaces it.
func (q *Queue) readWorking() (Working, bool) {
	b, err := os.ReadFile(filepath.Join(q.dir, workingName))
	if err != nil {
		return Working{}, false
	}
	var w Working
	if json.Unmarshal(b, &w) != nil || w.SessionID == "" {
		return Working{}, false
	}
	return w, true
}

// SessionState is what is happening about one session's summaries. The zero value
// is not used: a session with nothing happening has no entry in Status.Sessions.
type SessionState int

const (
	// SessionQueued is a request a worker would take as soon as it reaches it.
	SessionQueued SessionState = iota + 1
	// SessionWorking is the session a worker is generating for right now.
	SessionWorking
	// SessionFailed is a request held back by its failures: waiting out the
	// backoff, or out of attempts.
	SessionFailed
)

// SessionStatus is what the queue knows about one session.
type SessionStatus struct {
	State SessionState
	// Attempts, LastTried and LastError describe earlier failures. A session can
	// carry them in any state, since a failure is retried once its backoff is over.
	Attempts  int
	LastTried time.Time
	LastError string
	// RetryAt is when a failed request becomes ready again, or when a record of
	// giving up stops holding its session back.
	RetryAt time.Time
	// Call, Calls and CallStarted are a working session's, as in Working.
	Call        int
	Calls       int
	CallStarted time.Time
}

// CallLabel names the call a working session is on.
func (s SessionStatus) CallLabel() string {
	if s.Call == 0 {
		return "the session's own summary"
	}
	return fmt.Sprintf("call %d of %d", s.Call, s.Calls)
}

// Status is what can be said about summarizing at one moment. The zero value says
// nothing: summarizing is off, or nothing has looked yet.
type Status struct {
	// Enabled says this was read from a queue, so an empty one means idle rather
	// than unknown.
	Enabled bool
	// Running, PID and Since describe the worker holding the lock.
	Running bool
	PID     int
	Since   time.Time
	// Current is the session in hand, zero when there is none. Its entry in
	// Sessions has the call. It is only ever set with Running: the record of a
	// worker that was killed describes nothing.
	Current domain.SessionKey
	// Queued and Failed count the sessions in those states. The session in hand is
	// in neither.
	Queued int
	Failed int
	// Unreadable counts the files in the queue that do not read as a request.
	Unreadable int
	// Sessions holds every session with a request or in hand.
	Sessions map[domain.SessionKey]SessionStatus
}

// StatusReader reads the status again and again without paying for the queue each
// time: Queue.Headers reads a request only when its file has changed.
//
// It is safe to use from several goroutines: the TUI reads on every tick, and a tick
// can come while the previous one's read is still going.
type StatusReader struct {
	q        *Queue
	lockPath string
}

// NewStatusReader returns a reader over a queue and the worker lock at lockPath.
func NewStatusReader(q *Queue, lockPath string) *StatusReader {
	return &StatusReader{q: q, lockPath: lockPath}
}

// Read returns the status as of now.
func (sr *StatusReader) Read(now time.Time) Status {
	st := Status{Enabled: true, Sessions: map[domain.SessionKey]SessionStatus{}}
	if pid, since, err := LockHolder(sr.lockPath); err == nil {
		st.Running, st.PID, st.Since = true, pid, since
	}
	var requests []Request
	requests, st.Unreadable = sr.q.Headers()
	for _, r := range requests {
		ss := SessionStatus{State: SessionQueued, Attempts: r.Attempts, LastTried: r.LastTried, LastError: r.LastError}
		if r.Ready(now) {
			st.Queued++
		} else {
			ss.State = SessionFailed
			ss.RetryAt = r.LastTried.Add(RetryAfter)
			if r.GaveUp() {
				ss.RetryAt = r.LastTried.Add(GiveUpFor)
			}
			st.Failed++
		}
		st.Sessions[domain.SessionKey{PluginID: r.PluginID, SessionID: r.SessionID}] = ss
	}
	if !st.Running {
		return st
	}
	w, ok := sr.q.readWorking()
	if !ok {
		return st
	}
	st.Current = domain.SessionKey{PluginID: w.PluginID, SessionID: w.SessionID}
	ss := st.Sessions[st.Current]
	switch ss.State {
	case SessionQueued:
		st.Queued--
	case SessionFailed:
		st.Failed--
	}
	ss.State, ss.Call, ss.Calls, ss.CallStarted = SessionWorking, w.Call, w.Calls, w.Started
	ss.RetryAt = time.Time{}
	st.Sessions[st.Current] = ss
	return st
}
