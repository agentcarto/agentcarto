package transcript

import (
	"path/filepath"
	"runtime"
	"testing"
)

// abs builds an absolute path for the platform the test runs on: "/repo/app"
// is not absolute on Windows, where a path needs a volume, and a fixture that
// forgets it silently stops being a path the code under test treats as one.
func abs(slash string) string {
	p := filepath.FromSlash(slash)
	if runtime.GOOS == "windows" {
		p = `C:` + p
	}
	return p
}

// Edited-file paths render relative to the session's working directory;
// paths outside it (or already relative) stay as-is.
func TestRelCWD(t *testing.T) {
	rel := func(slash string) string { return filepath.FromSlash(slash) }
	cases := []struct{ path, cwd, want string }{
		{abs("/repo/app/internal/x.go"), abs("/repo/app"), rel("internal/x.go")},
		{abs("/etc/hosts"), abs("/repo/app"), abs("/etc/hosts")},
		{abs("/repo/app2/x.go"), abs("/repo/app"), abs("/repo/app2/x.go")}, // sibling with a shared name prefix
		{rel("internal/x.go"), abs("/repo/app"), rel("internal/x.go")},     // already relative
		{abs("/repo/app/x.go"), "", abs("/repo/app/x.go")},                 // no cwd known
	}
	for _, c := range cases {
		if got := RelCWD(c.path, c.cwd); got != c.want {
			t.Fatalf("RelCWD(%q, %q)=%q want %q", c.path, c.cwd, got, c.want)
		}
	}
}
