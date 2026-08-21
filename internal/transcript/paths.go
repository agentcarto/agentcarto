package transcript

import (
	"os"
	"path/filepath"
	"strings"
)

// RelCWD returns path relative to the session's working directory when it is
// inside it; paths outside cwd (or already relative) are returned unchanged.
func RelCWD(path, cwd string) string {
	if cwd == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return path
	}
	return rel
}
