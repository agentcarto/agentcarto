package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentcarto/core/domain"
)

// Markdown export of the whole session shown in the detail view. What is kept
// and what is dropped follows the turn view: the prompt and the reply with
// their bodies, everything else as a one-line entry, and nothing at all for
// thinking, tool output and injected system messages — those show up in the
// turn view as labels like "result (120 lines)", which say nothing on their own.
//
// The lines themselves are built from the events, not from the turn view's
// blocks: a block label is sized for one terminal row (toolCallLabel cuts the
// argument at 70 columns), and a truncated command is worse than useless in a
// document that is read later.

// exportTimeFormat is the timestamp form used throughout the document. Unlike
// the TUI, which shows clock times against today, an exported file is read
// elsewhere and later, so every timestamp carries its date.
const exportTimeFormat = "2006-01-02 15:04:05"

// eventMarkdown renders one event, or nil for the events the export leaves out.
func eventMarkdown(e domain.Event) []string {
	switch e.Kind {
	case domain.EventUser:
		if e.Prompt == "" {
			return nil // injected system message, not something the user typed
		}
		return bodySection("USER", e.Text)
	case domain.EventQueued:
		return bodySection("USER (queued)", e.Text)
	case domain.EventAssistant:
		return bodySection("ASSISTANT", e.Text)
	case domain.EventToolCall:
		name := e.ToolName
		if name == "" {
			name = "tool"
		}
		return toolEntry(name, e.ToolArg, e.ToolDetail)
	case domain.EventTask:
		// A task's ToolDetail is the finished subagent's report — output, not the
		// call — so only its label is kept, as with every other tool result.
		return toolEntry("TASK", e.ToolArg, "")
	case domain.EventAttachment:
		// Likewise the attachment's Text is the injected file content.
		return toolEntry("attachment", e.ToolArg, "")
	}
	return nil
}

// bodySection renders a labeled block of text, or nil when there is no text: an
// assistant event can carry none (encrypted thinking, for instance), and an
// empty **ASSISTANT** heading would stand for nothing.
func bodySection(label, text string) []string {
	body := strings.TrimRight(text, "\n")
	if strings.TrimSpace(body) == "" {
		return nil
	}
	return append([]string{"**" + label + "**", ""}, strings.Split(body, "\n")...)
}

func listEntry(text string) []string {
	if text = strings.TrimSpace(text); text != "" {
		return []string{"- " + text}
	}
	return nil
}

// toolEntry renders a tool call as a list entry. ToolArg is the plugin's
// one-line form of the call and stays on the entry's own line. ToolDetail is
// the same call with its line breaks intact (a heredoc, a patch, a payload):
// when it has more than one line it goes into a code block under the entry
// instead, because the one-line form of a script is unreadable — the plugin
// folded every newline into a space to fit a terminal row.
//
// Nothing is truncated either way. A command cut mid-path says nothing.
func toolEntry(name, arg, detail string) []string {
	detail = strings.TrimRight(detail, "\n")
	if !strings.Contains(detail, "\n") {
		return listEntry(strings.TrimSpace(name + " " + strings.TrimSpace(arg)))
	}
	lines := strings.Split(detail, "\n")
	fence := "  " + fenceFor(lines)
	// Two spaces line the block up under the entry's text ("- " is two columns
	// wide), which is what keeps it part of the list item.
	out := []string{"- " + name, "", fence}
	for _, ln := range lines {
		out = append(out, strings.TrimRight("  "+ln, " "))
	}
	return append(out, fence)
}

// fenceFor returns a code fence longer than any backtick run inside lines, so
// that an argument holding fences of its own (a Markdown file being written,
// say) cannot break out of the block.
func fenceFor(lines []string) string {
	longest := 2
	for _, ln := range lines {
		run := 0
		for _, r := range ln {
			if r == '`' {
				run++
				if run > longest {
					longest = run
				}
				continue
			}
			run = 0
		}
	}
	return strings.Repeat("`", longest+1)
}

// editedFilesMarkdown renders the consolidated file section the turn view puts
// at the top of a turn, with the paths left whole (the turn view shortens them
// to fit a row).
func (m Model) editedFilesMarkdown(events []domain.Event) []string {
	edits := turnFileEdits(events)
	if len(edits) == 0 {
		return nil
	}
	cwd := ""
	if m.detailSession != nil {
		cwd = m.detailSession.CWD
	}
	out := []string{fmt.Sprintf("- Edited files (%d)", len(edits))}
	for _, fe := range edits {
		out = append(out, fmt.Sprintf("  - %s %s (+%d -%d)", fe.op(), relCWD(fe.Path, cwd), fe.Added, fe.Removed))
	}
	return out
}

