package transcript

import (
	"path/filepath"
	"runtime"
	"testing"
)

// Edited-file paths render relative to the session's working directory;
// paths outside it (or already relative) stay as-is. Absolute paths are
// built per-platform: on Windows "/repo/app" is not absolute (no drive),
// so the fixtures get a volume prefix there.
func TestRelCWD(t *testing.T) {
	abs := func(slash string) string {
		p := filepath.FromSlash(slash)
		if runtime.GOOS == "windows" {
			p = `C:` + p
		}
		return p
	}
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
