package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// editorFinishedMsg reports that an editor launched from the turn view has
// exited. err is non-nil when the editor could not be started, the file was
// unreachable, or the editor itself failed.
type editorFinishedMsg struct{ err error }

// errNoEditor is reported when neither $VISUAL nor $EDITOR names an editor.
var errNoEditor = errors.New("set $EDITOR (or $VISUAL) to open files")

// editorCommand returns the editor to launch, split into a program and the
// arguments that precede the file. $VISUAL takes precedence over $EDITOR, as it
// names the editor suited to a full-screen terminal. Either may carry flags
// ("code -w"), so the value is split on whitespace; an editor whose own path
// contains spaces is not supported.
func editorCommand() (name string, args []string, err error) {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if f := strings.Fields(os.Getenv(env)); len(f) > 0 {
			return f[0], f[1:], nil
		}
	}
	return "", nil, errNoEditor
}

// openInEditor pauses the TUI, runs the editor on path in the same terminal, and
// resumes once it exits. Bubble Tea releases and restores the alt-screen around
// the child process, which is why this does not go through the resume/fork
// handoff — that one replaces agentcarto and never comes back.
//
// Failures are reported as an editorFinishedMsg rather than raised: a missing
// editor or a file the session no longer has on disk is a message to the user,
// not a reason to leave the turn view.
func openInEditor(path string) tea.Cmd {
	fail := func(err error) tea.Cmd {
		return func() tea.Msg { return editorFinishedMsg{err} }
	}
	name, args, err := editorCommand()
	if err != nil {
		return fail(err)
	}
	if _, err := os.Stat(path); err != nil {
		// The log records edits made elsewhere or long ago: the file may be
		// deleted, or the session may come from another machine.
		if os.IsNotExist(err) {
			return fail(fmt.Errorf("file no longer exists: %s", path))
		}
		return fail(fmt.Errorf("cannot open %s: %w", path, err))
	}
	argv := append(append([]string{}, args...), path)
	return tea.ExecProcess(exec.Command(name, argv...), func(err error) tea.Msg {
		return editorFinishedMsg{err}
	})
}
