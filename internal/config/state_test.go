package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateManager_NewWithDefaults(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	if sm == nil {
		t.Fatal("NewStateManager returned nil")
	}
	if sm.state == nil {
		t.Fatal("state is nil")
	}
}

func TestStateManager_SaveAndReload(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	if err := sm.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file was created
	statePath := filepath.Join(dir, "state.yaml")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state.yaml not found: %v", err)
	}

	// Create new manager from the same directory
	sm2, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager (reload): %v", err)
	}
	if sm2.state == nil {
		t.Fatal("reloaded state is nil")
	}
}

func TestStateManager_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	sm, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	if sm == nil {
		t.Fatal("NewStateManager returned nil")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}
}

func TestStateManager_RecordDirUsage_New(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	if err := sm.RecordDirUsage("local", "/home/user/project1"); err != nil {
		t.Fatalf("RecordDirUsage: %v", err)
	}

	entries := sm.GetDirHistory("local", 10)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Path != "/home/user/project1" {
		t.Errorf("Path = %q, want %q", entries[0].Path, "/home/user/project1")
	}
	if entries[0].HostID != "local" {
		t.Errorf("HostID = %q, want %q", entries[0].HostID, "local")
	}
}

func TestStateManager_RecordDirUsage_UpdateExisting(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	if err := sm.RecordDirUsage("local", "/home/user/project1"); err != nil {
		t.Fatalf("RecordDirUsage (1st): %v", err)
	}
	first := sm.GetDirHistory("local", 10)[0].LastUsedAt

	time.Sleep(10 * time.Millisecond)

	if err := sm.RecordDirUsage("local", "/home/user/project1"); err != nil {
		t.Fatalf("RecordDirUsage (2nd): %v", err)
	}

	entries := sm.GetDirHistory("local", 10)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 (dedup)", len(entries))
	}
	if !entries[0].LastUsedAt.After(first) {
		t.Error("LastUsedAt was not updated")
	}
}

func TestStateManager_RecordDirUsage_MaxLimit(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	// 25件追加 → 20件に切り詰められるはず
	for i := 0; i < 25; i++ {
		path := filepath.Join("/home/user", string(rune('a'+i)))
		if err := sm.RecordDirUsage("local", path); err != nil {
			t.Fatalf("RecordDirUsage(%d): %v", i, err)
		}
		time.Sleep(time.Millisecond) // LastUsedAtの順序保証
	}

	// 全hostで取得
	sm.mu.RLock()
	total := len(sm.state.DirHistory)
	sm.mu.RUnlock()

	if total != maxDirHistory {
		t.Errorf("total entries = %d, want %d", total, maxDirHistory)
	}
}

func TestStateManager_GetDirHistory_HostFilter(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	_ = sm.RecordDirUsage("local", "/home/user/local1")
	_ = sm.RecordDirUsage("remote1", "~/remote1")
	_ = sm.RecordDirUsage("local", "/home/user/local2")

	locals := sm.GetDirHistory("local", 10)
	if len(locals) != 2 {
		t.Fatalf("local entries = %d, want 2", len(locals))
	}

	remotes := sm.GetDirHistory("remote1", 10)
	if len(remotes) != 1 {
		t.Fatalf("remote1 entries = %d, want 1", len(remotes))
	}
	if remotes[0].Path != "~/remote1" {
		t.Errorf("remote path = %q, want %q", remotes[0].Path, "~/remote1")
	}
}

func TestStateManager_GetDirHistory_MaxEntries(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	for i := 0; i < 10; i++ {
		_ = sm.RecordDirUsage("local", filepath.Join("/home/user", string(rune('a'+i))))
		time.Sleep(time.Millisecond)
	}

	entries := sm.GetDirHistory("local", 5)
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5", len(entries))
	}

	// 最新が先頭
	if entries[0].LastUsedAt.Before(entries[4].LastUsedAt) {
		t.Error("entries not sorted by LastUsedAt descending")
	}
}

func TestStateManager_RemoveDirHistory(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	_ = sm.RecordDirUsage("local", "/home/user/a")
	_ = sm.RecordDirUsage("local", "/home/user/b")
	_ = sm.RecordDirUsage("local", "/home/user/c")

	if err := sm.RemoveDirHistory("local", "/home/user/b"); err != nil {
		t.Fatalf("RemoveDirHistory: %v", err)
	}

	entries := sm.GetDirHistory("local", 10)
	if len(entries) != 2 {
		t.Fatalf("got %d entries after remove, want 2", len(entries))
	}
	for _, e := range entries {
		if e.Path == "/home/user/b" {
			t.Error("removed entry still present")
		}
	}
}

func TestStateManager_RemoveDirHistory_NonExistent(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	// 存在しないエントリの削除はエラーにならない
	if err := sm.RemoveDirHistory("local", "/no/such/path"); err != nil {
		t.Fatalf("RemoveDirHistory(non-existent): %v", err)
	}
}

func TestStateManager_DirHistory_Persistence(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	_ = sm.RecordDirUsage("local", "/home/user/project")

	// 再読み込み
	sm2, err := NewStateManager(dir)
	if err != nil {
		t.Fatalf("NewStateManager (reload): %v", err)
	}

	entries := sm2.GetDirHistory("local", 10)
	if len(entries) != 1 {
		t.Fatalf("after reload: got %d entries, want 1", len(entries))
	}
	if entries[0].Path != "/home/user/project" {
		t.Errorf("after reload: Path = %q, want %q", entries[0].Path, "/home/user/project")
	}
}
