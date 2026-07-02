package config

import (
	"bytes"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Duration time.Duration

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, e := time.ParseDuration(n.Value)
	if e != nil {
		return e
	}
	*d = Duration(v)
	return nil
}

type Bytes int64

type byteUnit struct {
	name string
	n    int64
}

// byteUnits lists size suffixes from largest to smallest. A bare "B" (1 byte)
// is the implicit fallback and is handled separately where it applies.
var byteUnits = []byteUnit{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}}

func (b Bytes) MarshalYAML() (any, error) {
	v := int64(b)
	for _, u := range byteUnits {
		if v%u.n == 0 {
			return fmt.Sprintf("%d%s", v/u.n, u.name), nil
		}
	}
	return fmt.Sprintf("%dB", v), nil
}

func (b *Bytes) UnmarshalYAML(n *yaml.Node) error {
	s := n.Value
	units := append(append([]byteUnit{}, byteUnits...), byteUnit{"B", 1})
	for _, u := range units {
		if strings.HasSuffix(s, u.name) {
			v, e := strconv.ParseInt(strings.TrimSuffix(s, u.name), 10, 64)
			if e != nil {
				return e
			}
			*b = Bytes(v * u.n)
			return nil
		}
	}
	return fmt.Errorf("invalid byte size %q", s)
}

type UI struct {
	RefreshInterval Duration `yaml:"refresh_interval"`
	RescanInterval  Duration `yaml:"rescan_interval"`
	DefaultView     string   `yaml:"default_view"`
}
type Index struct {
	MaxCharsPerSession int `yaml:"max_chars_per_session"`
}
type Cache struct {
	Enabled            bool     `yaml:"enabled"`
	CacheConversations bool     `yaml:"cache_conversations"`
	MaxSize            Bytes    `yaml:"max_size"`
	MaxAge             Duration `yaml:"max_age"`
}
type Plugin struct {
	ID      string `yaml:"id"`
	Type    string `yaml:"type"`
	Enabled bool   `yaml:"enabled"`
	Color   string `yaml:"color"`
	// Command is the path to the plugin executable, used to launch a subprocess
	// plugin. When omitted, the binary is looked up next to the main executable,
	// or as "agentcarto-plugin-<type>" on PATH.
	Command string    `yaml:"command,omitempty"`
	Options yaml.Node `yaml:"options"`
}
type Config struct {
	Version int      `yaml:"version"`
	UI      UI       `yaml:"ui"`
	Index   Index    `yaml:"index"`
	Cache   Cache    `yaml:"cache"`
	Plugins []Plugin `yaml:"plugins"`
}

const defaults = `version: 1
ui: {refresh_interval: 2s, rescan_interval: 3s, default_view: time}
index: {max_chars_per_session: 131072}
cache: {enabled: true, cache_conversations: true, max_size: 512MiB, max_age: 720h}
plugins:
  - {id: claude, type: claude, enabled: true, color: cyan, options: {}}
  - {id: codex, type: codex, enabled: true, color: red, options: {}}
  - {id: grok, type: grok, enabled: true, color: magenta, options: {}}
  - {id: copilot-vc, type: copilot-vc, enabled: true, color: orange, options: {}}
  - {id: copilot-jb, type: copilot-jb, enabled: true, color: yellow, options: {}}
`

func decode(data []byte, dst any) error {
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	return d.Decode(dst)
}
func Load(explicit string) (Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(defaults), &root); err != nil {
		return Config{}, err
	}
	for _, p := range configPaths(explicit) {
		data, e := os.ReadFile(p)
		if errors.Is(e, os.ErrNotExist) {
			if p == explicit && explicit != "" {
				return Config{}, e
			}
			continue
		}
		if e != nil {
			return Config{}, e
		}
		var over yaml.Node
		if e = yaml.Unmarshal(data, &over); e != nil {
			return Config{}, fmt.Errorf("%s: %w", p, e)
		}
		mergeNode(root.Content[0], over.Content[0])
	}
	b, e := yaml.Marshal(&root)
	if e != nil {
		return Config{}, e
	}
	var c Config
	if e = decode(b, &c); e != nil {
		return c, e
	}
	if e := expand(&c); e != nil {
		return c, e
	}
	return c, Validate(c)
}

