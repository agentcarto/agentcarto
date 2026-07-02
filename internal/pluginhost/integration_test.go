package pluginhost

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agentcarto/core/plugin"
	"gopkg.in/yaml.v3"
)

// TestSubprocessClaudeRoundTrip launches the real claude plugin binary as a subprocess and
// drives Init→Scan→LoadConversation over RPC, confirming the subprocess path (descriptor
// retrieval, incremental scan, conversation loading) works end to end. It skips when the
// binary isn't present in bin/ (build it with `make plugins`).
func TestSubprocessClaudeRoundTrip(t *testing.T) {
	bin := pluginBinary(t, "agentcarto-plugin-claude")
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(proj, "s.jsonl")
	if err := os.WriteFile(src, []byte(`{"uuid":"u1","cwd":"/work","timestamp":"2026-06-23T00:00:00Z","message":{"role":"user","content":"real question"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	l, err := plugin.Launch(bin)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer l.Kill()

	var opts yaml.Node
	if err := opts.Encode(map[string]string{"projects_dir": proj}); err != nil {
		t.Fatal(err)
	}
	desc, err := l.API.Init(context.Background(), "claude", &opts)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if desc.Type != "claude" || !desc.Capabilities.Scan {
		t.Fatalf("unexpected descriptor: %+v", desc)
	}

	out, err := l.API.Scan(context.Background(), plugin.ScanInput{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(out.Sessions) != 1 {
		t.Fatalf("sessions=%d want 1", len(out.Sessions))
	}

	conv, err := l.API.LoadConversation(context.Background(), out.Sessions[0].SourceRef)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if conv == nil || len(conv.Nodes) == 0 {
		t.Fatalf("empty conversation over RPC: %+v", conv)
	}
}

func pluginBinary(t *testing.T, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	bin := filepath.Join(filepath.Dir(file), "..", "..", "bin", name)
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("plugin binary not built: %s (run `make plugins`)", bin)
	}
	return bin
}
