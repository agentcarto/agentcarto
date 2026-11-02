package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// summaryOverview says where summarizing stands as a whole, for the list header, most
// urgent last. It is empty with summarizing off: that is the default, and a mark on
// every screen would be saying so to everyone who never asked for the feature.
func (m Model) summaryOverview() []string {
	st := m.summaryStatus
	if !st.Enabled {
		return nil
	}
	var parts []string
	if st.Running {
		parts = append(parts, "running")
	}
	if st.Queued > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", st.Queued))
	}
	if st.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", st.Failed))
	}
	if len(parts) == 0 {
		return []string{"idle"}
	}
	return parts
}

// overviewText renders the parts of an overview as the header shows them.
func overviewText(parts []string) string {
	return summaryMark + " " + strings.Join(parts, " · ")
}

// summaryStatusColor is the color of a line about summarizing: the one that marks
// generated text, unless the line reports a failure.
func summaryStatusColor(failed bool) lipgloss.Color {
	if failed {
		return roleColor("del")
	}
	return roleColor("meta")
}

// listHeadLine lays out the list's first line with the summary overview at its right
// end. Measured from the right so it stays put while "scanning..." comes and goes on
// the left every few seconds. The left side keeps what it always had, so on a narrow
// terminal the overview gives way, whole parts at a time and the least urgent first:
// cut at the edge, it would lose the failures before anything else.
func (m Model) listHeadLine(left string, w int) string {
	left = clip(left, w)
	parts := m.summaryOverview()
	room := w - runewidth.StringWidth(left) - 1
	if len(parts) == 0 || room <= 0 {
		return left
	}
	right := overviewText(parts)
	for len(parts) > 1 && runewidth.StringWidth(right) > room {
		parts = parts[1:]
		right = overviewText(parts)
	}
	right = clip(right, room)
	gap := w - runewidth.StringWidth(left) - runewidth.StringWidth(right)
	return left + strings.Repeat(" ", gap) + styled(right, summaryStatusColor(m.summaryStatus.Failed > 0), false, false)
}

// sessionSummaryLine says where one session's summaries stand, in as many words as a
// line of its own allows. It is empty for a session with nothing happening, and a
// session with no status entry has nothing happening.
func sessionSummaryLine(ss summary.SessionStatus, now time.Time) string {
	switch ss.State {
	case summary.SessionWorking:
		text := "summarizing now, " + ss.CallLabel()
		if took := formatDuration(now.Sub(ss.CallStarted)); took != "" {
			text += " (" + took + ")"
		}
		return summaryMark + " " + text
	case summary.SessionQueued:
		if ss.Attempts == 0 {
			return summaryMark + " queued"
		}
		return summaryMark + fmt.Sprintf(" queued to retry, failed %d×", ss.Attempts) + failureReason(ss)
	case summary.SessionFailed:
		text := fmt.Sprintf(" failed %d×", ss.Attempts)
		if !ss.RetryAt.IsZero() {
			// A status is read every few seconds, so its backoff can be over by the
			// time it is drawn. No time left reads as an empty duration otherwise.
			if left := formatDuration(ss.RetryAt.Sub(now)); left != "" {
				text += ", retry in " + left
			} else {
				text += ", retrying soon"
			}
		}
		return summaryMark + text + failureReason(ss)
	}
	return ""
}

// failureReason is the reason as the end of a line. A request written before reasons
// were kept has none until it fails again, and must not read as one cut off.
func failureReason(ss summary.SessionStatus) string {
	if ss.LastError == "" {
		return ""
	}
	return ": " + ss.LastError
}

// listStatusLine is the reserved line over the list: the selected session's summary
// status, or blank. Blank is still spaces to the full width, which is what keeps the
// rows under it from jittering as the search box comes and goes (see View).
func (m Model) listStatusLine(now time.Time) string {
	w := max(1, m.width-1)
	s, ok := m.selected()
	if !ok {
		return strings.Repeat(" ", w)
	}
	ss := m.summaryStatus.Sessions[s.Key()]
	line := sessionSummaryLine(ss, now)
	if line == "" {
		return strings.Repeat(" ", w)
	}
	return styled(padCol(clip(" "+line, w), w), summaryStatusColor(ss.State == summary.SessionFailed), false, false)
}

// summaryRowMark is the mark a list row carries for its summaries. Only for what a
// reader goes looking for: a queue can hold most of the list, and marking every
// queued row would mark nothing.
func summaryRowMark(state summary.SessionState, selected bool) string {
	switch state {
	case summary.SessionFailed:
		return styled("! ", summaryStatusColor(true), selected, false)
	case summary.SessionWorking:
		return styled("⟳ ", summaryStatusColor(false), selected, false)
	}
	return ""
}

// detailSummaryNote is what the detail header says about this session's summaries:
// how many of its turns are described, then what is happening about the rest. The
// reason a request failed is left to the list's status line: this line already holds
// the session's times and counts, and has no room for a sentence.
func (m Model) detailSummaryNote(ss summary.SessionStatus) string {
	if !m.summaryStatus.Enabled {
		return ""
	}
	note := ""
	if m.summaryTurns > 0 {
		described := 0
		for turn, sum := range m.summaries {
			if turn != 0 && strings.TrimSpace(sum.Text) != "" {
				described++
			}
		}
		note = styled(fmt.Sprintf("  %s%d/%d", summaryMark, described, m.summaryTurns), summaryStatusColor(false), false, false)
	}
	switch ss.State {
	case summary.SessionWorking:
		text := "  summarizing"
		if ss.Call > 0 {
			text += fmt.Sprintf(" %d/%d", ss.Call, ss.Calls)
		}
		note += styled(text, summaryStatusColor(false), false, false)
	case summary.SessionQueued:
		note += styled("  queued", summaryStatusColor(false), false, false)
	case summary.SessionFailed:
		note += styled(fmt.Sprintf("  failed %d×", ss.Attempts), summaryStatusColor(true), false, false)
	}
	return note
}
