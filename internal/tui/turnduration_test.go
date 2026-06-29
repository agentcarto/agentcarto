package tui

import (
	"testing"
	"time"

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

// turnTime/turnEndTime return the min/max regardless of order.
func TestTurnTimeEndTimeAreMinMax(t *testing.T) {
	turn := []domain.Event{ev(ts(145)), ev(ts(100)), ev(ts(130))}
	if got := turnTime(turn); !got.Equal(ts(100)) {
		t.Fatalf("turnTime = %v, want %v", got, ts(100))
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
