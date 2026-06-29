package pluginhost

import (
	"path/filepath"
	"testing"

	"github.com/agentcarto/agentcarto/internal/config"
)

// A plugin that can't start (binary missing) must not fail the whole launch; it is recorded
// in Warnings and skipped (graceful degradation).
func TestLaunchSkipsUnavailablePlugin(t *testing.T) {
	c := config.Config{Plugins: []config.Plugin{
		{ID: "ghost", Type: "ghost", Enabled: true, Command: filepath.Join(t.TempDir(), "does-not-exist")},
		{ID: "disabled", Type: "disabled", Enabled: false},
	}}
	h, err := Launch(c)
	if err != nil {
		t.Fatalf("Launch should not error on a missing plugin: %v", err)
	}
	defer h.Close()
	if len(h.Instances) != 0 {
		t.Fatalf("instances=%d want 0", len(h.Instances))
	}
	if len(h.Warnings) != 1 {
		t.Fatalf("warnings=%d want 1 (skipped ghost): %v", len(h.Warnings), h.Warnings)
	}
}
