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

// HostConfig represents a remote host configuration
type HostConfig struct {
	ID          string   `mapstructure:"id"`                     // Host identifier (e.g., "ec2", "docker-dev")
	Type        string   `mapstructure:"type"`                   // "ssh" or "docker"
	Host        string   `mapstructure:"host,omitempty"`         // SSH target (e.g., "ec2-host")
	SSHOpts     []string `mapstructure:"ssh_opts,omitempty"`     // Additional SSH options
	Container   string   `mapstructure:"container,omitempty"`    // Docker container name/ID
	SocketPath  string   `mapstructure:"socket_path,omitempty"`  // Remote socket path (default: ~/.ccvalet/run/daemon.sock)
	CcvaletPath string   `mapstructure:"ccvalet_path,omitempty"` // Full path to ccvalet binary on remote (default: ccvalet)
}

// Config represents the application-wide configuration
type Config struct {
	HostID      string            `mapstructure:"host_id,omitempty"`     // This daemon's host ID (default: "local")
	Keybindings KeybindingsConfig `mapstructure:"keybindings,omitempty"` // Keybinding settings
	Hosts       []HostConfig      `mapstructure:"hosts,omitempty"`       // Remote host settings
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
	return &Config{}
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

	m.config = cfg
	return nil
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

	m.config = cfg
	return nil
}

// Save writes the configuration to file
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
	return &cfg
}

// GetHosts returns the list of remote hosts
func (m *Manager) GetHosts() []HostConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	hosts := make([]HostConfig, len(m.config.Hosts))
	copy(hosts, m.config.Hosts)
	return hosts
}

// GetHost returns the host with the given ID
func (m *Manager) GetHost(id string) *HostConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, h := range m.config.Hosts {
		if h.ID == id {
			hc := h
			return &hc
		}
	}
	return nil
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

// GetHostID returns the configured host ID (empty string if not set)
func (m *Manager) GetHostID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.HostID
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
