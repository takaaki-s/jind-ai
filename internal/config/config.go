package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// supportedDetachKeys is the list of supported detach keys
var supportedDetachKeys = []string{"ctrl+^", "ctrl+]", "ctrl+\\", "ctrl+g"}

// ValidateDetachKey checks whether the detach key is supported.
// Returns an error listing supported keys if the key is not supported.
func ValidateDetachKey(key string) error {
	if slices.Contains(supportedDetachKeys, key) {
		return nil
	}
	return fmt.Errorf("unsupported detach key %q: supported keys are %s",
		key, strings.Join(supportedDetachKeys, ", "))
}

// PluginActionKeybinding is the per-action subset of a plugin's keybindings.
// Kept as a struct (not a bare []string) so future per-binding options (e.g.
// whether to pass the cursor session, arg templates) can be added without a
// breaking schema flip.
type PluginActionKeybinding struct {
	// Keys is tmux bind-key notation, e.g. []string{"M-n"}. Must be
	// modifier-prefixed so bare-letter input in the display pane still
	// reaches the agent (same constraint as ActionPanel/TogglePane).
	// nil or empty ⇒ no binding for this action.
	Keys []string `mapstructure:"keys,omitempty"`
}

// PluginKeybindings groups one plugin's keybindings by action ID
// (YAML: keybindings.plugins.<name>.actions.<id>.keys). This replaced the
// pre-0.8 shape that put `keys` directly under the plugin name; the old
// shape is detected at load time and dropped with a warning (see
// dropDeprecatedPluginKeybindings).
type PluginKeybindings struct {
	// Actions maps action ID (as declared in the plugin manifest; "default"
	// for v1 single-action plugins) to its keybinding. Absent / empty map
	// ⇒ no bindings for this plugin.
	Actions map[string]PluginActionKeybinding `mapstructure:"actions,omitempty"`
}

// KeybindingsConfig represents keybinding settings
type KeybindingsConfig struct {
	// Session list screen
	Up      []string `mapstructure:"up,omitempty"`
	Down    []string `mapstructure:"down,omitempty"`
	Attach  []string `mapstructure:"attach,omitempty"`
	New     []string `mapstructure:"new,omitempty"`
	Kill    []string `mapstructure:"kill,omitempty"`
	Delete  []string `mapstructure:"delete,omitempty"`
	Refresh []string `mapstructure:"refresh,omitempty"`
	Quit    []string `mapstructure:"quit,omitempty"`
	Help    []string `mapstructure:"help,omitempty"`
	Search  []string `mapstructure:"search,omitempty"`
	Vscode  []string `mapstructure:"vscode,omitempty"`

	// Session creation form
	NextField  []string `mapstructure:"next_field,omitempty"`
	PrevField  []string `mapstructure:"prev_field,omitempty"`
	Submit     []string `mapstructure:"submit,omitempty"`
	CancelForm []string `mapstructure:"cancel_form,omitempty"`

	// Keys while attached
	Detach []string `mapstructure:"detach,omitempty"`

	// Outer tmux (jin-mgr) — sidebar toggle keys.
	// Format: tmux bind-key notation (e.g., "M-\\" for Alt+Backslash, "M-b" for Alt+b).
	// nil ⇒ use default from DefaultKeybindings. Explicit empty slice ⇒ disabled
	// (no bind-key is issued).
	TogglePane []string `mapstructure:"toggle_pane,omitempty"`

	// Outer tmux (jin-mgr) — action panel trigger.
	// Format: tmux bind-key notation (e.g., "M-p" for Alt+p). Must be
	// modifier-prefixed so bare-letter input in the display pane still reaches
	// the agent.
	// nil ⇒ use default from DefaultKeybindings. Explicit empty slice ⇒ disabled
	// (no bind-key is issued).
	ActionPanel []string `mapstructure:"action_panel,omitempty"`

	// Outer tmux (jin-mgr) — per-plugin-action triggers.
	// Key: plugin name (matches `name:` in jind-ai-plugin.yaml).
	// Absent map / empty map ⇒ no per-plugin bindings (no default).
	// Each PluginActionKeybinding.Keys is passed straight to tmux bind-key.
	Plugins map[string]PluginKeybindings `mapstructure:"plugins,omitempty"`
}

