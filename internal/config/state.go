package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// maxDirHistory is the maximum number of directory history entries
const maxDirHistory = 20

// DirHistoryEntry represents a single directory usage history entry
type DirHistoryEntry struct {
	Path       string    `yaml:"path"`
	LastUsedAt time.Time `yaml:"last_used_at"`
}

// State represents the application state (persistent state, not configuration)
type State struct {
	DirHistory             []DirHistoryEntry `yaml:"dir_history,omitempty"`
	RemoteControllerID     string            `yaml:"remote_controller_id,omitempty"`
	RemoteServerInstanceID string            `yaml:"remote_server_instance_id,omitempty"`
}

func (m *StateManager) EnsureRemoteControllerID() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.RemoteControllerID == "" {
		m.state.RemoteControllerID = "ctl-" + uuid.NewString()
		if err := m.saveLocked(); err != nil {
			m.state.RemoteControllerID = ""
			return "", fmt.Errorf("persist remote controller identity: %w", err)
		}
	}
	return m.state.RemoteControllerID, nil
}

func (m *StateManager) EnsureRemoteServerInstanceID() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.RemoteServerInstanceID == "" {
		m.state.RemoteServerInstanceID = "srv-" + uuid.NewString()
		if err := m.saveLocked(); err != nil {
			m.state.RemoteServerInstanceID = ""
			return "", fmt.Errorf("persist remote server identity: %w", err)
		}
	}
	return m.state.RemoteServerInstanceID, nil
}

// StateManager manages reading and writing state files
type StateManager struct {
	mu       sync.RWMutex
	state    *State
	filePath string
}

// NewStateManager creates a new state manager
func NewStateManager(dataDir string) (*StateManager, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}

	m := &StateManager{
		filePath: filepath.Join(dataDir, "state.yaml"),
		state:    &State{},
	}

	if err := m.load(); err != nil {
		// Use empty state if file does not exist
		if !os.IsNotExist(err) {
			return nil, err
		}
	}

	return m, nil
}

// load reads the state file
func (m *StateManager) load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := os.ReadFile(m.filePath)
	if err != nil {
		return err
	}

	state := &State{}
	if err := yaml.Unmarshal(data, state); err != nil {
		return err
	}

	m.state = state
	return nil
}

// Save writes the state to file
func (m *StateManager) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked()
}

// saveLocked saves the state (caller must hold the lock)
func (m *StateManager) saveLocked() error {
	data, err := yaml.Marshal(m.state)
	if err != nil {
		return err
	}
	return os.WriteFile(m.filePath, data, 0644)
}

// RecordDirUsage records directory usage history.
// Updates LastUsedAt if the same path already exists, otherwise adds a new entry.
// Removes oldest entries if the total exceeds maxDirHistory.
func (m *StateManager) RecordDirUsage(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	found := false
	for i := range m.state.DirHistory {
		if m.state.DirHistory[i].Path == path {
			m.state.DirHistory[i].LastUsedAt = now
			found = true
			break
		}
	}
	if !found {
		m.state.DirHistory = append(m.state.DirHistory, DirHistoryEntry{
			Path:       path,
			LastUsedAt: now,
		})
	}

	// Sort by LastUsedAt descending
	sort.Slice(m.state.DirHistory, func(i, j int) bool {
		return m.state.DirHistory[i].LastUsedAt.After(m.state.DirHistory[j].LastUsedAt)
	})

	// Trim if exceeding the limit
	if len(m.state.DirHistory) > maxDirHistory {
		m.state.DirHistory = m.state.DirHistory[:maxDirHistory]
	}

	return m.saveLocked()
}

// GetDirHistory returns directory usage history sorted by LastUsedAt descending,
// up to maxEntries.
func (m *StateManager) GetDirHistory(maxEntries int) []DirHistoryEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if maxEntries <= 0 || len(m.state.DirHistory) <= maxEntries {
		result := make([]DirHistoryEntry, len(m.state.DirHistory))
		copy(result, m.state.DirHistory)
		return result
	}
	result := make([]DirHistoryEntry, maxEntries)
	copy(result, m.state.DirHistory[:maxEntries])
	return result
}

// RemoveDirHistory removes the specified directory history entry.
func (m *StateManager) RemoveDirHistory(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	filtered := m.state.DirHistory[:0]
	for _, e := range m.state.DirHistory {
		if e.Path != path {
			filtered = append(filtered, e)
		}
	}
	m.state.DirHistory = filtered

	return m.saveLocked()
}
