package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentcarto/agentcarto/internal/transcript"
)

// Writing the open session out as a Markdown document. The document itself is
// built by internal/transcript, which the CLI renders from as well; what stays
// here is where the file goes and what the user is told about it.

// exportFileName is the name an export is written under: agent, short session
// id and date, e.g. "claude-8f3a2b1c-20260821.md". The session id is sanitized
// because its shape is the plugin's business and may contain path separators.
func exportFileName(agent, sessionID string, day time.Time) string {
	name := sanitizeFileName(agent)
	if name == "" {
		name = "session"
	}
	if id := sanitizeFileName(shortID(sessionID)); id != "" {
		name += "-" + id
	}
	return name + "-" + day.Format("20060102") + ".md"
}

// sanitizeFileName keeps what is safe in a file name on any platform and folds
// everything else into "-".
func sanitizeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// writeNewFile writes content under dir, never over an existing file: O_EXCL
// makes the create fail if the name is taken, and the next suffix is tried.
// Exporting the same session twice in a day is normal (the session has moved
// on), and the second export must not eat the first.
func writeNewFile(dir, name, content string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i <= 20; i++ {
		candidate := name
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		path := filepath.Join(dir, candidate)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err := f.WriteString(content); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("%s: 20 files with this name already exist", name)
}

// exportOptions is what the export asks a document for: a tool call in full,
// because a truncated command is worse than useless in a file read later, and
// nothing of what a subagent reported back — that is output, and the export has
// never carried output. (The CLI's "show --tools full" does carry it, because
// what a search matches on has to be readable somewhere.)
func exportOptions() transcript.Options { return transcript.Options{Tools: transcript.ToolsFull} }

// exportSession writes the open session to a Markdown file in the directory
// agentcarto was started from — where the user is, not where the session ran,
// which is often someone else's repository.
//
// The write happens inline rather than in a tea.Cmd: it is one small file, and
// the flash has to name the file that was actually created, which is only known
// once O_EXCL has settled the suffix.
func (m Model) exportSession() (tea.Model, tea.Cmd) {
	if m.detailSession == nil || m.detail == nil {
		return m, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		m.flash = "export failed: " + err.Error()
		return m, nil
	}
	s := m.detailSession
	name := exportFileName(s.AgentType, s.SessionID, time.Now())
	doc, turns := transcript.Markdown(*s, *m.detail, transcript.Turns(*m.detail, m.currentDetailPath()), exportOptions())
	path, err := writeNewFile(dir, name, doc)
	if err != nil {
		m.flash = "export failed: " + err.Error()
		return m, nil
	}
	// "./" spells out that the file landed in the current directory, which the
	// bare name would leave to guesswork. The path is kept short on purpose: a
	// flash is clipped to the window width, and the name is the part that matters.
	m.flash = fmt.Sprintf("Exported %s to ./%s", plural(turns, "turn"), filepath.Base(path))
	return m, nil
}
