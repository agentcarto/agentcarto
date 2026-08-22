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
	// Agent keeps one agent, by plugin id ("claude"), by agent type, or by the
	// family the two share ("copilot" for copilot-vc and copilot-jb). The three
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
		if agent != "" && !isAgent(s, agent) {
			continue
		}
		if !f.Since.IsZero() && s.UpdatedAt.Before(f.Since) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// isAgent reports whether the session belongs to the named agent. A name that
// is the family of several plugins matches all of them: nobody asking for
// "copilot" means one of its two editors and not the other, and being told to
// write "copilot-vc" for a session the list already labels "copilot-vc" is a
// worse answer than matching both.
func isAgent(s domain.Session, want string) bool {
	for _, name := range []string{strings.ToLower(s.PluginID), strings.ToLower(s.AgentType)} {
		if name == want || strings.HasPrefix(name, want+"-") {
			return true
		}
	}
	return false
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

// MergeDeletedLogs adds back the sessions the cache remembers and the scan no
// longer finds. An agent deletes its own logs — a cleanup, a project directory
// that moved — and until now that removed the session from agentcarto entirely,
// although everything needed to read it was still in the cache.
//
// A session is only called deleted when its plugin scanned successfully. A
// plugin that crashed or was misconfigured finds nothing either, and marking
// every one of its sessions as deleted would turn one bad start-up into a wall
// of wrong answers.
func MergeDeletedLogs(scanned, cached []domain.Session, successful map[string]bool) []domain.Session {
	found := make(map[string]bool, len(scanned))
	for _, s := range scanned {
		found[s.SourceRef.Source] = true
	}
	out := scanned
	for _, s := range cached {
		if s.SourceRef.Source == "" || found[s.SourceRef.Source] || !successful[s.PluginID] {
			continue
		}
		s.LogDeleted = true
		// Whatever the cache remembers of a running session is stale: the process
		// it belonged to is not being watched any more.
		s.Status, s.PermissionWait = "", false
		out = append(out, s)
	}
	return out
}
