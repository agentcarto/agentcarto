package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mattn/go-runewidth"
)

// Input-completion and path-validation logic for relocate (the "m" key).
// A set of pure functions with no bubbletea dependency, covering path completion,
// directory-candidate enumeration, and relocate confirmation.

// relocCycle holds the Tab-completion candidate cycling state.
// After the input has been extended to the common prefix, it is retained so that
// the candidate directories can be cycled through one at a time.
type relocCycle struct {
	target  string   // directory being enumerated
	names   []string // candidate directory names (immediate children of target)
	idx     int      // index of the current candidate
	applied string   // value most recently placed in the input field (used to detect unedited input)
}

// expandUser expands a leading "~" to the home directory (equivalent to os.path.expanduser).
func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, e := os.UserHomeDir(); e == nil {
			return home + p[1:]
		}
	}
	return p
}

// commonPrefix returns the longest common prefix of the given strings (byte-wise, equivalent to os.path.commonprefix).
func commonPrefix(names []string) string {
	if len(names) == 0 {
		return ""
	}
	pre := names[0]
	for _, n := range names[1:] {
		i := 0
		for i < len(pre) && i < len(n) && pre[i] == n[i] {
			i++
		}
		pre = pre[:i]
		if pre == "" {
			break
		}
	}
	return pre
}

// dirCandidates splits the input text into (directory to enumerate, basename being typed,
// candidate directory names). The relocation target is a directory, so candidates are
// directories only. A trailing "/" means every entry inside it.
func dirCandidates(text string) (target, base string, names []string) {
	expanded := expandUser(text)
	var dir string
	if strings.HasSuffix(expanded, "/") {
		dir, base = expanded, ""
	} else {
		dir, base = filepath.Dir(expanded), filepath.Base(expanded)
	}
	target = dir
	if target == "" {
		target = "/"
	}
	entries, e := os.ReadDir(target)
	if e != nil {
		return target, base, nil
	}
	for _, en := range entries {
		name := en.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		if info, e := os.Stat(filepath.Join(target, name)); e == nil && info.IsDir() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return target, base, names
}

// completePath returns the Tab-completion result for a relocation path: (completed text, candidate directory names).
// Shell-like behavior: with multiple candidates it extends to the longest common prefix; with a unique
// candidate it appends a trailing "/". If there are no candidates or enumeration fails, it returns text unchanged.
func completePath(text string) (string, []string) {
	target, _, names := dirCandidates(text)
	if len(names) == 0 {
		return text, nil
	}
	if len(names) == 1 {
		return strings.TrimRight(filepath.Join(target, names[0]), "/") + "/", names
	}
	return filepath.Join(target, commonPrefix(names)), names
}

// normalizeRelocPath normalizes the new input path (trims whitespace and a trailing slash, but keeps the root "/").
func normalizeRelocPath(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1 {
		s = strings.TrimRight(s, "/")
	}
	return s
}

// validateRelocPath validates the normalized new path. On a problem it returns (flash, false);
// if it equals the current cwd (which is treated as a cancel) it returns (flash, false) with cancel=true.
// On success it returns ("", true).
// Returns: msg=flash text, ok=whether to proceed to confirm, cancel=whether to collapse relocate entirely.
func validateRelocPath(newPath, oldCwd string) (msg string, ok, cancel bool) {
	if newPath == "" || !filepath.IsAbs(newPath) {
		return "Enter an absolute path", false, false
	}
	if newPath == oldCwd {
		return "New path is the same as the current cwd", false, true
	}
	if info, e := os.Stat(newPath); e != nil || !info.IsDir() {
		return "Destination does not exist: " + newPath, false, false
	}
	return "", true, false
}

// candidateGrid arranges the candidate display names into a zsh-style grid (column-major, i.e. filled downward).
// It chooses how many columns fit within the display width and returns, per row, the candidate index of each
// cell (-1 for an empty cell). cellW is the display width of one cell including the inter-column gap.
func candidateGrid(names []string, width int) (rows [][]int, cellW int) {
	if len(names) == 0 {
		return nil, 0
	}
	maxw := 0
	for _, n := range names {
		if w := runewidth.StringWidth(n); w > maxw {
			maxw = w
		}
	}
	cellW = maxw + 2 // inter-column gap
	ncols := width / cellW
	if ncols < 1 {
		ncols = 1
	}
	if ncols > len(names) {
		ncols = len(names)
	}
	nrows := (len(names) + ncols - 1) / ncols
	rows = make([][]int, nrows)
	for r := range rows {
		rows[r] = make([]int, ncols)
		for c := 0; c < ncols; c++ {
			if idx := c*nrows + r; idx < len(names) { // fill column-major
				rows[r][c] = idx
			} else {
				rows[r][c] = -1
			}
		}
	}
	return rows, cellW
}
