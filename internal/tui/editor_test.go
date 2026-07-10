package tui

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
	tea "github.com/charmbracelet/bubbletea"
)

func TestEditorCommand(t *testing.T) {
	for _, c := range []struct {
		name       string
		visual     string
		editor     string
		wantName   string
		wantArgs   []string
		wantNoEdit bool
	}{
		{name: "VISUAL wins over EDITOR", visual: "nvim", editor: "vi", wantName: "nvim"},
		{name: "falls back to EDITOR", editor: "vi", wantName: "vi"},
		{name: "flags are split off", editor: "code -w -n", wantName: "code", wantArgs: []string{"-w", "-n"}},
		{name: "blank VISUAL is skipped", visual: "   ", editor: "vi", wantName: "vi"},
		{name: "neither set", wantNoEdit: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("VISUAL", c.visual)
			t.Setenv("EDITOR", c.editor)
			name, args, err := editorCommand()
			if c.wantNoEdit {
				if !errors.Is(err, errNoEditor) {
					t.Fatalf("want errNoEditor, got %v", err)
				}
				return
			}
			if err != nil || name != c.wantName || strings.Join(args, " ") != strings.Join(c.wantArgs, " ") {
				t.Fatalf("editorCommand() = %q %v, %v", name, args, err)
			}
		})
	}
}

// msgOf runs a tea.Cmd that reports a failure without spawning a process.
func msgOf(t *testing.T, cmd tea.Cmd) editorFinishedMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil cmd")
	}
	msg, ok := cmd().(editorFinishedMsg)
	if !ok {
		t.Fatalf("want editorFinishedMsg, got %T", msg)
	}
	return msg
}

// Failures reach the user as a message, never as a crash or a screen change.
func TestOpenInEditorFailures(t *testing.T) {
	t.Run("no editor configured", func(t *testing.T) {
		t.Setenv("VISUAL", "")
		t.Setenv("EDITOR", "")
		if err := msgOf(t, openInEditor("/x/foo.go")).err; !errors.Is(err, errNoEditor) {
			t.Fatalf("want errNoEditor, got %v", err)
		}
	})
	t.Run("file is gone", func(t *testing.T) {
		t.Setenv("EDITOR", "vi")
		err := msgOf(t, openInEditor(filepath.Join(t.TempDir(), "gone.go"))).err
		if err == nil || !strings.Contains(err.Error(), "no longer exists") {
			t.Fatalf("want a not-exists error, got %v", err)
		}
	})
}

