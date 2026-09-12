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

// MinSchemaVersion is the oldest schema_version this build still accepts.
// Manifests below this value are rejected by rule #3.
const MinSchemaVersion = 1

// CurrentSchemaVersion is the newest schema_version this build understands.
// checkSchemaVersion accepts the closed range [MinSchemaVersion, CurrentSchemaVersion].
const CurrentSchemaVersion = 2

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

	// Actions declares one or more executable units per plugin (schema v2+).
	// For v1 manifests, normalize() synthesizes a single default action from
	// the top-level Install.Source.Entrypoint / On / Popup so downstream code
	// can uniformly consume Actions regardless of the source schema version.
	Actions []Action `yaml:"actions,omitempty"`
}

// Action is a single executable unit within a plugin. A plugin declares one
// or more actions; the first entry acts as the implicit default when the
// caller does not specify an action id.
type Action struct {
	ID         string       `yaml:"id"`
	Label      string       `yaml:"label,omitempty"`
	Entrypoint string       `yaml:"entrypoint"`
	On         []string     `yaml:"on,omitempty"`
	Popup      *PopupConfig `yaml:"popup,omitempty"`
	// Listener declares this action as an event-only endpoint: it still fires
	// on matching On events, but is excluded from user-facing surfaces
	// (palette, help popup, keybindings config, `jin plugin run` completion).
	// Direct invocation via `jin plugin run <plugin> <action>` remains
	// possible for debugging. Listener actions must declare a non-empty On.
	Listener bool `yaml:"listener,omitempty"`
	// Handoff marks an action as a synchronous, structured PR-handoff
	// provider. Handoff actions receive the bounded handoff request on stdin
	// and must return one JSON result on stdout. They are not ordinary palette
	// actions and cannot subscribe to status events.
	Handoff bool `yaml:"handoff,omitempty"`
	// MergeHandoff marks the separate two-phase merge provider endpoint. Core
	// invokes it first for read-only preflight and only later for an explicitly
	// confirmed merge operation.
	MergeHandoff bool `yaml:"merge_handoff,omitempty"`
}

// Install carries either a source build recipe or a release asset pattern.
// They are mutually exclusive; the XOR is enforced by rule #8.
type Install struct {
	Source       *SourceInstall       `yaml:"source,omitempty"`
	ReleaseAsset *ReleaseAssetInstall `yaml:"release_asset,omitempty"`
}

