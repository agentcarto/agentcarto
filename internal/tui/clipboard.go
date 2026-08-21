package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"github.com/agentcarto/core/domain"
)

// copyToClipboard hands text to the terminal with OSC 52, which reaches the
// clipboard of the machine the terminal runs on — the one the user pastes from,
// even when agentcarto runs over SSH (a local xclip/pbcopy would write the
// wrong machine's clipboard). It is fire-and-forget: the terminal never
// answers, so a refusal (tmux without set-clipboard, a terminal that caps the
// payload) cannot be reported here.
//
// The write is deferred into a tea.Cmd rather than run inline so the key
// handlers stay testable: a test drives Update without running the returned
// command, and the developer's own clipboard is left alone.
func copyToClipboard(text string) tea.Cmd {
	return func() tea.Msg {
		termenv.Copy(text)
		return nil
	}
}

// copiedFlash reports what was copied. The clipboard gives no visible feedback
// of its own, and OSC 52 cannot be confirmed, so this is the only sign the key
// did anything.
func copiedFlash(text string) string {
	n := len(strings.Split(text, "\n"))
	unit := "lines"
	if n == 1 {
		unit = "line"
	}
	return fmt.Sprintf("Copied %d %s to the clipboard", n, unit)
}

// turnCopyText is the exchange of a turn: what the user asked and what the agent
// answered, in the order they happened, each under the same role label the full
// turn view shows. Tool calls, their output and thinking are left out — those
// are copied one block at a time from the full turn view instead.
//
// A turn holds several such events (a reply interleaved with tool calls, a
// queued prompt sent mid-turn), so every one of them is kept.
func turnCopyText(events []domain.Event) string {
	var parts []string
	for _, e := range events {
		label := ""
		switch {
		case e.Kind == domain.EventUser && e.Prompt != "":
			label = "USER"
		case e.Kind == domain.EventQueued:
			label = "USER (queued)"
		case e.Kind == domain.EventAssistant:
			label = "ASSISTANT"
		default:
			// EventUser without a Prompt is an injected system message, not
			// something the user typed.
			continue
		}
		if t := strings.TrimRight(e.Text, "\n"); strings.TrimSpace(t) != "" {
			parts = append(parts, label+"\n"+t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// blockCopyText is what copying a block in the full turn view yields: its raw
// body, or the label when the block is a one-liner with no body (a tool call
// whose argument is the whole content, for instance).
func blockCopyText(b turnBlock) string {
	text := strings.TrimRight(strings.Join(b.Body, "\n"), "\n")
	if strings.TrimSpace(text) == "" {
		return strings.TrimSpace(b.Label)
	}
	return text
}

// copyTurnExchange copies the prompt and reply of the selected turn (turn list view).
func (m Model) copyTurnExchange() (tea.Model, tea.Cmd) {
	row, ok := m.selectedDetailRow()
	if !ok || row.Kind != "turn" {
		m.flash = "Select a turn row to copy it"
		return m, nil
	}
	text := turnCopyText(m.turnEvents(row.Turn))
	if text == "" {
		m.flash = "This turn has no prompt or reply to copy"
		return m, nil
	}
	m.flash = copiedFlash(text)
	return m, copyToClipboard(text)
}

// copyTurnBlock copies the block under the cursor (full turn view), so the reply,
// a thinking block or a tool result can each be taken on its own.
func (m Model) copyTurnBlock() (tea.Model, tea.Cmd) {
	blk := m.blockAtCursor()
	if blk < 0 || blk >= len(m.turnBlocks) {
		return m, nil
	}
	text := blockCopyText(m.turnBlocks[blk])
	if text == "" {
		m.flash = "Nothing to copy in this block"
		return m, nil
	}
	m.flash = copiedFlash(text)
	return m, copyToClipboard(text)
}
