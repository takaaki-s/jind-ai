package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestStore returns a Store rooted at a fresh temp dir, plus that dir for
// tests that need to inspect the files on disk directly.
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, dir
}

func TestStore_NewCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if store == nil {
		t.Fatal("NewStore returned nil")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}
}

func TestStore_SaveAndLoad_RoundTrip(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	session := &Session{
		ID:             "test-123",
		Description:    "my-session",
		WorkDir:        "/home/user/project",
		CreatedAt:      now,
		Status:         StatusIdle,
		LastActiveAt:   now,
		ErrorMessage:   "some error",
		AgentSessionID: "claude-456",
		TmuxWindowName: "sess-test-123",
		TmuxPaneID:     "%42",
		CurrentWorkDir: "/runtime/dir", // persisted
		CurrentBranch:  "main",         // json:"-"
	}

	if err := store.Save(session); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load("test-123")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.ID != session.ID {
		t.Errorf("ID: got %q, want %q", loaded.ID, session.ID)
	}
	if loaded.Description != session.Description {
		t.Errorf("Description: got %q, want %q", loaded.Description, session.Description)
	}
	if loaded.WorkDir != session.WorkDir {
		t.Errorf("WorkDir: got %q, want %q", loaded.WorkDir, session.WorkDir)
	}
	if loaded.Status != session.Status {
		t.Errorf("Status: got %q, want %q", loaded.Status, session.Status)
	}
	if loaded.AgentSessionID != session.AgentSessionID {
		t.Errorf("AgentSessionID: got %q, want %q", loaded.AgentSessionID, session.AgentSessionID)
	}
	if loaded.TmuxWindowName != session.TmuxWindowName {
		t.Errorf("TmuxWindowName: got %q, want %q", loaded.TmuxWindowName, session.TmuxWindowName)
	}
	if loaded.TmuxPaneID != session.TmuxPaneID {
		t.Errorf("TmuxPaneID: got %q, want %q", loaded.TmuxPaneID, session.TmuxPaneID)
	}
}

func TestStore_Load_NotFound(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	_, err = store.Load("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent ID")
	}
}

func TestStore_LoadAll_Multiple(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	for _, id := range []string{"a", "b", "c"} {
		s := &Session{ID: id, Description: "session-" + id, Status: StatusStopped}
		if err := store.Save(s); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}

	sessions, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("LoadAll: got %d, want 3", len(sessions))
	}

	ids := make(map[string]bool)
	for _, s := range sessions {
		ids[s.ID] = true
	}
	for _, id := range []string{"a", "b", "c"} {
		if !ids[id] {
			t.Errorf("session %q not found in LoadAll result", id)
		}
	}
}

func TestStore_LoadAll_SkipsInvalidFiles(t *testing.T) {
	store, dir := newTestStore(t)

	// Valid session
	s := &Session{ID: "valid", Description: "valid-session", Status: StatusStopped}
	if err := store.Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Invalid JSON file
	if err := os.WriteFile(filepath.Join(dir, "invalid.json"), []byte("{bad json"), 0644); err != nil {
		t.Fatalf("write invalid file: %v", err)
	}

	// Non-JSON file (should be skipped)
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write txt file: %v", err)
	}

	sessions, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("LoadAll: got %d, want 1", len(sessions))
	}
	if sessions[0].ID != "valid" {
		t.Errorf("session ID: got %q, want %q", sessions[0].ID, "valid")
	}
}

