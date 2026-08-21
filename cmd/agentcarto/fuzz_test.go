package main

import (
	"testing"
	"time"
)

// FuzzParseTurnSpec throws arbitrary text at the --turns parser. Whatever comes
// out has to be usable: no panic, no empty selection reported as valid, and no
// range that runs backwards.
func FuzzParseTurnSpec(f *testing.F) {
	for _, seed := range []string{"12", "12-14", "3,7,12-14", "", ",,", "-1", "1-", "0", "9999999999999999999", "1-2-3", " 4 , 5 "} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		sel, err := parseTurnSpec(spec)
		if err != nil {
			return
		}
		if len(sel.explicit) == 0 && len(sel.ranges) == 0 {
			t.Fatalf("%q was accepted but selects nothing", spec)
		}
		for n := range sel.explicit {
			if n < 1 {
				t.Fatalf("%q selected turn %d", spec, n)
			}
		}
		for _, r := range sel.ranges {
			if r[0] < 1 || r[1] < r[0] {
				t.Fatalf("%q produced the range %v", spec, r)
			}
		}
	})
}

// FuzzParseSince does the same for --since: what it accepts must be a time in
// the past (an age) or a date, never a moment after now.
func FuzzParseSince(f *testing.F) {
	for _, seed := range []string{"7d", "2w", "12h", "90m", "2026-08-01", "", "-1d", "yesterday", "99999999999999999999d", "0d", "1e9h"} {
		f.Add(seed)
	}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, in string) {
		got, err := parseSince(in, now)
		if err != nil {
			return
		}
		// A date is taken as written — asking for sessions since the year 7000 is
		// the caller's business — but an age must never resolve to the future.
		if _, isDate := time.ParseInLocation("2006-01-02", in, time.Local); isDate == nil {
			return
		}
		if got.After(now) {
			t.Fatalf("--since %q resolved to %v, which is after %v", in, got, now)
		}
	})
}
