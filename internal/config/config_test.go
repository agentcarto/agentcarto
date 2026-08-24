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

// Summaries cost money and send session text off the machine, so the shipped
// default generates none. An upgrade must not start spending on its own.
func TestSummaryIsOffByDefault(t *testing.T) {
	c, e := Load("")
	if e != nil {
		t.Fatal(e)
	}
	if c.Summary.Agent != "" {
		t.Errorf("the built-in default enables summaries with agent %q", c.Summary.Agent)
	}
	// The rest of the section still has usable values, so switching the agent on
	// is a one-line change.
	if c.Summary.Model == "" || c.Summary.Timeout <= 0 || c.Summary.MaxPerRun <= 0 {
		t.Errorf("the default summary section is not ready to be switched on: %+v", c.Summary)
	}
}

func TestValidateSummary(t *testing.T) {
	base, e := Load("")
	if e != nil {
		t.Fatal(e)
	}
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"off is valid", func(c *Config) { c.Summary.Agent = "" }, ""},
		{"claude", func(c *Config) { c.Summary.Agent = "claude" }, ""},
		{"codex", func(c *Config) { c.Summary.Agent = "codex" }, ""},
		{"unknown agent", func(c *Config) { c.Summary.Agent = "gemini" }, "summary.agent"},
		{"model required once on", func(c *Config) { c.Summary.Agent = "claude"; c.Summary.Model = "" }, "summary.model"},
		{"timeout must be positive", func(c *Config) { c.Summary.Agent = "claude"; c.Summary.Timeout = 0 }, "summary.timeout"},
		{"max_per_run must be positive", func(c *Config) { c.Summary.Agent = "claude"; c.Summary.MaxPerRun = 0 }, "summary.max_per_run"},
		// While off, the other fields are not checked: a half-filled section
		// that nobody uses is not an error.
		{"off ignores the rest", func(c *Config) { c.Summary = Summary{} }, ""},
	}
	for _, tc := range cases {
		c := base
		tc.mutate(&c)
		e := Validate(c)
		switch {
		case tc.wantErr == "" && e != nil:
			t.Errorf("%s: Validate=%v want nil", tc.name, e)
		case tc.wantErr != "" && e == nil:
			t.Errorf("%s: Validate=nil want an error naming %s", tc.name, tc.wantErr)
		case tc.wantErr != "" && e != nil && !strings.Contains(e.Error(), tc.wantErr):
			t.Errorf("%s: Validate=%v want it to name %s", tc.name, e, tc.wantErr)
		}
	}
}

// An unknown key is an error (KnownFields), so a typo in the section name is
// caught rather than silently leaving summaries off.
func TestSummaryTypoIsAnError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if e := os.WriteFile(p, []byte("summary: {agnet: claude}\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Load(p); e == nil {
		t.Error("a misspelled key inside summary was accepted")
	}
}
