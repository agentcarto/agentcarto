package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

// The file name carries agent, short id and date, with anything unsafe folded away.
func TestExportFileName(t *testing.T) {
	day := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	cases := []struct{ agent, id, want string }{
		{"claude", "8f3a2b1c-4d5e-6f70", "claude-8f3a2b1c-20260821.md"},
		{"copilot", "a/b:c", "copilot-a-b-c-20260821.md"},
		{"", "", "session-20260821.md"},
	}
	for _, c := range cases {
		if got := exportFileName(c.agent, c.id, day); got != c.want {
			t.Errorf("exportFileName(%q, %q)=%q want %q", c.agent, c.id, got, c.want)
		}
	}
}

// An export never overwrites: the second one lands next to the first.
func TestWriteNewFileNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	first, err := writeNewFile(dir, "s-20260821.md", "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := writeNewFile(dir, "s-20260821.md", "two")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "s-20260821.md" || filepath.Base(second) != "s-20260821-2.md" {
		t.Fatalf("names=%q, %q want s-20260821.md, s-20260821-2.md", filepath.Base(first), filepath.Base(second))
	}
	if b, err := os.ReadFile(first); err != nil || string(b) != "one" {
		t.Fatalf("the first export was not left intact: %q %v", b, err)
	}
	if b, err := os.ReadFile(second); err != nil || string(b) != "two" {
		t.Fatalf("second export content=%q %v", b, err)
	}
}

// x writes the session to the directory agentcarto runs in and reports the file.
func TestExportKeyWritesFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "やって", Prompt: "やって"}}},
		{ID: "a1", Parent: "u1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "できた"}}},
	})
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "8f3a2b1c-4d5e", CWD: "/repo", Title: "t"}
	m := Model{width: 120, height: 20, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	m = u.(Model)

	u, _ = m.Update(keyRunes("x"))
	m = u.(Model)
	if !strings.HasPrefix(m.flash, "Exported 1 turn to ./claude-8f3a2b1c-") {
		t.Fatalf("flash=%q want an export notice naming the file", m.flash)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory holds %d files (%v), want the single export", len(entries), err)
	}
	body, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := transcript.Markdown(s, c, transcript.Turns(c, c.ActivePath()), transcript.Options{})
	if string(body) != doc {
		t.Fatalf("the file does not hold the rendered document:\n%s", body)
	}
	if !strings.Contains(string(body), "**ASSISTANT**\n\nできた") {
		t.Fatalf("exported file is missing the reply:\n%s", body)
	}
}