// WorktreeConfig represents settings for the git-worktree session option.
type WorktreeConfig struct {
	BaseDir       string `mapstructure:"base_dir,omitempty"`       // Placement template. Empty → paths.State()/worktrees/{name}
	BranchPrefix  string `mapstructure:"branch_prefix,omitempty"`  // Auto-generated branch name prefix (default: "jin/")
	DefaultBranch string `mapstructure:"default_branch,omitempty"` // Fallback when origin/HEAD detection fails
	HookEnabled   *bool  `mapstructure:"hook_enabled,omitempty"`   // nil = default(true)
	HookTimeout   int    `mapstructure:"hook_timeout,omitempty"`   // seconds, 0 = default(300)
}

// PluginsConfig represents settings for the plugin dispatcher.
type PluginsConfig struct {
	Enabled      *bool    `mapstructure:"enabled"`       // nil = default(true)
	Disabled     []string `mapstructure:"disabled"`      // plugin names to skip dispatch for
	BuildTimeout int      `mapstructure:"build_timeout"` // seconds, 0 = default(300)
	Debounce     int      `mapstructure:"debounce"`      // seconds, 0 = default(3)
}

// PopupSizeConfig represents a single popup's percent-based dimensions.
// A field value of 0 (or the whole struct being nil) means "unset — fall
// through to the next tier". Valid range is 1-100 inclusive.
type PopupSizeConfig struct {
	Width  int `mapstructure:"width,omitempty"`
	Height int `mapstructure:"height,omitempty"`
}

// PopupsConfig groups per-popup size overrides. All fields are pointers so
// "unset" (nil) is distinguishable from "explicit zero" — both fall through
// to the hardcoded default, but only nil is silent.
type PopupsConfig struct {
	Create        *PopupSizeConfig            `mapstructure:"create,omitempty"`
	SessionFilter *PopupSizeConfig            `mapstructure:"session_filter,omitempty"`
	Help          *PopupSizeConfig            `mapstructure:"help,omitempty"`
	Action        *PopupSizeConfig            `mapstructure:"action,omitempty"`
	PluginDefault *PopupSizeConfig            `mapstructure:"plugin_default,omitempty"`
	Plugins       map[string]*PopupSizeConfig `mapstructure:"plugins,omitempty"`
}

// Config represents the application-wide configuration
type Config struct {
	Keybindings  KeybindingsConfig `mapstructure:"keybindings,omitempty"`   // Keybinding settings
	Worktree     WorktreeConfig    `mapstructure:"worktree,omitempty"`      // Git worktree session settings
	Plugins      PluginsConfig     `mapstructure:"plugins,omitempty"`       // Plugin dispatcher settings
	Popups       PopupsConfig      `mapstructure:"popups,omitempty"`        // Popup size overrides (core + plugin)
	Env          map[string]string `mapstructure:"-"`                       // Custom environment variables (loaded separately to preserve key case)
	DefaultAgent string            `mapstructure:"default_agent,omitempty"` // Adapter used when `jin session new` omits --agent (empty ⇒ "claude")
}

// Manager manages reading and writing configuration files
type Manager struct {
	v        *viper.Viper
	mu       sync.RWMutex
	config   *Config
	filePath string
	warned   map[string]bool // warn-once keys (popup-size resolution, deprecated config shapes); guarded by mu (Lock).
}

// NewManager creates a new configuration manager
func NewManager(dataDir string) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}

	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(dataDir)

	m := &Manager{
		v:        v,
		filePath: filepath.Join(dataDir, "config.yaml"),
		config:   defaultConfig(),
	}

	if err := m.load(); err != nil {
		// Use default settings if file does not exist
		if !os.IsNotExist(err) {
			if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
				return nil, err
			}
		}
	}

	return m, nil
}