func TestAbsCWD(t *testing.T) {
	// Built with filepath.Abs, not by prefixing a separator: on Windows a path
	// needs a volume name ("C:\x") to be absolute, so "\x" would not be.
	abs, err := filepath.Abs(filepath.Join("x", "foo.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, cwd, want string }{
		{abs, "/proj", abs}, // already absolute: left alone
		{"sub/foo.go", "/proj", filepath.Join("/proj", "sub/foo.go")}, // anchored to the session cwd
		{"sub/foo.go", "", "sub/foo.go"},                              // no cwd to anchor it: left alone
		{"", "/proj", ""},
	} {
		if got := absCWD(c.path, c.cwd); got != c.want {
			t.Fatalf("absCWD(%q,%q)=%q want %q", c.path, c.cwd, got, c.want)
		}
	}
}

// Only the edited-files blocks carry a Path, since only they stand for a file
// the user can open. The label keeps its shortened, cwd-relative rendering.
func TestTurnBlocksCarryFilePath(t *testing.T) {
	ev := []domain.Event{
		editEvent(domain.EventToolCall, domain.FileChange{Path: "sub/foo.go", Op: "update", Added: 1, Removed: 1, Diff: "@@\n-a\n+b"}),
		{Kind: domain.EventAssistant, Text: "done"},
	}
	m := Model{
		width: 120, height: 20,
		detailSession: &domain.Session{CWD: "/proj"},
		detail:        &domain.Conversation{Nodes: map[string]domain.ConvNode{"n1": {ID: "n1", Events: ev}}},
	}
	blocks := m.turnBlocksOf([]string{"n1"})
	var withPath, header int
	for _, b := range blocks {
		if b.Path != "" {
			withPath++
			if b.Path != filepath.Join("/proj", "sub/foo.go") {
				t.Fatalf("Path = %q, want it absolute", b.Path)
			}
			if !strings.Contains(b.Label, "sub/foo.go") {
				t.Fatalf("label lost the relative path: %q", b.Label)
			}
		}
		if strings.HasPrefix(b.Label, "Edited files") {
			header++
			if b.Path != "" {
				t.Fatalf("the section header is not a file: %q", b.Path)
			}
		}
	}
	if withPath != 1 || header != 1 {
		t.Fatalf("want 1 file block and 1 header, got %d/%d in %d blocks", withPath, header, len(blocks))
	}
}

// editTurnModel opens the turn-full view on a turn whose assistant edited a file.
// It goes through convMsg and the real keys so detailRows/turnBlocks are built the
// way the running program builds them.
func editTurnModel(t *testing.T) Model {
	t.Helper()
	ts := time.Date(2026, 7, 10, 1, 0, 0, 0, time.Local)
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: ts, Events: []domain.Event{{Kind: domain.EventUser, Text: "go", Prompt: "go"}}},
		{ID: "a1", Parent: "u1", Timestamp: ts, Events: []domain.Event{
			editEvent(domain.EventToolCall, domain.FileChange{Path: "sub/foo.go", Op: "update", Added: 1, Removed: 1, Diff: "@@\n-a\n+b"}),
			{Kind: domain.EventAssistant, Text: "done"},
		}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s", CWD: "/proj"}
	m := Model{width: 120, height: 20, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)
	for i, r := range m.detailRows {
		if r.Kind == "turn" {
			m.detailCursor = i
			break
		}
	}
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // open the turn in full view
	m = u.(Model)
	if !m.turnOpen {
		t.Fatal("test setup: Enter did not open the turn view")
	}
	return m
}

// "e" opens the file under the cursor and does nothing anywhere else, so it never
// steals the key from a block that has no file behind it.
func TestTurnFullEditKey(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "") // keep the test from spawning a real editor
	m := editTurnModel(t)

	fileBlock, otherBlock := -1, -1
	for i, b := range m.turnBlocks {
		if b.Path != "" && fileBlock < 0 {
			fileBlock = i
		}
		if b.Path == "" && b.Label == "ASSISTANT" {
			otherBlock = i
		}
	}
	if fileBlock < 0 || otherBlock < 0 {
		t.Fatalf("test setup: file=%d other=%d in %d blocks", fileBlock, otherBlock, len(m.turnBlocks))
	}
	if want := filepath.Join("/proj", "sub/foo.go"); m.turnBlocks[fileBlock].Path != want {
		t.Fatalf("Path = %q, want %q", m.turnBlocks[fileBlock].Path, want)
	}

	onBlock := func(blk int) Model {
		m.turnCursor = m.turnBlockHeaderLine(blk)
		return m
	}
	_, cmd := onBlock(fileBlock).updateTurnFull(keyRunes("e"))
	if cmd == nil {
		t.Fatal("e on a file block produced no command")
	}
	// $EDITOR is unset, so the command reports that instead of spawning anything.
	if err := msgOf(t, cmd).err; !errors.Is(err, errNoEditor) {
		t.Fatalf("want errNoEditor, got %v", err)
	}
	if _, cmd := onBlock(otherBlock).updateTurnFull(keyRunes("e")); cmd != nil {
		t.Fatal("e on a non-file block should do nothing")
	}
}

// A failed edit surfaces in the turn view's footer; a successful one says nothing.
func TestEditorFinishedFlash(t *testing.T) {
	m := editTurnModel(t)
	u, _ := m.Update(editorFinishedMsg{err: errNoEditor})
	m2 := u.(Model)
	if m2.flash != errNoEditor.Error() {
		t.Fatalf("flash = %q", m2.flash)
	}
	if !strings.Contains(m2.detailView(), "set $EDITOR") {
		t.Fatal("the turn view does not render the flash")
	}
	if u, _ := m.Update(editorFinishedMsg{}); u.(Model).flash != "" {
		t.Fatal("a successful edit should not flash")
	}
}
