package config

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// maxDirHistory はディレクトリ履歴の最大保存件数
const maxDirHistory = 20

// DirHistoryEntry はディレクトリ使用履歴の1エントリを表す
type DirHistoryEntry struct {
	Path       string    `yaml:"path"`
	HostID     string    `yaml:"host_id"`
	LastUsedAt time.Time `yaml:"last_used_at"`
}

// State はアプリケーションの状態を表す（設定ではない永続的な状態）
type State struct {
	DirHistory []DirHistoryEntry `yaml:"dir_history,omitempty"`
}

// StateManager は状態ファイルの読み書きを管理する
type StateManager struct {
	mu       sync.RWMutex
	state    *State
	filePath string
}

// NewStateManager は新しい状態マネージャを作成する
func NewStateManager(dataDir string) (*StateManager, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}

	m := &StateManager{
		filePath: filepath.Join(dataDir, "state.yaml"),
		state:    &State{},
	}

	if err := m.load(); err != nil {
		// ファイルが存在しない場合は空の状態を使用
		if !os.IsNotExist(err) {
			return nil, err
		}
	}

	return m, nil
}

// load は状態ファイルを読み込む
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

// Save は状態をファイルに保存する
func (m *StateManager) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked()
}

// saveLocked はロック取得済み前提で状態を保存する
func (m *StateManager) saveLocked() error {
	data, err := yaml.Marshal(m.state)
	if err != nil {
		return err
	}
	return os.WriteFile(m.filePath, data, 0644)
}

// RecordDirUsage はディレクトリ使用履歴を記録する。
// 同じ(hostID, path)が既にあればLastUsedAtを更新、なければ追加する。
// 全体でmaxDirHistory件を超えた場合は古いものから削除する。
func (m *StateManager) RecordDirUsage(hostID, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	found := false
	for i := range m.state.DirHistory {
		if m.state.DirHistory[i].HostID == hostID && m.state.DirHistory[i].Path == path {
			m.state.DirHistory[i].LastUsedAt = now
			found = true
			break
		}
	}
	if !found {
		m.state.DirHistory = append(m.state.DirHistory, DirHistoryEntry{
			Path:       path,
			HostID:     hostID,
			LastUsedAt: now,
		})
	}

	// LastUsedAt降順ソート
	sort.Slice(m.state.DirHistory, func(i, j int) bool {
		return m.state.DirHistory[i].LastUsedAt.After(m.state.DirHistory[j].LastUsedAt)
	})

	// 上限を超えたら切り詰め
	if len(m.state.DirHistory) > maxDirHistory {
		m.state.DirHistory = m.state.DirHistory[:maxDirHistory]
	}

	return m.saveLocked()
}

// GetDirHistory は指定ホストのディレクトリ使用履歴を返す。
// LastUsedAt降順、最大maxEntries件。
func (m *StateManager) GetDirHistory(hostID string, maxEntries int) []DirHistoryEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []DirHistoryEntry
	for _, e := range m.state.DirHistory {
		if e.HostID == hostID {
			result = append(result, e)
			if len(result) >= maxEntries {
				break
			}
		}
	}
	return result
}

// RemoveDirHistory は指定のディレクトリ履歴エントリを削除する。
func (m *StateManager) RemoveDirHistory(hostID, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	filtered := m.state.DirHistory[:0]
	for _, e := range m.state.DirHistory {
		if !(e.HostID == hostID && e.Path == path) {
			filtered = append(filtered, e)
		}
	}
	m.state.DirHistory = filtered

	return m.saveLocked()
}