// defaultConfig returns the default configuration
func defaultConfig() *Config {
	return &Config{
		Worktree: DefaultWorktreeConfig(),
		Plugins:  DefaultPluginsConfig(),
	}
}

// DefaultWorktreeConfig returns the default worktree configuration.
// BaseDir is left empty to signal "resolve at runtime from paths.State()" — this
// avoids importing internal/paths here (which would create an import cycle when
// paths eventually needs config).
func DefaultWorktreeConfig() WorktreeConfig {
	tru := true
	return WorktreeConfig{
		BaseDir:       "",
		BranchPrefix:  "jin/",
		DefaultBranch: "",
		HookEnabled:   &tru,
		HookTimeout:   300,
	}
}

// DefaultPluginsConfig returns the default plugin dispatcher configuration.
func DefaultPluginsConfig() PluginsConfig {
	tru := true
	return PluginsConfig{
		Enabled:      &tru,
		Disabled:     nil,
		BuildTimeout: 300,
		Debounce:     3,
	}
}

// Canonical popup names. These are the single source of truth for both the
// config schema (YAML keys under popups.*), the resolver's default lookup,
// and the hidden `jin *-popup` subcommand names the TUI launches. Using
// these constants at every call site prevents silent drift between the
// user config key, the default entry, and the subcommand jindaiko spawns.
const (
	PopupCreate        = "create"
	PopupSessionFilter = "session_filter"
	PopupHelp          = "help"
	PopupAction        = "action"
	PopupPluginDefault = "plugin_default"
)

// popupSpec bundles the per-popup metadata that must stay in lock-step:
// the hardcoded default size and the subcommand jindaiko spawns for it.
// Keeping them adjacent kills the fragile "config-key → subcommand" name
// derivation the TUI used to run at popup open time.
type popupSpec struct {
	DefaultSize PopupSizeConfig
	Subcmd      string
}

// popupCatalog owns the canonical popup metadata. Adding a new popup means
// touching this one map — the resolver, subcommand lookup, and default table
// all read from here.
var popupCatalog = map[string]popupSpec{
	PopupCreate:        {DefaultSize: PopupSizeConfig{Width: 80, Height: 80}, Subcmd: "create-popup"},
	PopupSessionFilter: {DefaultSize: PopupSizeConfig{Width: 70, Height: 70}, Subcmd: "session-filter-popup"},
	PopupHelp:          {DefaultSize: PopupSizeConfig{Width: 60, Height: 60}, Subcmd: "help-popup"},
	PopupAction:        {DefaultSize: PopupSizeConfig{Width: 70, Height: 70}, Subcmd: "action-popup"},
	PopupPluginDefault: {DefaultSize: PopupSizeConfig{Width: 70, Height: 70}, Subcmd: ""},
}

// defaultUnknownPopup is the final fallback used by GetPopupSize when the
// requested name isn't in popupCatalog — a safety net for programmer error
// (all in-tree callers pass canonical names). Kept as a plain value so
// there's no map lookup on this path.
var defaultUnknownPopup = PopupSizeConfig{Width: 70, Height: 70}

// DefaultPopupSizes returns a snapshot of the canonical default sizes,
// keyed by popup name. The returned map is a fresh copy — callers may
// mutate it freely without touching the shared catalog.
func DefaultPopupSizes() map[string]PopupSizeConfig {
	out := make(map[string]PopupSizeConfig, len(popupCatalog)+1)
	for name, spec := range popupCatalog {
		out[name] = spec.DefaultSize
	}
	out["default"] = defaultUnknownPopup
	return out
}

// PopupSubcmd returns the hidden `jin` subcommand name that renders the
// popup with the given canonical name. Empty for unknown names and for
// PopupPluginDefault (which is a resolver tier, not a real popup).
func PopupSubcmd(name string) string {
	if spec, ok := popupCatalog[name]; ok {
		return spec.Subcmd
	}
	return ""
}

