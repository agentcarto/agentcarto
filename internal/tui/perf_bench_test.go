package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/agentcarto/core/domain"
	tea "github.com/charmbracelet/bubbletea"
)

// bigConv builds a synthetic conversation with `turns` turns of `perTurn` nodes
// each, plus an abandoned rewind branch every `branchEvery` turns.
func bigConv(turns, perTurn, branchEvery int) domain.Conversation {
	base := time.Date(2026, 6, 23, 1, 0, 0, 0, time.Local)
	var nodes []domain.ConvNode
	parent := ""
	for t := 0; t < turns; t++ {
		for j := 0; j < perTurn; j++ {
			id := fmt.Sprintf("n%d-%d", t, j)
			ev := domain.Event{Kind: domain.EventAssistant, Text: "some assistant reply text that is reasonably long", Timestamp: base.Add(time.Duration(t*perTurn+j) * time.Second)}
			if j == 0 {
				ev = domain.Event{Kind: domain.EventUser, Text: "prompt", Prompt: fmt.Sprintf("prompt for turn %d", t), Timestamp: base.Add(time.Duration(t*perTurn) * time.Second)}
			} else if j%3 == 0 {
				ev = domain.Event{Kind: domain.EventToolCall, ToolName: "Bash", ToolArg: "ls -la", Timestamp: base.Add(time.Duration(t*perTurn+j) * time.Second)}
			}
			nodes = append(nodes, domain.ConvNode{ID: id, Parent: parent, Timestamp: ev.Timestamp, Events: []domain.Event{ev}})
			parent = id
		}
		if branchEvery > 0 && t%branchEvery == 0 {
			// An abandoned branch hanging off this turn's first node: 40 nodes deep.
			bp := fmt.Sprintf("n%d-0", t)
			for k := 0; k < 40; k++ {
				id := fmt.Sprintf("b%d-%d", t, k)
				nodes = append(nodes, domain.ConvNode{ID: id, Parent: bp, Timestamp: base.Add(time.Duration(t*perTurn) * time.Second), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "abandoned"}}})
				bp = id
			}
		}
	}
	return domain.NewConversation(nodes)
}

func benchModel(b *testing.B, turns int) Model {
	b.Helper()
	return benchModelConv(b, bigConv(turns, 10, 5))
}

func benchModelConv(b *testing.B, c domain.Conversation) Model {
	b.Helper()
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s", CWD: "/repo", Title: "t"}
	m := Model{width: 160, height: 50, detailSession: &s}
	u, _ := m.Update(convMsg{c: &c, reset: true})
	return u.(Model)
}

func BenchmarkDetailView100(b *testing.B) {
	m := benchModel(b, 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.detailView()
	}
}

func BenchmarkDetailView500(b *testing.B) {
	m := benchModel(b, 500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.detailView()
	}
}

// 2800 turns of 10 nodes is the size of the largest real session on disk
// (~28k JSONL lines).
func BenchmarkDetailView2800(b *testing.B) {
	m := benchModel(b, 2800)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.detailView()
	}
}

func BenchmarkDetailViewSearching2800(b *testing.B) {
	m := benchModel(b, 2800)
	m.turnQuery = "prompt for turn 42"
	(&m).syncTurnHits()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.detailView()
	}
}

// BenchmarkDetailScroll2800 is one cursor move as the user sees it: the update
// plus the render it causes.
func BenchmarkDetailScroll2800(b *testing.B) {
	m := benchModel(b, 2800)
	key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		u, _ := m.Update(key)
		m = u.(Model)
		_ = m.View()
	}
}

func BenchmarkDetailViewSearching500(b *testing.B) {
	m := benchModel(b, 500)
	m.turnQuery = "prompt for turn 42"
	(&m).syncTurnHits()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.detailView()
	}
}

func BenchmarkRebuildDetailRows500(b *testing.B) {
	m := benchModel(b, 500)
	path := m.detail.ActivePath()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.rebuildDetailRows(path)
	}
}

func BenchmarkConvMsgLoad500(b *testing.B) {
	c := bigConv(500, 10, 5)
	s := domain.Session{PluginID: "claude", AgentType: "claude", SessionID: "s", CWD: "/repo", Title: "t"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := Model{width: 160, height: 50, detailSession: &s}
		_, _ = m.Update(convMsg{c: &c, reset: true})
	}
}

// deepBranchConv is a session with one long abandoned rewind: the branch rows
// the detail view shows report the size of the branch behind them.
func deepBranchConv(turns, branchLen int) domain.Conversation {
	base := time.Date(2026, 6, 23, 1, 0, 0, 0, time.Local)
	var nodes []domain.ConvNode
	parent := ""
	for t := 0; t < turns; t++ {
		id := fmt.Sprintf("n%d", t)
		nodes = append(nodes, domain.ConvNode{ID: id, Parent: parent, Timestamp: base.Add(time.Duration(t) * time.Minute), Events: []domain.Event{{Kind: domain.EventUser, Prompt: fmt.Sprintf("turn %d", t), Timestamp: base.Add(time.Duration(t) * time.Minute)}}})
		parent = id
	}
	bp := "n1"
	for k := 0; k < branchLen; k++ {
		id := fmt.Sprintf("b%d", k)
		nodes = append(nodes, domain.ConvNode{ID: id, Parent: bp, Timestamp: base.Add(time.Duration(k) * time.Second), Events: []domain.Event{{Kind: domain.EventUser, Prompt: "abandoned"}}})
		bp = id
	}
	return domain.NewConversation(nodes)
}

func BenchmarkDetailViewDeepBranch(b *testing.B) {
	m := benchModelConv(b, deepBranchConv(60, 5000))
	m.detailCursor = len(m.detailRows) - 1
	(&m).ensureDetailOffset()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.detailView()
	}
}
