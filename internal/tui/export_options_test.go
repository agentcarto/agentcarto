package tui

import (
	"strings"
	"testing"

	"github.com/agentcarto/agentcarto/internal/transcript"
	"github.com/agentcarto/core/domain"
)

// The export the "x" key writes keeps a subagent's report out: it is a document
// to read, and the report is what the CLI's --tools full is for.
func TestExportKeepsSubagentReportsOut(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{{ID: "u1", Events: []domain.Event{
		{Kind: domain.EventUser, Text: "調べて", Prompt: "調べて"},
		{Kind: domain.EventTask, ToolArg: "explore [done]", ToolDetail: "長い報告の本文"},
		{Kind: domain.EventAssistant, Text: "分かった"},
	}}})
	s := domain.Session{AgentType: "claude", SessionID: "x", Title: "t"}
	doc, _ := transcript.Markdown(s, c, transcript.Turns(c, c.ActivePath()), exportOptions())
	if !strings.Contains(doc, "- TASK explore [done]") {
		t.Fatalf("the task line is missing:\n%s", doc)
	}
	if strings.Contains(doc, "長い報告の本文") || strings.Contains(doc, "TASK report") {
		t.Fatalf("the export carries a subagent report:\n%s", doc)
	}
}