// load reads the configuration file
func (m *Manager) load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.v.ReadInConfig(); err != nil {
		return err
	}

	cfg := &Config{}
	if err := m.v.Unmarshal(cfg); err != nil {
		return err
	}
	m.dropDeprecatedPluginKeybindings(cfg)

	cfg.Env = loadEnvFromFile(m.filePath)
	m.config = cfg
	return nil
}

// dropDeprecatedPluginKeybindings detects plugins still configured with the
// pre-0.8 shape — `keys` directly under keybindings.plugins.<name> instead
// of nested under `actions.<id>` — and drops their bindings from cfg. The
// new struct doesn't declare a `keys` field, so viper's Unmarshal silently
// ignores it; only the raw viper data reveals the old shape. Each affected
// plugin gets one warning per Manager lifetime (stderr via log; the daemon's
// debug log captures the same stream) telling the user to rewrite the entry.
// Must be called with m.mu held (writes m.warned directly).
func (m *Manager) dropDeprecatedPluginKeybindings(cfg *Config) {
	raw, ok := m.v.Get("keybindings.plugins").(map[string]interface{})
	if !ok {
		return
	}
	for name, v := range raw {
		entry, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if _, hasKeys := entry["keys"]; !hasKeys {
			continue
		}
		delete(cfg.Keybindings.Plugins, name)
		m.warnOnceLocked(
			"keybindings.plugins."+name+".v1shape",
			"plugin keybindings config: %s uses deprecated v1 shape (map[plugin]{keys}); rewrite as map[plugin]{actions.<id>.keys}. binding ignored.",
			name,
		)
	}
}

