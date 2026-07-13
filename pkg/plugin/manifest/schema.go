// Package manifest is the single source of truth for jind-ai plugin
// manifests as they appear both at runtime (the dispatcher reads them) and in
// the plugin registry ecosystem (the crawler and the CLI's install command
// read them). Every consumer — jin binary, registry crawler, external tooling
// — imports this package to parse and validate jind-ai-plugin.yaml so that
// author-local, PR CI, and crawler results are bit-for-bit identical.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Filename is the fixed name of the plugin manifest, read from the plugin
// directory root (either the source repo or the installed plugin directory).
const Filename = "jind-ai-plugin.yaml"

// CurrentSchemaVersion is the newest schema_version this build understands.
// MinSchemaVersion is the oldest it still accepts (validator supports N-2
// generations per docs/plugin-registry.md).
const (
	CurrentSchemaVersion = 1
	MinSchemaVersion     = 1
)

// DefaultTimeout is applied to a plugin run when the manifest omits timeout.
const DefaultTimeout = 30 * time.Second

// Manifest is the parsed jind-ai-plugin.yaml. It carries both publish-time
// fields (name, version, install, jin compat) and runtime dispatch fields
// (on, timeout, popup). Consumers pick the subset they care about.
type Manifest struct {
	SchemaVersion int     `yaml:"schema_version"`
	Name          string  `yaml:"name"`
	Version       string  `yaml:"version"`
	Description   string  `yaml:"description"`
	License       string  `yaml:"license,omitempty"`
	Homepage      string  `yaml:"homepage,omitempty"`
	Jin           string  `yaml:"jin"`
	Install       Install `yaml:"install"`

	// Runtime dispatch fields (consumed by internal/plugin).
	On      []string      `yaml:"on,omitempty"`
	Timeout time.Duration `yaml:"-"`
	Popup   *PopupConfig  `yaml:"popup,omitempty"`
}

// Install carries either a source build recipe or a release asset pattern.
// They are mutually exclusive; the XOR is enforced by rule #8.
type Install struct {
	Source       *SourceInstall       `yaml:"source,omitempty"`
	ReleaseAsset *ReleaseAssetInstall `yaml:"release_asset,omitempty"`
}

// SourceInstall builds the plugin from source. Each build entry runs as its
// own process (no shell piping); Entrypoint must exist after the build and
// is what the runtime dispatcher executes on each event.
type SourceInstall struct {
	Build      []string `yaml:"build"`
	Entrypoint string   `yaml:"entrypoint"`
}

// ReleaseAssetInstall fetches a pre-built binary from a GitHub Release.
// Pattern supports {os} and {arch} placeholders.
type ReleaseAssetInstall struct {
	Pattern string `yaml:"pattern"`
}

// PopupConfig declares a plugin's preferred popup size as a percentage of the
// terminal (1-100). A zero field means "unset" — the resolver falls back to
// user config or the hardcoded plugin default.
type PopupConfig struct {
	Width  int `yaml:"width,omitempty"`
	Height int `yaml:"height,omitempty"`
}

// UnmarshalYAML decodes the manifest, translating the human-friendly timeout
// string (e.g. "30s") into a time.Duration. A shadow struct avoids recursing
// into this method and lets Timeout arrive as a string.
func (m *Manifest) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		SchemaVersion int          `yaml:"schema_version"`
		Name          string       `yaml:"name"`
		Version       string       `yaml:"version"`
		Description   string       `yaml:"description"`
		License       string       `yaml:"license"`
		Homepage      string       `yaml:"homepage"`
		Jin           string       `yaml:"jin"`
		Install       Install      `yaml:"install"`
		On            []string     `yaml:"on"`
		Timeout       string       `yaml:"timeout"`
		Popup         *PopupConfig `yaml:"popup"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}

	m.SchemaVersion = raw.SchemaVersion
	m.Name = raw.Name
	m.Version = raw.Version
	m.Description = raw.Description
	m.License = raw.License
	m.Homepage = raw.Homepage
	m.Jin = raw.Jin
	m.Install = raw.Install
	m.On = raw.On
	m.Popup = raw.Popup
	if raw.Timeout != "" {
		d, err := time.ParseDuration(raw.Timeout)
		if err != nil {
			return fmt.Errorf("parse timeout %q: %w", raw.Timeout, err)
		}
		m.Timeout = d
	}
	return nil
}

// EffectiveTimeout returns the run timeout, substituting DefaultTimeout when
// the manifest left it unset (or non-positive).
func (m *Manifest) EffectiveTimeout() time.Duration {
	if m.Timeout <= 0 {
		return DefaultTimeout
	}
	return m.Timeout
}

// Entrypoint returns the runtime-executable path declared by install.source,
// or "" for release_asset installs (whose runtime path is decided at fetch
// time by the installer). Callers use this in place of the historical `run`
// field on the runtime manifest.
func (m *Manifest) Entrypoint() string {
	if m.Install.Source == nil {
		return ""
	}
	return m.Install.Source.Entrypoint
}

// BuildCommands returns the ordered list of build commands for install.source,
// or nil for a release_asset install. Each element is a self-contained command
// (no shell piping across elements); the installer runs them in order.
func (m *Manifest) BuildCommands() []string {
	if m.Install.Source == nil {
		return nil
	}
	return m.Install.Source.Build
}

// Parse decodes YAML bytes into a Manifest and returns any unknown top-level
// or install-level fields for forward-compatible WARN reporting. A YAML
// syntax error is returned as err; on success err is nil even if the manifest
// is missing required fields (that is checks.go's job).
func Parse(data []byte) (*Manifest, []string, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("invalid YAML: %w", err)
	}

	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, nil, fmt.Errorf("invalid YAML: %w", err)
	}

	return &m, unknownFields(raw), nil
}

// LoadFile reads Filename at pluginDir and delegates to Parse. Returns a
// wrapped os error if the file is missing so callers can distinguish
// "no manifest" (rule #1) from "bad YAML" (rule #2).
func LoadFile(pluginDir string) (*Manifest, []string, error) {
	path := filepath.Join(pluginDir, Filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return Parse(data)
}

var knownTopLevel = map[string]struct{}{
	"schema_version": {},
	"name":           {},
	"version":        {},
	"description":    {},
	"license":        {},
	"homepage":       {},
	"jin":            {},
	"install":        {},
	"on":             {},
	"timeout":        {},
	"popup":          {},
}

var knownInstall = map[string]struct{}{
	"source":        {},
	"release_asset": {},
}

func unknownFields(raw map[string]any) []string {
	var out []string
	for k := range raw {
		if _, ok := knownTopLevel[k]; !ok {
			out = append(out, k)
		}
	}
	if inst, ok := raw["install"].(map[string]any); ok {
		for k := range inst {
			if _, ok := knownInstall[k]; !ok {
				out = append(out, "install."+k)
			}
		}
	}
	return out
}
