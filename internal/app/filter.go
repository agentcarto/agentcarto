package app

import (
	"github.com/agentcarto/core/domain"
	"path/filepath"
	"strings"
	"time"
)

// SessionFilter narrows a session list to the ones a question is about. The
// zero value keeps everything except empty forks.
type SessionFilter struct {
	// CWD keeps the sessions that ran in this directory or below it. It is
	// compared as a cleaned path, so a trailing separator or a "." segment does
	// not change the answer.
	CWD string
	// Agent keeps one agent, by plugin id ("claude") or by agent type. The two
	// are the same for most plugins, and telling a user they differ is not worth
	// a second flag.
	Agent string
	// Since keeps the sessions last updated at or after this instant.
	Since time.Time
	// IncludeEmptyForks keeps forks that were never continued. They are a full
	// copy of their parent, so a search that keeps them reports every hit twice.
	IncludeEmptyForks bool
}

// FilterSessions returns the sessions the filter keeps, in the order given.
func FilterSessions(sessions []domain.Session, f SessionFilter) []domain.Session {
	cwd := ""
	if f.CWD != "" {
		cwd = filepath.Clean(f.CWD)
	}
	agent := strings.ToLower(f.Agent)
	var out []domain.Session
	for _, s := range sessions {
		if s.EmptyFork && !f.IncludeEmptyForks {
			continue
		}
		if cwd != "" && !UnderDir(s.CWD, cwd) {
			continue
		}
		if agent != "" && strings.ToLower(s.PluginID) != agent && strings.ToLower(s.AgentType) != agent {
			continue
		}
		if !f.Since.IsZero() && s.UpdatedAt.Before(f.Since) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// UnderDir reports whether path is dir or sits inside it. A plain string prefix
// would also match a sibling whose name starts with the same letters
// ("/repo/app2" under "/repo/app"), so the separator is required.
func UnderDir(path, dir string) bool {
	if path == "" {
		return false
	}
	path = filepath.Clean(path)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}