// loadEnvFromFile reads the "env" map from the YAML file directly,
// preserving the original key case (viper lowercases all keys).
func loadEnvFromFile(filePath string) map[string]string {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}

	var raw struct {
		Env map[string]string `yaml:"env"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil
	}
	return raw.Env
}

// Reload re-reads the configuration file.
func (m *Manager) Reload() error {
	return m.load()
}

// Save writes the configuration to file.
// Note: Env field is not persisted by Save (it is loaded directly from YAML via loadEnvFromFile).
func (m *Manager) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.v.WriteConfigAs(m.filePath)
}

// Get returns the current configuration
func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cfg := *m.config
	if cfg.Env != nil {
		env := make(map[string]string, len(cfg.Env))
		for k, v := range cfg.Env {
			env[k] = v
		}
		cfg.Env = env
	}
	return &cfg
}

// GetWorktreeConfig returns the worktree configuration, filling unset fields
// from DefaultWorktreeConfig so callers can rely on non-empty defaults.
// BaseDir is intentionally kept empty when unset — the caller resolves it via
// paths.State() to avoid a config→paths import cycle.
func (m *Manager) GetWorktreeConfig() WorktreeConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cfg := m.config.Worktree
	defaults := DefaultWorktreeConfig()
	if cfg.BranchPrefix == "" {
		cfg.BranchPrefix = defaults.BranchPrefix
	}
	if cfg.HookEnabled == nil {
		cfg.HookEnabled = defaults.HookEnabled
	}
	if cfg.HookTimeout == 0 {
		cfg.HookTimeout = defaults.HookTimeout
	}
	return cfg
}

// GetPluginsConfig returns the plugin dispatcher configuration, filling unset
// fields from DefaultPluginsConfig so callers can rely on non-empty defaults.
func (m *Manager) GetPluginsConfig() PluginsConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cfg := m.config.Plugins
	defaults := DefaultPluginsConfig()
	if cfg.Enabled == nil {
		cfg.Enabled = defaults.Enabled
	}
	if cfg.BuildTimeout == 0 {
		cfg.BuildTimeout = defaults.BuildTimeout
	}
	if cfg.Debounce == 0 {
		cfg.Debounce = defaults.Debounce
	}
	return cfg
}

// GetEnv returns the custom environment variables configured for Claude Code sessions.
// Returns an empty map (never nil) if no env vars are configured.
func (m *Manager) GetEnv() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.config.Env == nil {
		return make(map[string]string)
	}

	// Return a copy to prevent callers from modifying the internal state
	env := make(map[string]string, len(m.config.Env))
	for k, v := range m.config.Env {
		env[k] = v
	}
	return env
}

// GetShell returns the shell to use when launching the agent.
// Uses the $SHELL environment variable, defaulting to /bin/sh.
func (m *Manager) GetShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

// GetDefaultAgent returns the adapter kind selected when `jin session new`
// omits --agent. Empty / unset falls back to "claude" so a fresh install
// works without touching config.yaml.
func (m *Manager) GetDefaultAgent() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.config == nil || m.config.DefaultAgent == "" {
		return "claude"
	}
	return m.config.DefaultAgent
}

// DefaultKeybindings returns the default keybindings
func DefaultKeybindings() KeybindingsConfig {
	return KeybindingsConfig{
		// Session list screen
		Up:      []string{"up", "k"},
		Down:    []string{"down", "j"},
		Attach:  []string{"enter"},
		New:     []string{"n"},
		Kill:    []string{"x"},
		Delete:  []string{"d"},
		Refresh: []string{"r"},
		Quit:    []string{"q", "ctrl+c"},
		Help:    []string{"?"},
		Search:  []string{"M-f"},
		Vscode:  []string{"v"},

		// Session creation form
		NextField:  []string{"tab"},
		PrevField:  []string{"shift+tab"},
		Submit:     []string{"enter"},
		CancelForm: []string{"esc"},

		// Keys while attached
		Detach: []string{"ctrl+]"},

		// Outer tmux — sidebar toggle
		TogglePane: []string{"M-\\"},

		// Outer tmux — action panel trigger
		ActionPanel: []string{"M-p"},
	}
}

// GetKeybindings returns keybinding settings (uses defaults for unset items)
func (m *Manager) GetKeybindings() KeybindingsConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	defaults := DefaultKeybindings()
	cfg := m.config.Keybindings

	// Use defaults for unset items
	if len(cfg.Up) == 0 {
		cfg.Up = defaults.Up
	}
	if len(cfg.Down) == 0 {
		cfg.Down = defaults.Down
	}
	if len(cfg.Attach) == 0 {
		cfg.Attach = defaults.Attach
	}
	if len(cfg.New) == 0 {
		cfg.New = defaults.New
	}
	if len(cfg.Kill) == 0 {
		cfg.Kill = defaults.Kill
	}
	if len(cfg.Delete) == 0 {
		cfg.Delete = defaults.Delete
	}
	if len(cfg.Refresh) == 0 {
		cfg.Refresh = defaults.Refresh
	}
	if len(cfg.Quit) == 0 {
		cfg.Quit = defaults.Quit
	}
	if len(cfg.Help) == 0 {
		cfg.Help = defaults.Help
	}
	if len(cfg.Search) == 0 {
		cfg.Search = defaults.Search
	}
	if len(cfg.Vscode) == 0 {
		cfg.Vscode = defaults.Vscode
	}
	if len(cfg.NextField) == 0 {
		cfg.NextField = defaults.NextField
	}
	if len(cfg.PrevField) == 0 {
		cfg.PrevField = defaults.PrevField
	}
	if len(cfg.Submit) == 0 {
		cfg.Submit = defaults.Submit
	}
	if len(cfg.CancelForm) == 0 {
		cfg.CancelForm = defaults.CancelForm
	}
	if len(cfg.Detach) == 0 {
		cfg.Detach = defaults.Detach
	} else {
		// Filter out unsupported keys
		var valid []string
		for _, k := range cfg.Detach {
			if err := ValidateDetachKey(k); err != nil {
				log.Printf("WARNING: %v", err)
			} else {
				valid = append(valid, k)
			}
		}
		if len(valid) == 0 {
			cfg.Detach = defaults.Detach
		} else {
			cfg.Detach = valid
		}
	}
	if len(cfg.ActionPanel) == 0 {
		cfg.ActionPanel = defaults.ActionPanel
	}

	return cfg
}

// GetDetachKey returns the byte value of the detach key used while attached
func (m *Manager) GetDetachKey() byte {
	m.mu.RLock()
	defer m.mu.RUnlock()

	detachKeys := m.config.Keybindings.Detach
	if len(detachKeys) == 0 {
		detachKeys = DefaultKeybindings().Detach
	}

	return parseKeyToByte(detachKeys[0])
}

// GetDetachKeyHint returns the display string for the detach key
func (m *Manager) GetDetachKeyHint() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	detachKeys := m.config.Keybindings.Detach
	if len(detachKeys) == 0 {
		detachKeys = DefaultKeybindings().Detach
	}

	return formatKeyHint(detachKeys[0])
}

// GetDetachKeyCSIu returns the CSI u sequence for the detach key
func (m *Manager) GetDetachKeyCSIu() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()

	detachKeys := m.config.Keybindings.Detach
	if len(detachKeys) == 0 {
		detachKeys = DefaultKeybindings().Detach
	}

	return parseKeyToCSIu(detachKeys[0])
}

// parseKeyToByte converts a key string to its byte value
func parseKeyToByte(key string) byte {
	switch key {
	case "ctrl+^":
		return 0x1e
	case "ctrl+]":
		return 0x1d
	case "ctrl+\\":
		return 0x1c
	case "ctrl+g":
		return 0x07
	default:
		return 0x1d // default: ctrl+]
	}
}

// parseKeyToCSIu converts a key string to its CSI u sequence.
// For terminals that support CSI u mode (e.g., iTerm2).
func parseKeyToCSIu(key string) []byte {
	switch key {
	case "ctrl+^":
		// Ctrl+Shift+6: keycode=54('6'), modifiers=6(Ctrl+Shift)
		return []byte("\x1b[54;6u")
	case "ctrl+]":
		// Ctrl+]: keycode=93(']'), modifiers=5(Ctrl)
		return []byte("\x1b[93;5u")
	case "ctrl+\\":
		// Ctrl+\: keycode=92('\'), modifiers=5(Ctrl)
		return []byte("\x1b[92;5u")
	case "ctrl+g":
		// Ctrl+G: keycode=103('g'), modifiers=5(Ctrl)
		return []byte("\x1b[103;5u")
	default:
		return []byte("\x1b[93;5u") // default: ctrl+]
	}
}

// formatKeyHint formats a key string for display
func formatKeyHint(key string) string {
	switch key {
	case "ctrl+^":
		return "Ctrl+^"
	case "ctrl+]":
		return "Ctrl+]"
	case "ctrl+\\":
		return "Ctrl+\\"
	case "ctrl+g":
		return "Ctrl+G"
	default:
		return "Ctrl+]"
	}
}

// formatKeyForTmux converts a key string to tmux bind-key format
func formatKeyForTmux(key string) string {
	switch key {
	case "ctrl+^":
		return "C-^"
	case "ctrl+]":
		return "C-]"
	case "ctrl+\\":
		return "C-\\"
	case "ctrl+g":
		return "C-g"
	default:
		return "C-]"
	}
}

// GetDetachKeyTmux returns the detach key as a tmux bind-key format string
func (m *Manager) GetDetachKeyTmux() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	detachKeys := m.config.Keybindings.Detach
	if len(detachKeys) == 0 {
		detachKeys = DefaultKeybindings().Detach
	}

	return formatKeyForTmux(detachKeys[0])
}

// GetTogglePaneKeys returns the tmux bind-key strings for the outer-tmux
// sidebar toggle. The nil ↔ empty-slice distinction is significant:
//   - nil (field unset in config) ⇒ default from DefaultKeybindings
//   - explicit empty slice ⇒ feature disabled by the user (no bind-key issued)
//
// The returned strings are passed straight to tmux bind-key, so they use tmux
// notation (e.g. "M-\\" for Alt+Backslash) rather than the "ctrl+]" style used
// by Detach.
func (m *Manager) GetTogglePaneKeys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.config.Keybindings.TogglePane == nil {
		return DefaultKeybindings().TogglePane
	}
	return normalizeTmuxKeys(m.config.Keybindings.TogglePane)
}

// GetActionPanelKeys returns the tmux bind-key strings for the outer-tmux
// action panel trigger. The nil ↔ empty-slice distinction is significant:
//   - nil (field unset in config) ⇒ default from DefaultKeybindings
//   - explicit empty slice ⇒ feature disabled by the user (no bind-key issued)
//
// The returned strings are passed straight to tmux bind-key, so they use tmux
// notation (e.g. "M-p" for Alt+p).
func (m *Manager) GetActionPanelKeys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.config.Keybindings.ActionPanel == nil {
		return DefaultKeybindings().ActionPanel
	}
	return normalizeTmuxKeys(m.config.Keybindings.ActionPanel)
}

// GetPluginKeybindings returns the per-plugin-action outer-tmux bindings as
// plugin name → action ID → tmux keys. Returns a fresh copy (never the
// internal maps, never nil) so callers can iterate or mutate without
// touching Manager state — same defensive-copy policy as GetEnv. Semantics:
//   - plugin absent from map ⇒ no bindings (plugins whose actions all
//     filtered down to empty Keys are omitted entirely)
//   - action absent / Keys nil / empty ⇒ that action has no binding
//   - Keys non-empty ⇒ each string is passed straight to tmux bind-key
//
// Plugins whose config still uses the deprecated pre-0.8 shape were already
// dropped at load time (see dropDeprecatedPluginKeybindings) and never
// appear here.
func (m *Manager) GetPluginKeybindings() map[string]map[string][]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	src := m.config.Keybindings.Plugins
	out := make(map[string]map[string][]string, len(src))
	for name, kb := range src {
		actions := make(map[string][]string, len(kb.Actions))
		for actionID, akb := range kb.Actions {
			if len(akb.Keys) == 0 {
				continue
			}
			// normalizeTmuxKeys allocates a fresh slice for non-empty input,
			// so this is already a defensive copy.
			actions[actionID] = normalizeTmuxKeys(akb.Keys)
		}
		if len(actions) > 0 {
			out[name] = actions
		}
	}
	return out
}

// GetSessionFilterKeys returns the tmux bind-key strings for the outer-tmux
// session filter popup trigger. Same nil ↔ empty-slice semantics as
// GetActionPanelKeys. Sourced from the keybindings.search field (repurposed
// from the removed inline substring filter into the popup launcher).
func (m *Manager) GetSessionFilterKeys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.config.Keybindings.Search == nil {
		return DefaultKeybindings().Search
	}
	return normalizeTmuxKeys(m.config.Keybindings.Search)
}

// GetPopupSize returns tmux-formatted width/height strings (e.g. "70%") for a
// canonical core popup name (create / session_filter / help / action).
// Unknown names fall back to the "default" entry in
// DefaultPopupSizes. Out-of-range user values (<1 or >100) log a warn once
// per key and fall back to the hardcoded default; a zero value falls back
// silently.
func (m *Manager) GetPopupSize(name string) (width, height string) {
	m.mu.RLock()
	var user *PopupSizeConfig
	if m.config != nil {
		user = coreUserPopup(&m.config.Popups, name)
	}
	m.mu.RUnlock()
	return m.resolvePopupSize(user, name)
}

// GetPluginPopupSize resolves a plugin popup's size using the priority chain:
//  1. user config popups.plugins[pluginName]
//  2. manifest (caller converts manifest.PopupConfig → PopupSizeConfig to
//     avoid an import cycle from pkg/plugin/manifest into internal/config)
//  3. user config popups.plugin_default
//  4. hardcoded DefaultPopupSizes["plugin_default"]
//
// The returned strings are tmux-formatted percent values.
func (m *Manager) GetPluginPopupSize(pluginName string, manifest *PopupSizeConfig) (width, height string) {
	m.mu.RLock()
	var perPlugin, pluginDefault *PopupSizeConfig
	if m.config != nil {
		if m.config.Popups.Plugins != nil {
			perPlugin = m.config.Popups.Plugins[pluginName]
		}
		pluginDefault = m.config.Popups.PluginDefault
	}
	m.mu.RUnlock()
	return m.resolvePluginPopupSize(perPlugin, manifest, pluginDefault, pluginName)
}

// coreUserPopup returns the user-configured popup size for a canonical core
// popup name, or nil if the name is not one of the five core popups.
func coreUserPopup(p *PopupsConfig, name string) *PopupSizeConfig {
	switch name {
	case "create":
		return p.Create
	case "session_filter":
		return p.SessionFilter
	case "help":
		return p.Help
	case "action":
		return p.Action
	}
	return nil
}

// popupTier bundles a size config with its warn-key prefix so out-of-range
// values can be reported without duplicate log spam.
type popupTier struct {
	cfg *PopupSizeConfig
	key string
}

// resolvePopupSize applies the single-tier lookup used by core popups.
func (m *Manager) resolvePopupSize(user *PopupSizeConfig, name string) (width, height string) {
	fallback := defaultUnknownPopup
	if spec, ok := popupCatalog[name]; ok {
		fallback = spec.DefaultSize
	}
	tiers := []popupTier{
		{cfg: user, key: "popups." + name},
	}
	w := m.pickPopupDim(tiers, "width", fallback.Width)
	h := m.pickPopupDim(tiers, "height", fallback.Height)
	return formatPopupPercent(w, h)
}

// resolvePluginPopupSize applies the 4-tier lookup used by plugin popups.
func (m *Manager) resolvePluginPopupSize(perPlugin, manifest, pluginDefault *PopupSizeConfig, pluginName string) (width, height string) {
	fallback := popupCatalog[PopupPluginDefault].DefaultSize
	tiers := []popupTier{
		{cfg: perPlugin, key: "popups.plugins." + pluginName},
		{cfg: manifest, key: "manifest:" + pluginName},
		{cfg: pluginDefault, key: "popups.plugin_default"},
	}
	w := m.pickPopupDim(tiers, "width", fallback.Width)
	h := m.pickPopupDim(tiers, "height", fallback.Height)
	return formatPopupPercent(w, h)
}

// formatPopupPercent shapes two integer percents into the tmux width/height
// notation ("70%") both resolvers hand back to callers.
func formatPopupPercent(w, h int) (width, height string) {
	return fmt.Sprintf("%d%%", w), fmt.Sprintf("%d%%", h)
}

// pickPopupDim walks tiers in priority order, returning the first valid
// dimension in [1, 100]. Zero silently falls through; out-of-range emits a
// warn once per (tier-key, dim) pair and falls through. Returns fallback
// when no tier yields a valid value.
func (m *Manager) pickPopupDim(tiers []popupTier, dim string, fallback int) int {
	for _, t := range tiers {
		if t.cfg == nil {
			continue
		}
		v := t.cfg.Width
		if dim == "height" {
			v = t.cfg.Height
		}
		if v == 0 {
			continue
		}
		if v < 1 || v > 100 {
			m.warnPopupOnce(
				t.key+"."+dim,
				"config: %s.%s = %d out of range (1-100); falling back",
				t.key, dim, v,
			)
			continue
		}
		return v
	}
	return fallback
}

// warnPopupOnce logs a warning at most once per key over the Manager's
// lifetime. Uses mu (write lock) to guard warned; safe to call concurrently.
func (m *Manager) warnPopupOnce(key, format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warnOnceLocked(key, format, args...)
}

// warnOnceLocked is the lock-free core of the warn-once mechanism, shared by
// warnPopupOnce and dropDeprecatedPluginKeybindings. Must be called with
// m.mu held (write lock).
func (m *Manager) warnOnceLocked(key, format string, args ...any) {
	if m.warned == nil {
		m.warned = make(map[string]bool)
	}
	if m.warned[key] {
		return
	}
	m.warned[key] = true
	log.Printf(format, args...)
}
