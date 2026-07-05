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

// KeybindingsConfig represents keybinding settings
type KeybindingsConfig struct {
	// Session list screen
	Up            []string `mapstructure:"up,omitempty"`
	Down          []string `mapstructure:"down,omitempty"`
	Attach        []string `mapstructure:"attach,omitempty"`
	New           []string `mapstructure:"new,omitempty"`
	Kill          []string `mapstructure:"kill,omitempty"`
	Delete        []string `mapstructure:"delete,omitempty"`
	Refresh       []string `mapstructure:"refresh,omitempty"`
	Quit          []string `mapstructure:"quit,omitempty"`
	Help          []string `mapstructure:"help,omitempty"`
	Search        []string `mapstructure:"search,omitempty"`
	Vscode        []string `mapstructure:"vscode,omitempty"`
	Notifications []string `mapstructure:"notifications,omitempty"`

	// Session creation form
	NextField  []string `mapstructure:"next_field,omitempty"`
	PrevField  []string `mapstructure:"prev_field,omitempty"`
	Submit     []string `mapstructure:"submit,omitempty"`
	CancelForm []string `mapstructure:"cancel_form,omitempty"`

	// Keys while attached
	Detach []string `mapstructure:"detach,omitempty"`
}

// WorktreeConfig represents settings for the git-worktree session option.
type WorktreeConfig struct {
	BaseDir       string `mapstructure:"base_dir,omitempty"`       // Placement template. Empty → paths.State()/worktrees/{name}
	BranchPrefix  string `mapstructure:"branch_prefix,omitempty"`  // Auto-generated branch name prefix (default: "jin/")
	DefaultBranch string `mapstructure:"default_branch,omitempty"` // Fallback when origin/HEAD detection fails
	HookEnabled   *bool  `mapstructure:"hook_enabled,omitempty"`   // nil = default(true)
	HookTimeout   int    `mapstructure:"hook_timeout,omitempty"`   // seconds, 0 = default(300)
}

// Config represents the application-wide configuration
type Config struct {
	Keybindings KeybindingsConfig `mapstructure:"keybindings,omitempty"` // Keybinding settings
	Worktree    WorktreeConfig    `mapstructure:"worktree,omitempty"`    // Git worktree session settings
	Env         map[string]string `mapstructure:"-"`                     // Custom environment variables (loaded separately to preserve key case)
}

// Manager manages reading and writing configuration files
type Manager struct {
	v        *viper.Viper
	mu       sync.RWMutex
	config   *Config
	filePath string
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

	cfg.Env = loadEnvFromFile(m.filePath)
	m.config = cfg
	return nil
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

// Reload re-reads the configuration file
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.v.ReadInConfig(); err != nil {
		return err
	}

	cfg := &Config{}
	if err := m.v.Unmarshal(cfg); err != nil {
		return err
	}

	cfg.Env = loadEnvFromFile(m.filePath)
	m.config = cfg
	return nil
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

// GetShell returns the shell to use when launching Claude Code.
// Uses the $SHELL environment variable, defaulting to /bin/sh.
func (m *Manager) GetShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

// DefaultKeybindings returns the default keybindings
func DefaultKeybindings() KeybindingsConfig {
	return KeybindingsConfig{
		// Session list screen
		Up:            []string{"up", "k"},
		Down:          []string{"down", "j"},
		Attach:        []string{"enter"},
		New:           []string{"n"},
		Kill:          []string{"x"},
		Delete:        []string{"d"},
		Refresh:       []string{"r"},
		Quit:          []string{"q", "ctrl+c"},
		Help:          []string{"?"},
		Search:        []string{"/"},
		Vscode:        []string{"v"},
		Notifications: []string{"!"},

		// Session creation form
		NextField:  []string{"tab"},
		PrevField:  []string{"shift+tab"},
		Submit:     []string{"enter"},
		CancelForm: []string{"esc"},

		// Keys while attached
		Detach: []string{"ctrl+]"},
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
	if len(cfg.Notifications) == 0 {
		cfg.Notifications = defaults.Notifications
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
