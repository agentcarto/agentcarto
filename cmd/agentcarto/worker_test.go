package main

import (
	"testing"
	"time"
)

// The guard is against regenerating in a loop, not against regenerating at all:
// a session that grew must still be summarized again once the window passes.
func TestTooSoon(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		when       time.Time
		summarized bool
		want       bool
	}{
		{"never summarized", time.Time{}, false, false},
		{"just now", now.Add(-time.Minute), true, true},
		{"inside the window", now.Add(-regenerateWithin + time.Minute), true, true},
		{"exactly the window", now.Add(-regenerateWithin), true, false},
		{"past the window", now.Add(-regenerateWithin - time.Minute), true, false},
		{"long ago", now.Add(-30 * 24 * time.Hour), true, false},
		// An unreadable store answers "not summarized" with a zero time. Treating
		// that as recent would stop summarizing entirely; treating it as never is
		// what the guard is for — the caller then pays once, not on every run,
		// because a successful write moves the time forward.
		{"unreadable store", time.Time{}, false, false},
		// A clock that moved backwards (a laptop waking, an NTP step) must not
		// make a summary look like it comes from the future and block forever.
		{"written in the future", now.Add(time.Hour), true, true},
	}
	for _, c := range cases {
		if got := tooSoon(c.when, c.summarized, now); got != c.want {
			t.Errorf("%s: tooSoon=%v want %v", c.name, got, c.want)
		}
	}
}

func TestShort8(t *testing.T) {
	if got := short8("f64330cd-b244-4e5d"); got != "f64330cd" {
		t.Errorf("short8=%q", got)
	}
	if got := short8("abc"); got != "abc" {
		t.Errorf("short8 of a short id=%q", got)
	}
	if got := short8(""); got != "" {
		t.Errorf("short8 of an empty id=%q", got)
	}
}