// SourceInstall builds the plugin from source. Build is optional — plugins
// that ship a directly-executable entrypoint (shell scripts, prebuilt bins
// committed to the repo) omit it entirely. Each build entry runs as its own
// process (no shell piping); Entrypoint must exist after any build steps
// complete and is what the runtime dispatcher executes on each event.
type SourceInstall struct {
	Build      []string `yaml:"build,omitempty"`
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
// string (e.g. "30s") into a time.Duration. The type alias in aux breaks
// method resolution so yaml does not recurse back into this method; every
// other field decodes through the alias's inline embedding.
func (m *Manifest) UnmarshalYAML(value *yaml.Node) error {
	type manifestFields Manifest
	aux := struct {
		*manifestFields `yaml:",inline"`
		Timeout         string `yaml:"timeout"`
	}{manifestFields: (*manifestFields)(m)}
	if err := value.Decode(&aux); err != nil {
		return err
	}
	if aux.Timeout == "" {
		return nil
	}
	d, err := time.ParseDuration(aux.Timeout)
	if err != nil {
		return fmt.Errorf("parse timeout %q: %w", aux.Timeout, err)
	}
	m.Timeout = d
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

// Entrypoint returns the runtime-executable path of the default action.
// For v1 manifests this resolves via normalize() to Install.Source.Entrypoint;
// for v2 it is Actions[0].Entrypoint. Returns "" when no default action
// exists (release_asset installs, or v1 manifests missing an entrypoint —
// the latter is rejected by checks.go).
func (m *Manifest) Entrypoint() string {
	if a := m.DefaultAction(); a != nil {
		return a.Entrypoint
	}
	return ""
}

// DefaultAction returns Actions[0] or nil when Actions is empty. Callers
// running the plugin without an explicit action id use this.
func (m *Manifest) DefaultAction() *Action {
	if len(m.Actions) == 0 {
		return nil
	}
	return &m.Actions[0]
}

// FindAction returns the action whose ID matches id, or nil when not found.
// An empty id is treated as no-match; callers wanting the default should
// use DefaultAction instead.
func (m *Manifest) FindAction(id string) *Action {
	if id == "" {
		return nil
	}
	for i := range m.Actions {
		if m.Actions[i].ID == id {
			return &m.Actions[i]
		}
	}
	return nil
}

// ActionIDs returns action IDs in declaration order. Useful for completion
// and for error messages listing available actions.
func (m *Manifest) ActionIDs() []string {
	ids := make([]string, 0, len(m.Actions))
	for _, a := range m.Actions {
		ids = append(ids, a.ID)
	}
	return ids
}

// normalize synthesizes Actions[] for v1 manifests so downstream helpers
// (DefaultAction / FindAction / Entrypoint) can uniformly read Actions[]
// regardless of the source schema version. The original top-level
// Install.Source.Entrypoint / On / Popup are intentionally left populated:
// validation (checks.go) runs after normalize and reports v1 findings
// against the fields the author actually wrote, so clearing them here would
// misfire the required-field, matcher, and popup rules. Runtime consumers
// read Actions[] only.
//
// No-op when the manifest is v2 (author supplied Actions explicitly), when
// Actions is already non-empty, or when the v1 manifest lacks an entrypoint
// (checks.go rejects the latter via rule #4).
func (m *Manifest) normalize() {
	if m.SchemaVersion >= 2 || len(m.Actions) > 0 {
		return
	}
	if m.Install.Source == nil || m.Install.Source.Entrypoint == "" {
		return
	}
	m.Actions = []Action{{
		ID:         "default",
		Label:      m.Name,
		Entrypoint: m.Install.Source.Entrypoint,
		On:         m.On,
		Popup:      m.Popup,
	}}
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
//
// The bytes are parsed once: root.Decode drives Manifest.UnmarshalYAML, and
// unknownFieldsFromNode walks the same yaml.Node to collect field keys the
// current schema does not recognise.
func Parse(data []byte) (*Manifest, []string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return &Manifest{}, nil, nil
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("invalid YAML: root is not a mapping")
	}

	var m Manifest
	if err := top.Decode(&m); err != nil {
		return nil, nil, fmt.Errorf("invalid YAML: %w", err)
	}
	m.normalize()
	return &m, unknownFieldsFromNode(top), nil
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
	"actions":        {},
}

var knownInstall = map[string]struct{}{
	"source":        {},
	"release_asset": {},
}

var knownAction = map[string]struct{}{
	"id":            {},
	"label":         {},
	"entrypoint":    {},
	"on":            {},
	"popup":         {},
	"listener":      {},
	"handoff":       {},
	"merge_handoff": {},
}

// unknownFieldsFromNode walks the top-level mapping and its nested `install`
// and `actions` structures once, collecting any keys the current schema does
// not recognise. The order of returned entries follows the mapping's own
// key order, so diagnostics are deterministic per input.
func unknownFieldsFromNode(mapping *yaml.Node) []string {
	var out []string
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i].Value
		val := mapping.Content[i+1]
		if _, ok := knownTopLevel[key]; !ok {
			out = append(out, key)
			continue
		}
		switch key {
		case "install":
			if val.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(val.Content); j += 2 {
				sub := val.Content[j].Value
				if _, ok := knownInstall[sub]; !ok {
					out = append(out, "install."+sub)
				}
			}
		case "actions":
			out = append(out, actionsUnknownFields(val)...)
		}
	}
	return out
}

// actionsUnknownFields collects unknown keys from every mapping element under
// an `actions:` sequence. It walks each element in a single pass, finding the
// action's `id` (if present) and unknown keys together so downstream diagnostics
// can be tagged with `actions[<id>].<key>` (or `actions[].<key>` when id is
// absent). A non-mapping sequence element is silently skipped — schema errors
// there surface via yaml.Decode.
func actionsUnknownFields(seq *yaml.Node) []string {
	if seq.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, elem := range seq.Content {
		if elem.Kind != yaml.MappingNode {
			continue
		}
		var actionID string
		var unknownKeys []string
		for j := 0; j+1 < len(elem.Content); j += 2 {
			key := elem.Content[j].Value
			if key == "id" {
				actionID = elem.Content[j+1].Value
				continue
			}
			if _, ok := knownAction[key]; !ok {
				unknownKeys = append(unknownKeys, key)
			}
		}
		prefix := "actions[]."
		if actionID != "" {
			prefix = "actions[" + actionID + "]."
		}
		for _, k := range unknownKeys {
			out = append(out, prefix+k)
		}
	}
	return out
}
