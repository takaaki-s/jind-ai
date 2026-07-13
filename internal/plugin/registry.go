package plugin

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// PluginState classifies an installed plugin at dispatch time: whether it will
// run, and if not, why. It is recomputed on every Load so that linked-plugin
// edits and config changes take effect without restarting the daemon.
type PluginState int

const (
	// StateEnabled means the manifest loads, its jin compat range is
	// satisfied, and config permits it — the plugin will receive events.
	StateEnabled PluginState = iota
	// StateDisabled means config turned the plugin off (plugins.enabled=false or
	// the name appears in plugins.disabled). The plugin is otherwise healthy.
	StateDisabled
	// StateIncompatible means the manifest loads but the plugin's jin compat
	// range does not include this jin build.
	StateIncompatible
	// StateBroken means the manifest is missing or fails to load/validate — most
	// often the plugin directory is gone.
	StateBroken
)

// String returns the lowercase label used in `jin plugin list` output and logs.
func (s PluginState) String() string {
	switch s {
	case StateEnabled:
		return "enabled"
	case StateDisabled:
		return "disabled"
	case StateIncompatible:
		return "incompatible"
	case StateBroken:
		return "broken"
	default:
		return fmt.Sprintf("PluginState(%d)", int(s))
	}
}

// Entry pairs one lock record with the current on-disk state of its plugin.
// Manifest is nil when State is StateBroken; Err carries the reason a plugin is
// StateBroken or StateIncompatible and is nil otherwise.
type Entry struct {
	Name     string
	Manifest *manifest.Manifest
	Lock     LockEntry
	State    PluginState
	Err      error
}

// Registry resolves installed plugins by reconciling the lock file, each
// plugin's on-disk manifest, and the user's plugins config. It caches nothing:
// every Load re-reads from disk so linked-plugin edits are picked up
// immediately. Plugin counts are tiny, so the repeated reads cost nothing.
type Registry struct {
	pluginsDir string
	stateDir   string
	cfg        config.PluginsConfig
}

// NewRegistry returns a Registry that reads plugins from pluginsDir, the lock
// file from stateDir, and applies cfg when deciding which plugins are enabled.
func NewRegistry(pluginsDir, stateDir string, cfg config.PluginsConfig) *Registry {
	return &Registry{pluginsDir: pluginsDir, stateDir: stateDir, cfg: cfg}
}

// Load reads the lock file and, for each locked plugin, loads and classifies
// the plugin at <pluginsDir>/<name>. Entries are returned sorted by name.
//
// Only a failure to read the lock file itself is returned as an error; an
// individual broken plugin surfaces as an Entry with State StateBroken so that
// one bad plugin never hides the healthy ones. Plugin directories that are not
// in the lock are ignored — installation always writes the lock, so an unlocked
// directory was never installed through the supported path.
func (r *Registry) Load() ([]Entry, error) {
	lock, err := LoadLock(r.stateDir)
	if err != nil {
		return nil, err
	}

	locked := lock.All()
	entries := make([]Entry, 0, len(locked))
	for name, le := range locked {
		entries = append(entries, r.classify(name, le))
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		return strings.Compare(a.Name, b.Name)
	})
	return entries, nil
}

// Runnable returns only the entries in StateEnabled — the plugins an event will
// actually reach. It is a thin filter over Load.
func (r *Registry) Runnable() ([]Entry, error) {
	entries, err := r.Load()
	if err != nil {
		return nil, err
	}
	runnable := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.State == StateEnabled {
			runnable = append(runnable, e)
		}
	}
	return runnable, nil
}

// classify resolves a single locked plugin into an Entry. Check order matters:
// a missing/broken manifest is reported before compatibility, and compatibility
// before config, so the most fundamental problem wins.
func (r *Registry) classify(name string, le LockEntry) Entry {
	e := Entry{Name: name, Lock: le}

	m, err := loadManifest(filepath.Join(r.pluginsDir, name))
	if err != nil {
		e.State = StateBroken
		e.Err = err
		return e
	}
	e.Manifest = m

	if err := checkJinCompat(m); err != nil {
		e.State = StateIncompatible
		e.Err = err
		return e
	}

	if r.disabled(name) {
		e.State = StateDisabled
		return e
	}

	e.State = StateEnabled
	return e
}

// disabled reports whether config turns this plugin off, either globally
// (plugins.enabled=false) or by naming it in plugins.disabled.
func (r *Registry) disabled(name string) bool {
	if r.cfg.Enabled != nil && !*r.cfg.Enabled {
		return true
	}
	return slices.Contains(r.cfg.Disabled, name)
}