// turnMarkdown renders one turn: a heading carrying the number the turn view
// shows, the compact notice when the turn sits on a /compact boundary, and the
// events that survive eventMarkdown. A turn left with nothing to show (one that
// holds only injected system messages) produces no heading either.
func (m Model) turnMarkdown(row detailRow) []string {
	events := m.turnEvents(row.Turn)
	body := m.editedFilesMarkdown(events)
	for _, e := range events {
		if skipInFileSection(e) {
			continue // already in the consolidated file section
		}
		lines := eventMarkdown(e)
		if len(lines) == 0 {
			continue
		}
		if len(body) > 0 {
			body = append(body, "")
		}
		body = append(body, lines...)
	}
	if len(body) == 0 {
		return nil
	}
	head := fmt.Sprintf("## Turn %d", row.TurnIndex+1)
	if t := turnTime(events); !t.IsZero() {
		head += " — " + t.Format(exportTimeFormat)
	}
	out := []string{head, ""}
	if row.Badge {
		// The » badge of the turn list. Where the context was compacted matters to
		// someone reading the transcript: the agent stopped seeing what came before.
		out = append(out, "_(context compacted here)_", "")
	}
	return append(append(out, body...), "")
}

// exportHeader renders the session's own metadata. Fields the plugin could not
// fill (a session with no recorded cwd, for instance) are left out rather than
// shown empty.
func (m Model) exportHeader(turns int) []string {
	s := m.detailSession
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = "(untitled)"
	}
	out := []string{"# " + title, ""}
	add := func(k, v string) {
		if v != "" {
			out = append(out, fmt.Sprintf("- **%s**: %s", k, v))
		}
	}
	add("Agent", s.AgentType)
	add("Session", s.SessionID)
	if s.CWD != "" {
		add("CWD", "`"+s.CWD+"`")
	}
	if !s.StartedAt.IsZero() {
		add("Started", s.StartedAt.Format(exportTimeFormat))
	}
	if !s.UpdatedAt.IsZero() {
		add("Updated", s.UpdatedAt.Format(exportTimeFormat))
	}
	add("Turns", fmt.Sprintf("%d", turns))
	return append(out, "")
}

// tightenLists drops the blank line between consecutive list entries, which the
// per-event spacing would otherwise leave behind. Without this a run of tool
// calls renders as a loose list, one paragraph per entry.
func tightenLists(lines []string) []string {
	isEntry := func(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), "- ") }
	out := make([]string, 0, len(lines))
	inFence := false
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			// A tool argument's code block can hold blank lines between lines that
			// start with "- "; those belong to the command, not to the document.
			inFence = !inFence
		}
		if !inFence && ln == "" && i > 0 && i+1 < len(lines) && isEntry(lines[i-1]) && isEntry(lines[i+1]) {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// exportMarkdown renders the whole document and reports how many turns it
// holds. It walks detailRows rather than detailTurns so that what is exported is what the screen lists — same turns,
// same numbering, same compact badges — in chronological order (the rows are
// newest first). Turn numbers can therefore skip: they are the turn view's
// "turn #N", not a count of what the document contains. Branch rows name
// lineages that are not on screen either; they are skipped.
func (m Model) exportMarkdown() (doc string, turns int) {
	if m.detailSession == nil {
		return "", 0
	}
	var body []string
	for i := len(m.detailRows) - 1; i >= 0; i-- {
		if m.detailRows[i].Kind != "turn" {
			continue
		}
		// The turns are rendered before the header so that the count it states is
		// the number of turns the document actually holds, which is also what the
		// flash reports. Turns that render to nothing are not among them.
		if lines := m.turnMarkdown(m.detailRows[i]); len(lines) > 0 {
			body = append(body, lines...)
			turns++
		}
	}
	return strings.Join(tightenLists(append(m.exportHeader(turns), body...)), "\n"), turns
}

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

// exportSession writes the open session to a Markdown file in the directory
// agentcarto was started from — where the user is, not where the session ran,
// which is often someone else's repository.
//
// The write happens inline rather than in a tea.Cmd: it is one small file, and
// the flash has to name the file that was actually created, which is only known
// once O_EXCL has settled the suffix.
func (m Model) exportSession() (tea.Model, tea.Cmd) {
	if m.detailSession == nil {
		return m, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		m.flash = "export failed: " + err.Error()
		return m, nil
	}
	s := m.detailSession
	name := exportFileName(s.AgentType, s.SessionID, time.Now())
	doc, turns := m.exportMarkdown()
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