// configPaths returns the config files to merge, in increasing priority order
// (later entries win): config.yaml next to the executable (a portable config
// shipped alongside the binary), the per-user config (UserPath), and finally
// the path given via --config. Files that do not exist are skipped by Load.
func configPaths(explicit string) []string {
	var paths []string
	if p := exeDirConfig(); p != "" {
		paths = append(paths, p)
	}
	paths = append(paths, UserPath())
	if explicit != "" {
		paths = append(paths, explicit)
	}
	return paths
}
func mergeNode(dst, src *yaml.Node) {
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		*dst = *src
		return
	}
	for i := 0; i < len(src.Content); i += 2 {
		k, v := src.Content[i], src.Content[i+1]
		found := -1
		for j := 0; j < len(dst.Content); j += 2 {
			if dst.Content[j].Value == k.Value {
				found = j
				break
			}
		}
		if found < 0 {
			dst.Content = append(dst.Content, k, v)
		} else if k.Value == "plugins" && dst.Content[found+1].Kind == yaml.SequenceNode && v.Kind == yaml.SequenceNode {
			mergePluginSeq(dst.Content[found+1], v)
		} else if dst.Content[found+1].Kind == yaml.MappingNode && v.Kind == yaml.MappingNode {
			mergeNode(dst.Content[found+1], v)
		} else {
			dst.Content[found+1] = v
		}
	}
}
func mergePluginSeq(dst, src *yaml.Node) {
	for _, p := range src.Content {
		id := mappingValue(p, "id")
		if id == "" {
			dst.Content = append(dst.Content, p)
			continue
		}
		found := -1
		for i, existing := range dst.Content {
			if mappingValue(existing, "id") == id {
				found = i
				break
			}
		}
		if found < 0 {
			dst.Content = append(dst.Content, p)
			continue
		}
		if dst.Content[found].Kind == yaml.MappingNode && p.Kind == yaml.MappingNode {
			mergeNode(dst.Content[found], p)
		} else {
			dst.Content[found] = p
		}
	}
}
func mappingValue(n *yaml.Node, key string) string {
	if n == nil || n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1].Value
		}
	}
	return ""
}
func Validate(c Config) error {
	if c.Version != 1 {
		return fmt.Errorf("version: unsupported value %d", c.Version)
	}
	if c.UI.DefaultView != "time" && c.UI.DefaultView != "folder" {
		return fmt.Errorf("ui.default_view: expected time or folder")
	}
	if c.Cache.MaxSize <= 0 {
		return fmt.Errorf("cache.max_size: must be positive")
	}
	if c.Cache.MaxAge <= 0 {
		return fmt.Errorf("cache.max_age: must be positive")
	}
	// A non-positive rescan interval would make the TUI's tick fire immediately
	// and continuously (a scan busy-loop).
	if c.UI.RescanInterval <= 0 {
		return fmt.Errorf("ui.rescan_interval: must be positive")
	}
	if c.UI.RefreshInterval <= 0 {
		return fmt.Errorf("ui.refresh_interval: must be positive")
	}
	colors := map[string]bool{"default": true, "black": true, "red": true, "green": true, "yellow": true, "blue": true, "magenta": true, "cyan": true, "white": true, "orange": true}
	ids := map[string]bool{}
	for i, p := range c.Plugins {
		if p.ID == "" {
			return fmt.Errorf("plugins[%d].id: required", i)
		}
		if ids[p.ID] {
			return fmt.Errorf("plugins[%d].id: duplicate %q", i, p.ID)
		}
		ids[p.ID] = true
		if !colors[p.Color] && !strings.HasPrefix(p.Color, "bright-") {
			return fmt.Errorf("plugins[%d].color: unsupported %q", i, p.Color)
		}
	}
	return nil
}

// exeDirConfig returns the path to config.yaml in the same directory as the
// executable, or "" if it does not exist. This lets portable distributions that
// keep the binary and its config together be picked up without passing --config.
func exeDirConfig() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	p := filepath.Join(filepath.Dir(exe), "config.yaml")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
func UserPath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "agentcarto", "config.yaml")
	case "windows":
		return filepath.Join(os.Getenv("AppData"), "agentcarto", "config.yaml")
	default:
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
		return filepath.Join(base, "agentcarto", "config.yaml")
	}
}
func expand(c *Config) error {
	home, _ := os.UserHomeDir()
	var walk func(*yaml.Node) error
	walk = func(n *yaml.Node) error {
		if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
			s := os.Expand(n.Value, func(k string) string {
				v, ok := os.LookupEnv(k)
				if !ok {
					return "\x00"
				}
				return v
			})
			if strings.ContainsRune(s, '\x00') {
				return fmt.Errorf("undefined environment variable in %q", n.Value)
			}
			if s == "~" || strings.HasPrefix(s, "~/") {
				s = filepath.Join(home, strings.TrimPrefix(s, "~/"))
			}
			n.Value = s
		}
		for _, x := range n.Content {
			if e := walk(x); e != nil {
				return e
			}
		}
		return nil
	}
	for i := range c.Plugins {
		if e := walk(&c.Plugins[i].Options); e != nil {
			return fmt.Errorf("plugins[%d].options: %w", i, e)
		}
	}
	return nil
}
