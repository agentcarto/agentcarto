package app

import (
	"context"
	"github.com/agentcarto/agentcarto/internal/catalog"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"testing"
	"time"
)

type activeStub struct {
	status domain.Status
}

func (s *activeStub) DetectActive(_ context.Context, sessions []domain.Session, _ []domain.Process) ([]domain.Session, error) {
	for i := range sessions {
		sessions[i].Status = s.status
	}
	return sessions, nil
}

func TestDetectActiveDoesNotDebounceUnmatchedBlankStatus(t *testing.T) {
	stub := &activeStub{status: domain.StatusRunning}
	a := &App{
		Catalog: catalog.Catalog{Plugins: []plugin.Instance{{
			ID:         "p",
			Descriptor: plugin.Descriptor{Capabilities: domain.Capabilities{Active: true}},
			Impl:       stub,
		}}},
		lastRunning: map[domain.SessionKey]time.Time{},
	}
	sessions := []domain.Session{{PluginID: "p", SessionID: "s"}}
	out, err := a.DetectActive(context.Background(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Status != domain.StatusRunning {
		t.Fatalf("first status=%q", out[0].Status)
	}
	stub.status = ""
	out, err = a.DetectActive(context.Background(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Status != "" {
		t.Fatalf("unmatched blank status should not be held as running: %#v", out[0])
	}
}
