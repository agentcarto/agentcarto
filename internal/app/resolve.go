package app

import (
	"fmt"
	"github.com/agentcarto/core/domain"
	"path/filepath"
	"strings"
)

// ResolveSession finds the session a reference names: a full session id, or a
// prefix of one that only one session answers to. A prefix is how the ids are
// read anywhere else (the list truncates them, and so does a search result), so
// requiring the whole thing would mean copying 36 characters by hand.
//
// An exact id wins over a prefix match, and the error for an ambiguous
// reference names the candidates rather than picking one.
func ResolveSession(sessions []domain.Session, ref string) (domain.Session, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return domain.Session{}, fmt.Errorf("no session given")
	}
	// Case is folded because ids are copied by hand between a terminal, a log
	// path and a search result, and not every agent writes them in one case.
	want := strings.ToLower(ref)
	var exact, prefix []domain.Session
	for _, s := range sessions {
		id := strings.ToLower(s.SessionID)
		switch {
		case id == want:
			exact = append(exact, s)
		case strings.HasPrefix(id, want):
			prefix = append(prefix, s)
		}
	}
	found := exact
	if len(found) == 0 {
		found = prefix
	}
	switch len(found) {
	case 0:
		return domain.Session{}, fmt.Errorf("no session matches %q", ref)
	case 1:
		return found[0], nil
	}
	advice := "name one of them in full, or pass --source with its path"
	if sameID(found) {
		// Naming it in full changes nothing here: these sessions really do carry
		// the same id (codex writes one rollout per resume, and a fork keeps its
		// parent's), and only the log path tells them apart.
		advice = "they share this id — pass --source with the path of the one you want"
	}
	return domain.Session{}, fmt.Errorf("%q matches %d sessions:\n%s\n%s", ref, len(found), candidateList(found), advice)
}

// ResolveSource finds the session whose log is at path. Two sessions can share
// an id (a fork keeps its parent's, and some agents reuse one across resumes),
// and the log path is what tells them apart.
func ResolveSource(sessions []domain.Session, path string) (domain.Session, error) {
	want := filepath.Clean(path)
	for _, s := range sessions {
		if filepath.Clean(s.SourceRef.Source) == want {
			return s, nil
		}
	}
	return domain.Session{}, fmt.Errorf("no session is stored at %q", path)
}

// sameID reports whether every candidate carries the same session id.
func sameID(sessions []domain.Session) bool {
	for _, s := range sessions[1:] {
		if s.SessionID != sessions[0].SessionID {
			return false
		}
	}
	return true
}

// candidateList prints at most ten candidates, which is enough to tell them
// apart without burying the message that follows.
func candidateList(sessions []domain.Session) string {
	var b strings.Builder
	for i, s := range sessions {
		if i == 10 {
			fmt.Fprintf(&b, "  … and %d more\n", len(sessions)-i)
			break
		}
		fmt.Fprintf(&b, "  %s %s  %s  %s\n", s.PluginID, s.SessionID, s.CWD, s.SourceRef.Source)
	}
	return strings.TrimRight(b.String(), "\n")
}
