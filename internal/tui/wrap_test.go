package tui

import (
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestWrapWidth(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want []string
	}{
		{"empty keeps one blank line", "", 10, []string{""}},
		{"fits in width", "hello", 10, []string{"hello"}},
		{"exact width", "hello", 5, []string{"hello"}},
		{"ascii hard wrap", "abcdef", 4, []string{"abcd", "ef"}},
		{"non-positive width returns as-is", "abcdef", 0, []string{"abcdef"}},
		// Intentional CJK (multibyte) test data: full-width chars are display-width 2. At width 5,
		// "あいう" (width 6) breaks after "あい" (4) with "う" (2) on the next line; it never splits a char.
		{"cjk wraps by display width", "あいう", 5, []string{"あい", "う"}},
		// Intentional CJK test data: even mixing full-width and half-width, wrapping is by display width.
		{"mixed width", "aあbいc", 4, []string{"aあb", "いc"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wrapWidth(c.s, c.n)
			if len(got) != len(c.want) {
				t.Fatalf("len=%d want %d (%q)", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("seg[%d]=%q want %q", i, got[i], c.want[i])
				}
				if c.n > 0 && runewidth.StringWidth(got[i]) > c.n {
					t.Fatalf("seg[%d]=%q width %d exceeds %d", i, got[i], runewidth.StringWidth(got[i]), c.n)
				}
			}
		})
	}
}
