package config

import (
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c, e := Load("")
	if e != nil {
		t.Fatal(e)
	}
	if c.Version != 1 || len(c.Plugins) != 5 {
		t.Fatalf("unexpected defaults: %#v", c)
	}
	if time.Duration(c.UI.RefreshInterval) != 2*time.Second {
		t.Fatal(c.UI.RefreshInterval)
	}
	if int64(c.Cache.MaxSize) != 512<<20 {
		t.Fatal(c.Cache.MaxSize)
	}
}
func TestUnknownFieldHasPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.yaml")
	if e := os.WriteFile(p, []byte("version: 1\nunknown: true\n"), 0600); e != nil {
		t.Fatal(e)
	}
	_, e := Load(p)
	if e == nil || !strings.Contains(e.Error(), "field unknown not found") {
		t.Fatalf("unexpected error: %v", e)
	}
}
func TestUndefinedEnvironment(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.yaml")
	data := `version: 1
plugins:
  - id: x
    type: claude
    enabled: true
    color: cyan
    options: {projects_dir: "${AGENTCARTO_TEST_UNDEFINED}"}
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	_, e := Load(p)
	if e == nil || !strings.Contains(e.Error(), "undefined environment variable") {
		t.Fatalf("unexpected error: %v", e)
	}
}
func TestPartialCacheOverridePreservesDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(p, []byte("cache:\n  enabled: false\n"), 0600)
	c, e := Load(p)
	if e != nil {
		t.Fatal(e)
	}
	if c.Cache.Enabled {
		t.Fatal("enabled should be false")
	}
	if c.Cache.MaxSize == 0 || c.Cache.MaxAge == 0 {
		t.Fatal("defaults were lost")
	}
}
func TestPluginOverrideMergesByID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	data := `plugins:
  - id: claude
    options:
      executable: claude-custom
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := Load(p)
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Plugins) != 5 {
		t.Fatalf("plugin defaults should be preserved, got %d: %#v", len(c.Plugins), c.Plugins)
	}
	seen := map[string]bool{}
	for _, p := range c.Plugins {
		seen[p.ID] = true
		if p.ID == "claude" {
			if p.Type != "claude" || !p.Enabled || p.Color != "cyan" {
				t.Fatalf("claude defaults were lost: %#v", p)
			}
			if got := nodeScalar(p.Options, "executable"); got != "claude-custom" {
				t.Fatalf("claude executable override lost: %q", got)
			}
		}
	}
	for _, id := range []string{"claude", "codex", "grok", "copilot-vc", "copilot-jb"} {
		if !seen[id] {
			t.Fatalf("missing plugin %q after merge", id)
		}
	}
}

func nodeScalar(n yaml.Node, key string) string {
	if n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1].Value
		}
	}
	return ""
}