func TestStore_Delete_Existing(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	s := &Session{ID: "delete-me", Status: StatusStopped}
	if err := store.Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := store.Delete("delete-me"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = store.Load("delete-me")
	if err == nil {
		t.Fatal("expected error after deletion")
	}
}

func TestStore_Delete_NonExistent(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.Delete("does-not-exist"); err != nil {
		t.Fatalf("Delete non-existent: expected nil, got %v", err)
	}
}

func TestStore_RuntimeFieldsNotPersisted(t *testing.T) {
	store, dir := newTestStore(t)

	s := &Session{
		ID:             "runtime-test",
		Status:         StatusRunning,
		CurrentWorkDir: "/runtime/dir",
		CurrentBranch:  "feature-x",
		IsGitRepo:      true,
		SSHAuthSock:    "/tmp/ssh-agent.sock",
		LastOutputTime: time.Now(),
		StartedAt:      time.Now(),
	}
	if err := store.Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Read raw JSON to verify runtime fields are absent
	data, err := os.ReadFile(filepath.Join(dir, "runtime-test.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	runtimeFields := []string{
		"current_branch", "is_git_repo",
		"ssh_auth_sock", "last_output_time", "started_at",
	}
	for _, field := range runtimeFields {
		if _, found := raw[field]; found {
			t.Errorf("runtime field %q found in persisted JSON", field)
		}
	}
}

// ---------------------------------------------------------------------------
// Save: atomic temp-file + rename
// ---------------------------------------------------------------------------

// TestStore_Save_LeavesNoTempFile pins the invariant that a completed Save
// leaves exactly the record behind, with no temp file to reclaim.
func TestStore_Save_LeavesNoTempFile(t *testing.T) {
	store, dir := newTestStore(t)
	if err := store.Save(&Session{ID: "abc", Description: "x"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "abc.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contents = %v, want [abc.json]", names)
	}
}

// TestStore_Save_NewFileNotWorldReadable locks in the mode new records are
// created with. Session JSON carries work dirs and agent session ids, so it
// must not be readable by other users on a shared host.
func TestStore_Save_NewFileNotWorldReadable(t *testing.T) {
	store, dir := newTestStore(t)
	if err := store.Save(&Session{ID: "abc"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, "abc.json"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != sessionFileMode {
		t.Errorf("mode = %o, want %o", got, sessionFileMode)
	}
}

// TestStore_Save_PreservesExistingMode ensures a re-save does not reset a mode
// the user chose. os.WriteFile left an existing file's mode alone; the
// temp+rename path has to reproduce that.
func TestStore_Save_PreservesExistingMode(t *testing.T) {
	store, dir := newTestStore(t)
	sess := &Session{ID: "abc"}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path := filepath.Join(dir, "abc.json")
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	sess.Description = "updated"
	if err := store.Save(sess); err != nil {
		t.Fatalf("re-Save: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0640 {
		t.Errorf("mode = %o, want 0640 (user-chosen mode must survive a save)", got)
	}
}

// TestStore_Save_ConcurrentWritesStayParseable is the reason Save writes via a
// temp file: several goroutines reach it without a shared lock, and an
// interleaved truncate/write pair would leave a record LoadAll silently drops.
func TestStore_Save_ConcurrentWritesStayParseable(t *testing.T) {
	store, _ := newTestStore(t)

	// Long, differently-sized payloads: a torn write shows up as trailing
	// bytes from the longer record.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			desc := strings.Repeat("d", 1+i*500)
			for j := 0; j < 20; j++ {
				if err := store.Save(&Session{ID: "abc", Description: desc}); err != nil {
					t.Errorf("Save: %v", err)
					return
				}
			}
		}(i)
	}

	// Read concurrently too — a reader must never observe a partial file.
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := store.Load("abc"); err != nil && !os.IsNotExist(err) {
				t.Errorf("Load during concurrent Save: %v", err)
				return
			}
		}
	}()

	wg.Wait()
	close(stop)
	<-readerDone

	if _, err := store.Load("abc"); err != nil {
		t.Fatalf("Load after concurrent saves: %v", err)
	}
}

// TestStore_LoadAll_IgnoresTempFiles pins the coupling between Save's temp
// name and LoadAll's ".json" extension filter: renaming the temp pattern to
// something ending in .json would make LoadAll pick up half-written records.
func TestStore_LoadAll_IgnoresTempFiles(t *testing.T) {
	store, dir := newTestStore(t)
	if err := store.Save(&Session{ID: "abc"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// A temp file left behind mid-write, containing truncated JSON.
	if err := os.WriteFile(filepath.Join(dir, "abc.json.12345.tmp"), []byte(`{"id":"a`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sessions, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "abc" {
		t.Errorf("LoadAll returned %d sessions, want 1 (temp file must be ignored)", len(sessions))
	}
}

// TestStore_NewStore_ReclaimsStaleTempFiles covers the crash case: a Save
// killed between CreateTemp and Rename leaves a temp file nothing else would
// ever remove.
func TestStore_NewStore_ReclaimsStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "abc.json.999.tmp")
	keep := filepath.Join(dir, "abc.json")
	if err := os.WriteFile(stale, []byte("{}"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(keep, []byte(`{"id":"abc"}`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := NewStore(dir); err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale temp file survived NewStore")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("real record was removed: %v", err)
	}
}
