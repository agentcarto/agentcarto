package search

import (
	"fmt"
	"github.com/agentcarto/core/domain"
	"testing"
)

func BenchmarkMatch(b *testing.B) {
	idx := New(131072)
	s := domain.Session{PluginID: "codex", SessionID: "id", Title: "title", CWD: "/repo", SourceRef: domain.SessionRef{Source: "/repo/id"}}
	idx.Set(s, "needle"+fmt.Sprint(make([]byte, 100000)), 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = idx.Match(s, "needle")
	}
}
