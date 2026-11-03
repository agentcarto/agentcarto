package tui

import (
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

func ev(ts time.Time) domain.Event {
	return domain.Event{Kind: domain.EventAssistant, Timestamp: ts}
}

func ts(unix int64) time.Time {
	return time.Unix(unix, 0)
}

// turnDuration returns the difference from the minimum timestamp to the maximum timestamp.
func TestTurnDurationSpansMinToMax(t *testing.T) {
	turn := []domain.Event{ev(ts(100)), ev(ts(130)), ev(ts(145))}
	if got := turnDuration(turn); got != 45*time.Second {
		t.Fatalf("got %v, want 45s", got)
	}
}

// Even if events are not in chronological order, the duration is always computed from min to max.
func TestTurnDurationOutOfOrder(t *testing.T) {
	// The order is 145 -> 100 -> 130, but the duration is max(145)-min(100)=45s.
	turn := []domain.Event{ev(ts(145)), ev(ts(100)), ev(ts(130))}
	if got := turnDuration(turn); got != 45*time.Second {
		t.Fatalf("got %v, want 45s", got)
	}
}

// Events written while no turn was running are not part of the turn's time: an idle compaction the
// turn is filed under (here forty minutes before the work) and a recap after it (half an hour later).
func TestTurnDurationLeavesOutBetweenTurns(t *testing.T) {
	between := func(e domain.Event) domain.Event { e.BetweenTurns = true; return e }
	turn := []domain.Event{between(ev(ts(-2300))), ev(ts(100)), ev(ts(145)), between(ev(ts(1900)))}
	if got := turnDuration(turn); got != 45*time.Second {
		t.Fatalf("got %v, want 45s", got)
	}
	if got := transcript.TurnTime(turn); !got.Equal(ts(100)) {
		t.Fatalf("TurnTime = %v, want %v", got, ts(100))
	}
	// A turn that holds nothing else keeps its time rather than losing it.
	only := []domain.Event{between(ev(ts(100))), between(ev(ts(130)))}
	if got := transcript.TurnTime(only); !got.Equal(ts(100)) {
		t.Fatalf("TurnTime of a turn of between-turn events only = %v, want %v", got, ts(100))
	}
	if got := turnDuration(only); got != 30*time.Second {
		t.Fatalf("duration of a turn of between-turn events only = %v, want 30s", got)
	}
}

// TurnTime/turnEndTime return the min/max regardless of order.
func TestTurnTimeEndTimeAreMinMax(t *testing.T) {
	turn := []domain.Event{ev(ts(145)), ev(ts(100)), ev(ts(130))}
	if got := transcript.TurnTime(turn); !got.Equal(ts(100)) {
		t.Fatalf("TurnTime = %v, want %v", got, ts(100))
	}
	if got := turnEndTime(turn); !got.Equal(ts(145)) {
		t.Fatalf("turnEndTime = %v, want %v", got, ts(145))
	}
}

// In-progress turn: if now is later than the latest event time, the elapsed time extends to now.
func TestTurnElapsedExtendsToNow(t *testing.T) {
	turn := []domain.Event{ev(ts(100)), ev(ts(130))}
	// now=170 -> max(latest 130, now 170)=170, 170-100=70s.
	if got := turnElapsed(turn, ts(170)); got != 70*time.Second {
		t.Fatalf("got %v, want 70s", got)
	}
}

// If now is earlier than the latest event time (clock skew, etc.), it stops at the latest event time.
func TestTurnElapsedNowBeforeLastEvent(t *testing.T) {
	turn := []domain.Event{ev(ts(100)), ev(ts(130))}
	if got := turnElapsed(turn, ts(120)); got != 30*time.Second {
		t.Fatalf("got %v, want 30s (last event)", got)
	}
}

// now=zero is treated as a completed turn and matches turnDuration.
func TestTurnElapsedZeroNowEqualsDuration(t *testing.T) {
	turn := []domain.Event{ev(ts(100)), ev(ts(130))}
	if turnElapsed(turn, time.Time{}) != turnDuration(turn) {
		t.Fatalf("zero now must equal turnDuration")
	}
}

// Zero timestamps are ignored.
func TestTurnDurationIgnoresZero(t *testing.T) {
	turn := []domain.Event{ev(time.Time{}), ev(ts(100)), ev(time.Time{}), ev(ts(160))}
	if got := turnDuration(turn); got != 60*time.Second {
		t.Fatalf("got %v, want 60s", got)
	}
}

// Fewer than two timestamps (or all equal) yields 0.
func TestTurnDurationInsufficient(t *testing.T) {
	if got := turnDuration([]domain.Event{ev(ts(100))}); got != 0 {
		t.Fatalf("single event: got %v, want 0", got)
	}
	if got := turnDuration([]domain.Event{ev(ts(100)), ev(ts(100))}); got != 0 {
		t.Fatalf("equal timestamps: got %v, want 0", got)
	}
	if got := turnDuration(nil); got != 0 {
		t.Fatalf("empty: got %v, want 0", got)
	}
}
