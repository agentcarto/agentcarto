package app

import (
	"github.com/agentcarto/agentcarto/internal/catalog"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"testing"
)

func TestMatchesExecNames(t *testing.T) {
	names := map[string]bool{"claude": true, "codex": true, "grok": true}
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"claude", nil, true},                                     // executable name matches
		{"/usr/local/bin/codex", nil, true},                       // matches by basename even with a path
		{"node", []string{"node", "/opt/claude/claude.js"}, true}, // picks up a node-launched .js by stripping the extension
		{"grok", []string{"--resume", "abc"}, true},
		{"firefox", []string{"firefox", "https://x"}, false}, // unrelated
		{"node", []string{"node", "server.js"}, false},       // a .js that is not a candidate name
		{"", nil, false},
	}
	for _, c := range cases {
		if got := matchesExecNames(names, c.name, c.args); got != c.want {
			t.Errorf("matchesExecNames(%q, %v)=%v want %v", c.name, c.args, got, c.want)
		}
	}
}

type execStub struct {
	activeStub
	exe string
}

func (s *execStub) Executable() string { return s.exe }

func TestProcessEnrichCollectsActivePluginExecutables(t *testing.T) {
	a := &App{Catalog: catalog.Catalog{Plugins: []plugin.Instance{
		{ID: "claude", Descriptor: plugin.Descriptor{Executable: "/usr/bin/claude", Capabilities: domain.Capabilities{Active: true}}, Impl: &execStub{exe: "/usr/bin/claude"}},
		// plugins with Active=false are not candidates
		{ID: "copilot", Descriptor: plugin.Descriptor{Executable: "code", Capabilities: domain.Capabilities{Active: false}}, Impl: &execStub{exe: "code"}},
	}}}
	enrich := a.processEnrich()
	if enrich == nil {
		t.Fatal("expected non-nil enrich when an active plugin provides an executable")
	}
	if !enrich("claude", nil) {
		t.Error("claude process should be enriched")
	}
	if enrich("code", nil) {
		t.Error("inactive plugin executable should not be a candidate")
	}
}

func TestProcessEnrichNilWhenNoCandidates(t *testing.T) {
	a := &App{Catalog: catalog.Catalog{Plugins: []plugin.Instance{
		{ID: "copilot", Descriptor: plugin.Descriptor{Capabilities: domain.Capabilities{Active: false}}, Impl: &execStub{exe: "code"}},
	}}}
	if a.processEnrich() != nil {
		t.Fatal("expected nil enrich (fall back to enriching all) when no active candidates")
	}
}
