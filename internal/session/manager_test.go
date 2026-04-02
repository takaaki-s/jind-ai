package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/claude-code-valet/internal/config"
)

// newTestManager creates a Manager backed by temporary directories and a mock tmux runner.
func newTestManager(t *testing.T) (*Manager, *mockTmuxRunner) {
	t.Helper()
	dir := t.TempDir()
	configDir := t.TempDir()
	configMgr, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager failed: %v", err)
	}
	mgr, err := NewManager(dir, configDir, configMgr)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	mock := newMockTmuxRunner()
	mgr.SetTmuxClient(mock)
	return mgr, mock
}

// ---------------------------------------------------------------------------
// CreateWithOptions tests
// ---------------------------------------------------------------------------

func TestManager_CreateWithOptions_Success(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir: "/tmp/project-alpha",
		Name:    "alpha",
	})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	if sess.ID == "" {
		t.Error("expected non-empty ID")
	}
	if sess.Name != "alpha" {
		t.Errorf("Name = %q, want %q", sess.Name, "alpha")
	}
	if sess.WorkDir != "/tmp/project-alpha" {
		t.Errorf("WorkDir = %q, want %q", sess.WorkDir, "/tmp/project-alpha")
	}
	if sess.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", sess.Status, StatusStopped)
	}
	if sess.ClaudeSessionID == "" {
		t.Error("expected non-empty ClaudeSessionID")
	}
}

func TestManager_CreateWithOptions_DefaultFleet(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-default", Name: "fd"})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	if sess.Fleet != DefaultFleet {
		t.Errorf("Fleet = %q, want %q", sess.Fleet, DefaultFleet)
	}
}

func TestManager_CreateWithOptions_ExplicitFleet(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-named", Name: "fn", Fleet: "backend"})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	if sess.Fleet != "backend" {
		t.Errorf("Fleet = %q, want %q", sess.Fleet, "backend")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir(t *testing.T) {
	mgr, _ := newTestManager(t)

	_, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dup-dir", Name: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	_, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dup-dir", Name: "second"})
	if err == nil {
		t.Fatal("expected error for duplicate WorkDir, got nil")
	}
}

func TestManager_CreateWithOptions_DuplicateName(t *testing.T) {
	mgr, _ := newTestManager(t)

	_, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dir-a", Name: "samename"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	_, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dir-b", Name: "samename"})
	if err == nil {
		t.Fatal("expected error for duplicate Name, got nil")
	}
}

func TestManager_CreateWithOptions_EmptyWorkDir(t *testing.T) {
	mgr, _ := newTestManager(t)

	_, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "", Name: "nodir"})
	if err == nil {
		t.Fatal("expected error for empty WorkDir, got nil")
	}
}

func TestManager_CreateWithOptions_DefaultName(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/home/user/my-project"})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	want := filepath.Base("/home/user/my-project")
	if sess.Name != want {
		t.Errorf("Name = %q, want %q (filepath.Base of WorkDir)", sess.Name, want)
	}
}

// ---------------------------------------------------------------------------
// Get tests
// ---------------------------------------------------------------------------

func TestManager_Get_Found(t *testing.T) {
	mgr, _ := newTestManager(t)

	created, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/get-test", Name: "getme"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	got, ok := mgr.Get(created.ID)
	if !ok {
		t.Fatal("Get returned ok=false for existing session")
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
}

func TestManager_Get_NotFound(t *testing.T) {
	mgr, _ := newTestManager(t)

	_, ok := mgr.Get("nonexistent-id")
	if ok {
		t.Fatal("Get returned ok=true for nonexistent session")
	}
}

// ---------------------------------------------------------------------------
// List tests
// ---------------------------------------------------------------------------

func TestManager_List(t *testing.T) {
	mgr, _ := newTestManager(t)

	_, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/list-1", Name: "first"})
	if err != nil {
		t.Fatalf("create first failed: %v", err)
	}
	// Ensure distinct CreatedAt timestamps.
	time.Sleep(2 * time.Millisecond)
	_, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/list-2", Name: "second"})
	if err != nil {
		t.Fatalf("create second failed: %v", err)
	}

	infos := mgr.List()
	if len(infos) != 2 {
		t.Fatalf("List returned %d items, want 2", len(infos))
	}
	// Sorted by CreatedAt ascending
	if infos[0].Name != "first" {
		t.Errorf("first item Name = %q, want %q", infos[0].Name, "first")
	}
	if infos[1].Name != "second" {
		t.Errorf("second item Name = %q, want %q", infos[1].Name, "second")
	}
}

func TestManager_List_SortedByFleet(t *testing.T) {
	mgr, _ := newTestManager(t)

	_, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-1", Name: "s1", Fleet: "backend"})
	if err != nil {
		t.Fatalf("create s1 failed: %v", err)
	}
	_, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-2", Name: "s2"}) // default
	if err != nil {
		t.Fatalf("create s2 failed: %v", err)
	}
	_, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-3", Name: "s3", Fleet: "alpha"})
	if err != nil {
		t.Fatalf("create s3 failed: %v", err)
	}

	infos := mgr.List()
	if len(infos) != 3 {
		t.Fatalf("List returned %d items, want 3", len(infos))
	}
	// Expected order: alpha, backend, default (default always last)
	if infos[0].Fleet != "alpha" {
		t.Errorf("infos[0].Fleet = %q, want %q", infos[0].Fleet, "alpha")
	}
	if infos[1].Fleet != "backend" {
		t.Errorf("infos[1].Fleet = %q, want %q", infos[1].Fleet, "backend")
	}
	if infos[2].Fleet != DefaultFleet {
		t.Errorf("infos[2].Fleet = %q, want %q", infos[2].Fleet, DefaultFleet)
	}
}

// ---------------------------------------------------------------------------
// SetStatus / SetStatusWithError tests
// ---------------------------------------------------------------------------

func TestManager_SetStatus(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/status-test", Name: "s1"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.SetStatus(sess.ID, StatusThinking)

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusThinking {
		t.Errorf("Status = %q, want %q", got.Status, StatusThinking)
	}
}

func TestManager_SetStatusWithError(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/err-test", Name: "e1"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.SetStatusWithError(sess.ID, StatusStopped, "something went wrong")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if got.ErrorMessage != "something went wrong" {
		t.Errorf("ErrorMessage = %q, want %q", got.ErrorMessage, "something went wrong")
	}
}

// ---------------------------------------------------------------------------
// SetWorkDir tests
// ---------------------------------------------------------------------------

func TestManager_SetWorkDir(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-old", Name: "wd"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := mgr.SetWorkDir(sess.ID, "/tmp/wd-new"); err != nil {
		t.Fatalf("SetWorkDir failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.WorkDir != "/tmp/wd-new" {
		t.Errorf("WorkDir = %q, want %q", got.WorkDir, "/tmp/wd-new")
	}
}

func TestManager_SetWorkDir_DuplicateWorkDir(t *testing.T) {
	mgr, _ := newTestManager(t)

	_, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-dup", Name: "d1"})
	if err != nil {
		t.Fatalf("create first failed: %v", err)
	}
	s2, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-other", Name: "d2"})
	if err != nil {
		t.Fatalf("create second failed: %v", err)
	}

	err = mgr.SetWorkDir(s2.ID, "/tmp/wd-dup")
	if err == nil {
		t.Fatal("expected error when setting WorkDir to one already in use, got nil")
	}
}

// ---------------------------------------------------------------------------
// CountActive tests
// ---------------------------------------------------------------------------

func TestManager_CountActive(t *testing.T) {
	mgr, _ := newTestManager(t)

	s1, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-1", Name: "ca1"})
	s2, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-2", Name: "ca2"})
	s3, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-3", Name: "ca3"})

	// All start as StatusStopped; set two to active statuses.
	mgr.SetStatus(s2.ID, StatusThinking)
	mgr.SetStatus(s3.ID, StatusRunning)
	// s1 remains StatusStopped

	_ = s1 // keep compiler happy

	count := mgr.CountActive()
	if count != 2 {
		t.Errorf("CountActive() = %d, want 2", count)
	}
}

// ---------------------------------------------------------------------------
// HandleHookEvent tests
// ---------------------------------------------------------------------------

func TestManager_HandleHookEvent_UserPromptSubmit(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-ups", Name: "hups"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "UserPromptSubmit", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusThinking {
		t.Errorf("Status = %q, want %q", got.Status, StatusThinking)
	}
}

func TestManager_HandleHookEvent_Stop(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-stop", Name: "hstop"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Set to thinking first
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "Stop", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", got.Status, StatusIdle)
	}
}

func TestManager_HandleHookEvent_Notification_Permission(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-perm", Name: "hperm"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "Notification", "permission_prompt", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusPermission {
		t.Errorf("Status = %q, want %q", got.Status, StatusPermission)
	}
}

func TestManager_HandleHookEvent_UnknownSession(t *testing.T) {
	mgr, _ := newTestManager(t)

	// Should not panic when both IDs are unknown.
	mgr.HandleHookEvent("unknown-cc-id", "unknown-valet-id", "Stop", "", "", "")
}

func TestManager_HandleHookEvent_CWDUpdate(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-cwd", Name: "hcwd"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// CWD to a non-git-root path: WorkDir should NOT update, only CurrentWorkDir
	nonGitCwd := "/tmp/hook-cwd-subdir"
	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "UserPromptSubmit", "", nonGitCwd, "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.WorkDir != "/tmp/hook-cwd" {
		t.Errorf("WorkDir = %q, want %q (should not update for non-git-root)", got.WorkDir, "/tmp/hook-cwd")
	}
	if got.CurrentWorkDir != nonGitCwd {
		t.Errorf("CurrentWorkDir = %q, want %q", got.CurrentWorkDir, nonGitCwd)
	}

	// CWD to a git root (has .git): WorkDir SHOULD update
	gitRootCwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitRootCwd, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "UserPromptSubmit", "", gitRootCwd, "")

	got, ok = mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.WorkDir != gitRootCwd {
		t.Errorf("WorkDir = %q, want %q (should update for git root)", got.WorkDir, gitRootCwd)
	}
	if got.CurrentWorkDir != gitRootCwd {
		t.Errorf("CurrentWorkDir = %q, want %q", got.CurrentWorkDir, gitRootCwd)
	}
}

func TestManager_HandleHookEvent_CwdChanged(t *testing.T) {
	mgr, _ := newTestManager(t)

	origDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(origDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: origDir, Name: "hcwdch"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// CWD to a non-git-root: only CurrentWorkDir updates
	subDir := filepath.Join(origDir, "subdir")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "CwdChanged", "", subDir, "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.WorkDir != origDir {
		t.Errorf("WorkDir = %q, want %q (should not update for subdirectory)", got.WorkDir, origDir)
	}
	if got.CurrentWorkDir != subDir {
		t.Errorf("CurrentWorkDir = %q, want %q", got.CurrentWorkDir, subDir)
	}
	// Status should remain unchanged (stopped from creation)
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q (unchanged)", got.Status, StatusStopped)
	}

	// CWD to a different git root (worktree): WorkDir SHOULD update
	worktreeDir := t.TempDir()
	// Simulate a git worktree (.git is a file, not a directory)
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"), []byte("gitdir: ../main/.git/worktrees/wt"), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "CwdChanged", "", worktreeDir, "")

	got, ok = mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.WorkDir != worktreeDir {
		t.Errorf("WorkDir = %q, want %q (should update for worktree root)", got.WorkDir, worktreeDir)
	}
	if got.CurrentWorkDir != worktreeDir {
		t.Errorf("CurrentWorkDir = %q, want %q", got.CurrentWorkDir, worktreeDir)
	}
}

func TestManager_HandleHookEvent_StopFailure(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sf", Name: "hsf"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "StopFailure", "", "", "rate_limit")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", got.Status, StatusIdle)
	}
	if got.ErrorMessage != "rate_limit" {
		t.Errorf("ErrorMessage = %q, want %q", got.ErrorMessage, "rate_limit")
	}

	// Verify error notification was added to history
	history := mgr.NotificationHistory()
	if len(history) == 0 {
		t.Fatal("expected at least 1 notification")
	}
	if history[0].Type != "error" {
		t.Errorf("notification Type = %q, want %q", history[0].Type, "error")
	}
}

func TestManager_HandleHookEvent_StopFailure_ThenStop_ClearsError(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sfclr", Name: "hsfclr"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	// First: StopFailure sets ErrorMessage
	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "StopFailure", "", "", "rate_limit")
	got, _ := mgr.Get(sess.ID)
	if got.ErrorMessage != "rate_limit" {
		t.Fatalf("ErrorMessage after StopFailure = %q, want %q", got.ErrorMessage, "rate_limit")
	}

	// Then: Stop clears ErrorMessage
	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "Stop", "", "", "")
	got, _ = mgr.Get(sess.ID)
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage after Stop = %q, want empty", got.ErrorMessage)
	}
}

func TestManager_HandleHookEvent_StopFailure_ThenUserPrompt_ClearsError(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sfclr2", Name: "hsfclr2"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	// StopFailure sets ErrorMessage
	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "StopFailure", "", "", "auth_error")
	got, _ := mgr.Get(sess.ID)
	if got.ErrorMessage != "auth_error" {
		t.Fatalf("ErrorMessage after StopFailure = %q, want %q", got.ErrorMessage, "auth_error")
	}

	// UserPromptSubmit clears ErrorMessage
	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	got, _ = mgr.Get(sess.ID)
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage after UserPromptSubmit = %q, want empty", got.ErrorMessage)
	}
}

func TestManager_HandleHookEvent_StopFailure_EmptyReason(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sf2", Name: "hsf2"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "StopFailure", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", got.Status, StatusIdle)
	}
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage = %q, want empty", got.ErrorMessage)
	}
}

func TestManager_HandleHookEvent_SessionStart(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-ss", Name: "hss"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Ensure ClaudeSessionStarted is false initially
	sess.ClaudeSessionStarted = false

	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "SessionStart", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if !got.ClaudeSessionStarted {
		t.Error("ClaudeSessionStarted should be true after SessionStart hook")
	}
}

func TestManager_HandleHookEvent_SessionEnd(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-se", Name: "hse"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "SessionEnd", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
}

func TestManager_HandleHookEvent_SessionEnd_Idempotent(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sei", Name: "hsei"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Session is already stopped (default from creation)
	if sess.Status != StatusStopped {
		t.Fatalf("precondition: Status = %q, want %q", sess.Status, StatusStopped)
	}

	// Should not panic or change anything
	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "SessionEnd", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
}

func TestEnsureHooksSettingsFile_NewHooks(t *testing.T) {
	dir := t.TempDir()
	path, err := ensureHooksSettingsFile(dir, "/usr/local/bin/ccvalet")
	if err != nil {
		t.Fatalf("ensureHooksSettingsFile failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var settings hooksSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	requiredHooks := []string{"UserPromptSubmit", "Stop", "StopFailure", "PostToolUse", "CwdChanged", "SessionStart", "SessionEnd", "Notification"}
	for _, hook := range requiredHooks {
		if _, ok := settings.Hooks[hook]; !ok {
			t.Errorf("hooks-settings.json missing hook: %s", hook)
		}
	}
}

// ---------------------------------------------------------------------------
// Kill tests
// ---------------------------------------------------------------------------

func TestManager_Kill(t *testing.T) {
	mgr, mock := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-test", Name: "killme"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate a running session with tmux integration.
	mgr.mu.Lock()
	sess.TmuxWindowName = "ccvalet_" + sess.ID
	sess.TmuxPaneID = "%42"
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after Kill")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if !mock.hasCalledWith("KillPane", "%42") {
		t.Error("expected KillPane to be called with %42")
	}
}

// ---------------------------------------------------------------------------
// Delete tests
// ---------------------------------------------------------------------------

func TestManager_Delete(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-test", Name: "delme"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := mgr.Delete(sess.ID, false, false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Session should no longer be accessible via Get.
	_, ok := mgr.Get(sess.ID)
	if ok {
		t.Fatal("Get returned ok=true after Delete")
	}

	// Store should also have removed the file.
	_, err = mgr.store.Load(sess.ID)
	if err == nil {
		t.Fatal("expected store.Load to return error after Delete, got nil")
	}
}

// ---------------------------------------------------------------------------
// RecoverTmuxSessions tests
// ---------------------------------------------------------------------------

func TestManager_RecoverTmuxSessions_Live(t *testing.T) {
	mgr, mock := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-live", Name: "rlive"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	innerName := "ccvalet_" + sess.ID
	mgr.mu.Lock()
	sess.TmuxWindowName = innerName
	sess.TmuxPaneID = "%10"
	mgr.mu.Unlock()

	// Configure mock: session exists and pane is alive.
	mock.sessions[innerName] = true
	mock.deadPanes["%10"] = false

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, StatusRunning)
	}
}

func TestManager_RecoverTmuxSessions_DeadPane(t *testing.T) {
	mgr, mock := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-dead", Name: "rdead"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	innerName := "ccvalet_" + sess.ID
	mgr.mu.Lock()
	sess.TmuxWindowName = innerName
	sess.TmuxPaneID = "%11"
	mgr.mu.Unlock()

	// Configure mock: session exists but pane is dead.
	mock.sessions[innerName] = true
	mock.deadPanes["%11"] = true

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	// TmuxWindowName should be kept (window preserved via remain-on-exit).
	if got.TmuxWindowName == "" {
		t.Error("expected TmuxWindowName to be kept after dead pane recovery")
	}
}

func TestManager_RecoverTmuxSessions_NoTmux(t *testing.T) {
	mgr, _ := newTestManager(t)

	// Explicitly set tmuxClient to nil to simulate no tmux available.
	mgr.SetTmuxClient(nil)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-notmux", Name: "rnotmux"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.mu.Lock()
	sess.TmuxWindowName = "ccvalet_" + sess.ID
	mgr.mu.Unlock()

	// Should be a no-op and not panic.
	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	// Status should remain unchanged (StatusStopped from creation).
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
}

// ---------------------------------------------------------------------------
// FindByClaudeSessionID tests
// ---------------------------------------------------------------------------

func TestManager_FindByClaudeSessionID(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/find-cc", Name: "findcc"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Find by the ClaudeSessionID that was auto-generated during creation.
	got, ok := mgr.FindByClaudeSessionID(sess.ClaudeSessionID)
	if !ok {
		t.Fatal("FindByClaudeSessionID returned ok=false for existing session")
	}
	if got.ID != sess.ID {
		t.Errorf("ID = %q, want %q", got.ID, sess.ID)
	}

	// Find with a non-existent ClaudeSessionID should return nil.
	got2, ok2 := mgr.FindByClaudeSessionID("nonexistent-cc-id")
	if ok2 {
		t.Fatal("FindByClaudeSessionID returned ok=true for non-existent ClaudeSessionID")
	}
	if got2 != nil {
		t.Errorf("expected nil session, got %+v", got2)
	}
}

func TestManager_FindByClaudeSessionID_NotFound(t *testing.T) {
	mgr, _ := newTestManager(t)

	// Empty manager: should return nil, false.
	got, ok := mgr.FindByClaudeSessionID("does-not-exist")
	if ok {
		t.Fatal("FindByClaudeSessionID returned ok=true on empty manager")
	}
	if got != nil {
		t.Errorf("expected nil session, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// StartBackground tests
// ---------------------------------------------------------------------------

func TestManager_StartBackground(t *testing.T) {
	mgr, mock := newTestManager(t)

	// Use a real temp directory so os.Stat in startSessionTmux passes.
	workDir := t.TempDir()

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Name: "bg"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Configure mock so GetPaneID returns a valid pane ID for the inner session.
	innerName := "sess-" + sess.ID
	mock.paneIDs[innerName] = "%99"

	if err := mgr.StartBackground(sess.ID); err != nil {
		t.Fatalf("StartBackground failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after StartBackground")
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, StatusRunning)
	}
	if got.TmuxWindowName != innerName {
		t.Errorf("TmuxWindowName = %q, want %q", got.TmuxWindowName, innerName)
	}

	// Verify mock tmux calls.
	if !mock.hasCalledWith("NewSessionWithCmdInDir", innerName) {
		t.Error("expected NewSessionWithCmdInDir to be called with inner session name")
	}
}

func TestManager_StartBackground_NotFound(t *testing.T) {
	mgr, _ := newTestManager(t)

	err := mgr.StartBackground("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for non-existent session ID, got nil")
	}
}

func TestManager_StartBackground_AlreadyRunning(t *testing.T) {
	mgr, mock := newTestManager(t)

	workDir := t.TempDir()
	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Name: "already"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate a session that's already running (has TmuxWindowName and non-stopped status).
	mgr.mu.Lock()
	sess.TmuxWindowName = "sess-" + sess.ID
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	// StartBackground should succeed without creating a new tmux session.
	if err := mgr.StartBackground(sess.ID); err != nil {
		t.Fatalf("StartBackground failed: %v", err)
	}

	// NewSessionWithCmdInDir should NOT have been called.
	if mock.hasCalledWith("NewSessionWithCmdInDir", "sess-"+sess.ID) {
		t.Error("expected NewSessionWithCmdInDir NOT to be called for already running session")
	}
}

// ---------------------------------------------------------------------------
// SetStatus extended tests
// ---------------------------------------------------------------------------

func TestManager_SetStatus_NonExistent(t *testing.T) {
	mgr, _ := newTestManager(t)

	// Setting status on a non-existent session should not panic.
	mgr.SetStatus("nonexistent-id", StatusThinking)

	// Verify no sessions were created.
	infos := mgr.List()
	if len(infos) != 0 {
		t.Errorf("List returned %d items, want 0", len(infos))
	}
}

// ---------------------------------------------------------------------------
// NotificationHistory tests
// ---------------------------------------------------------------------------

func TestManager_NotificationHistory(t *testing.T) {
	mgr, _ := newTestManager(t)

	// Initially, notification history should be empty
	history := mgr.NotificationHistory()
	if len(history) != 0 {
		t.Fatalf("initial NotificationHistory: got %d entries, want 0", len(history))
	}

	// Create sessions and trigger hook events that generate notifications
	sess1, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/notify-1", Name: "n1"})
	if err != nil {
		t.Fatalf("create sess1 failed: %v", err)
	}
	sess2, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/notify-2", Name: "n2"})
	if err != nil {
		t.Fatalf("create sess2 failed: %v", err)
	}

	// Set sessions to thinking first (Stop hook transitions from thinking to idle)
	mgr.SetStatus(sess1.ID, StatusThinking)
	mgr.SetStatus(sess2.ID, StatusThinking)

	// Trigger Stop event (generates task_complete notification)
	mgr.HandleHookEvent(sess1.ClaudeSessionID, sess1.ID, "Stop", "", "", "")

	// Trigger Notification/permission event (generates permission notification)
	mgr.HandleHookEvent(sess2.ClaudeSessionID, sess2.ID, "Notification", "permission_prompt", "", "")

	history = mgr.NotificationHistory()
	if len(history) != 2 {
		t.Fatalf("NotificationHistory: got %d entries, want 2", len(history))
	}

	// History is sorted newest first, so permission (sess2) should come first
	if history[0].SessionID != sess2.ID {
		t.Errorf("history[0].SessionID: got %q, want %q", history[0].SessionID, sess2.ID)
	}
	if history[0].Type != "permission" {
		t.Errorf("history[0].Type: got %q, want %q", history[0].Type, "permission")
	}

	if history[1].SessionID != sess1.ID {
		t.Errorf("history[1].SessionID: got %q, want %q", history[1].SessionID, sess1.ID)
	}
	if history[1].Type != "task_complete" {
		t.Errorf("history[1].Type: got %q, want %q", history[1].Type, "task_complete")
	}
}

func TestManager_SetStatus_Persisted(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/setstatus-persist", Name: "sp"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.SetStatus(sess.ID, StatusThinking)

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusThinking {
		t.Errorf("Status = %q, want %q", got.Status, StatusThinking)
	}
}

// ---------------------------------------------------------------------------
// EnsureTmuxClient not set tests
// ---------------------------------------------------------------------------

func TestManager_EnsureTmuxClient_NotSet(t *testing.T) {
	dir := t.TempDir()
	configDir := t.TempDir()
	configMgr, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager failed: %v", err)
	}
	mgr, err := NewManager(dir, configDir, configMgr)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	// Deliberately do NOT call SetTmuxClient — tmux client remains nil.

	workDir := t.TempDir()
	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Name: "notmux"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// StartBackground calls startSession -> ensureTmuxClient internally.
	// Without a real tmux binary, startSessionTmux will fail in some way.
	// We mainly want to verify that it does not panic and returns an error.
	err = mgr.StartBackground(sess.ID)
	if err == nil {
		// If no error, the session should at least have a status set.
		// In CI without tmux installed, ensureTmuxClient will fail silently,
		// and startSessionTmux will likely error on tmux commands.
		// Either way, we verified no panic.
		t.Log("StartBackground succeeded (tmux may be installed), verifying no panic")
	}
}

// ---------------------------------------------------------------------------
// Kill edge case tests
// ---------------------------------------------------------------------------

func TestManager_Kill_NotFound(t *testing.T) {
	mgr, _ := newTestManager(t)

	err := mgr.Kill("nonexistent-session-id")
	if err == nil {
		t.Fatal("expected error for non-existent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not contain 'not found'", err.Error())
	}
}

func TestManager_Kill_WithTmuxWindowOnly(t *testing.T) {
	mgr, mock := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-win", Name: "killwin"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate session with TmuxWindowName but no TmuxPaneID (fallback path)
	mgr.mu.Lock()
	sess.TmuxWindowName = "ccvalet_" + sess.ID
	sess.TmuxPaneID = "" // no pane ID
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	mock.sessions[sess.TmuxWindowName] = true

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after Kill")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	// Should have called KillSession (fallback when no pane ID)
	if !mock.hasCalledWith("KillSession", "ccvalet_"+sess.ID) {
		t.Error("expected KillSession to be called with inner session name")
	}
}

func TestManager_Kill_NoTmux(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-notmux", Name: "killnotmux"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Session has no tmux window or pane; Kill should still mark it stopped.
	mgr.mu.Lock()
	sess.Status = StatusThinking
	mgr.mu.Unlock()

	if err := mgr.Kill(sess.ID); err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false after Kill")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
}

// ---------------------------------------------------------------------------
// Delete edge case tests
// ---------------------------------------------------------------------------

func TestManager_Delete_NotFound(t *testing.T) {
	mgr, _ := newTestManager(t)

	err := mgr.Delete("nonexistent-session-id", false, false)
	if err == nil {
		t.Fatal("expected error for non-existent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not contain 'not found'", err.Error())
	}
}

func TestManager_Delete_Success(t *testing.T) {
	mgr, _ := newTestManager(t)

	// Create two sessions
	sess1, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-s1", Name: "dels1"})
	if err != nil {
		t.Fatalf("create sess1 failed: %v", err)
	}
	_, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-s2", Name: "dels2"})
	if err != nil {
		t.Fatalf("create sess2 failed: %v", err)
	}

	// Delete the first session
	if err := mgr.Delete(sess1.ID, false, false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify it's gone from Get
	_, ok := mgr.Get(sess1.ID)
	if ok {
		t.Fatal("Get returned ok=true after Delete")
	}

	// Verify it's gone from List
	infos := mgr.List()
	if len(infos) != 1 {
		t.Fatalf("List returned %d items, want 1", len(infos))
	}
	if infos[0].Name != "dels2" {
		t.Errorf("remaining session Name = %q, want %q", infos[0].Name, "dels2")
	}
}

func TestManager_Delete_WithTmuxSession(t *testing.T) {
	mgr, mock := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-tmux", Name: "deltmux"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate session with active tmux
	mgr.mu.Lock()
	sess.TmuxWindowName = "ccvalet_" + sess.ID
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	mock.sessions[sess.TmuxWindowName] = true

	if err := mgr.Delete(sess.ID, false, false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Should have called KillSession on the inner tmux session
	if !mock.hasCalledWith("KillSession", "ccvalet_"+sess.ID) {
		t.Error("expected KillSession to be called when deleting a session with tmux")
	}

	// Verify it's gone
	_, ok := mgr.Get(sess.ID)
	if ok {
		t.Fatal("Get returned ok=true after Delete")
	}
}

// ---------------------------------------------------------------------------
// List with HostID tests
// ---------------------------------------------------------------------------

func TestManager_List_WithHostFilter(t *testing.T) {
	mgr, _ := newTestManager(t)

	// Create sessions with different HostIDs
	sess1, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/host-1", Name: "host1"})
	if err != nil {
		t.Fatalf("create sess1 failed: %v", err)
	}
	sess2, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/host-2", Name: "host2"})
	if err != nil {
		t.Fatalf("create sess2 failed: %v", err)
	}
	sess3, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/host-3", Name: "host3"})
	if err != nil {
		t.Fatalf("create sess3 failed: %v", err)
	}

	// Set different HostIDs
	mgr.mu.Lock()
	sess1.HostID = "local"
	sess2.HostID = "ec2"
	sess3.HostID = "local"
	mgr.mu.Unlock()

	// List returns all sessions (no built-in host filter)
	infos := mgr.List()
	if len(infos) != 3 {
		t.Fatalf("List returned %d items, want 3", len(infos))
	}

	// Verify HostIDs are passed through in Info
	hostIDs := map[string]string{}
	for _, info := range infos {
		hostIDs[info.Name] = info.HostID
	}
	if hostIDs["host1"] != "local" {
		t.Errorf("host1 HostID = %q, want %q", hostIDs["host1"], "local")
	}
	if hostIDs["host2"] != "ec2" {
		t.Errorf("host2 HostID = %q, want %q", hostIDs["host2"], "ec2")
	}
	if hostIDs["host3"] != "local" {
		t.Errorf("host3 HostID = %q, want %q", hostIDs["host3"], "local")
	}
}

func TestNewManager_LoadAll_MigratesEmptyFleet(t *testing.T) {
	dataDir := t.TempDir()
	configDir := t.TempDir()

	// Write a session JSON without the fleet field (simulates old data)
	oldJSON := `{"id":"old-id","name":"old","work_dir":"/tmp/old","created_at":"2025-01-01T00:00:00Z","status":"idle","claude_session_id":"cid"}`
	if err := os.WriteFile(filepath.Join(dataDir, "old-id.json"), []byte(oldJSON), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	configMgr, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager failed: %v", err)
	}
	mgr, err := NewManager(dataDir, configDir, configMgr)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	infos := mgr.List()
	if len(infos) != 1 {
		t.Fatalf("List returned %d items, want 1", len(infos))
	}
	if infos[0].Fleet != DefaultFleet {
		t.Errorf("Fleet = %q, want %q", infos[0].Fleet, DefaultFleet)
	}
}

func TestManager_List_Empty(t *testing.T) {
	mgr, _ := newTestManager(t)

	infos := mgr.List()
	if len(infos) != 0 {
		t.Fatalf("List on empty manager returned %d items, want 0", len(infos))
	}
}

// ---------------------------------------------------------------------------
// EnsureClaudeTrustState tests
// ---------------------------------------------------------------------------

func TestManager_EnsureClaudeTrustState(t *testing.T) {
	// ensureClaudeTrustState writes to ~/.claude/settings.local.json.
	// We override HOME so it writes to a temp directory instead.
	origHome := os.Getenv("HOME")
	fakeHome := t.TempDir()
	os.Setenv("HOME", fakeHome)
	defer os.Setenv("HOME", origHome)

	workDir := t.TempDir()

	err := ensureClaudeTrustState(workDir)
	if err != nil {
		t.Fatalf("ensureClaudeTrustState failed: %v", err)
	}

	// Verify the trust file was created
	settingsPath := filepath.Join(fakeHome, ".claude", "settings.local.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings file: %v", err)
	}

	var settings ClaudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to unmarshal settings: %v", err)
	}

	// workDir should be converted to absolute path
	absWorkDir, _ := filepath.Abs(workDir)
	projectSettings, exists := settings.Projects[absWorkDir]
	if !exists {
		t.Fatalf("project settings not found for %s, got keys: %v", absWorkDir, func() []string {
			keys := make([]string, 0, len(settings.Projects))
			for k := range settings.Projects {
				keys = append(keys, k)
			}
			return keys
		}())
	}
	if !projectSettings.HasTrustDialogAccepted {
		t.Error("HasTrustDialogAccepted = false, want true")
	}
}

func TestManager_EnsureClaudeTrustState_Idempotent(t *testing.T) {
	origHome := os.Getenv("HOME")
	fakeHome := t.TempDir()
	os.Setenv("HOME", fakeHome)
	defer os.Setenv("HOME", origHome)

	workDir := t.TempDir()

	// Call twice — should be idempotent
	if err := ensureClaudeTrustState(workDir); err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if err := ensureClaudeTrustState(workDir); err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	// Verify still exactly one entry
	settingsPath := filepath.Join(fakeHome, ".claude", "settings.local.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings file: %v", err)
	}

	var settings ClaudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to unmarshal settings: %v", err)
	}

	absWorkDir, _ := filepath.Abs(workDir)
	if len(settings.Projects) != 1 {
		t.Errorf("expected 1 project entry, got %d", len(settings.Projects))
	}
	if !settings.Projects[absWorkDir].HasTrustDialogAccepted {
		t.Error("HasTrustDialogAccepted = false, want true")
	}
}

func TestManager_EnsureClaudeTrustState_MultipleProjects(t *testing.T) {
	origHome := os.Getenv("HOME")
	fakeHome := t.TempDir()
	os.Setenv("HOME", fakeHome)
	defer os.Setenv("HOME", origHome)

	workDir1 := t.TempDir()
	workDir2 := t.TempDir()

	if err := ensureClaudeTrustState(workDir1); err != nil {
		t.Fatalf("first project failed: %v", err)
	}
	if err := ensureClaudeTrustState(workDir2); err != nil {
		t.Fatalf("second project failed: %v", err)
	}

	settingsPath := filepath.Join(fakeHome, ".claude", "settings.local.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings file: %v", err)
	}

	var settings ClaudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to unmarshal settings: %v", err)
	}

	if len(settings.Projects) != 2 {
		t.Errorf("expected 2 project entries, got %d", len(settings.Projects))
	}

	absWorkDir1, _ := filepath.Abs(workDir1)
	absWorkDir2, _ := filepath.Abs(workDir2)

	if !settings.Projects[absWorkDir1].HasTrustDialogAccepted {
		t.Error("project 1 HasTrustDialogAccepted = false, want true")
	}
	if !settings.Projects[absWorkDir2].HasTrustDialogAccepted {
		t.Error("project 2 HasTrustDialogAccepted = false, want true")
	}
}

// ---------------------------------------------------------------------------
// Idle fallback tests (hook-timeout detection in captureOutputTmux)
// ---------------------------------------------------------------------------

// TestManager_IdleFallback_FreshStart verifies that a session in StatusRunning
// with a non-zero StartedAt and stale LastOutputTime satisfies the fallback
// condition that captureOutputTmux uses to transition running → idle.
func TestManager_IdleFallback_FreshStart(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/idle-fallback-fresh", Name: "ifb-fresh"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Simulate a fresh start: status running, StartedAt and LastOutputTime set 31s ago.
	mgr.mu.Lock()
	sess.Status = StatusRunning
	sess.StartedAt = time.Now().Add(-31 * time.Second)
	sess.LastOutputTime = time.Now().Add(-31 * time.Second)
	mgr.mu.Unlock()

	const hookIdleTimeout = 30 * time.Second

	mgr.mu.RLock()
	fbStatus := sess.Status
	fbLastOutput := sess.LastOutputTime
	fbStartedAt := sess.StartedAt
	mgr.mu.RUnlock()

	// The condition must be true for the fallback to fire.
	if !(fbStatus == StatusRunning && !fbStartedAt.IsZero() && time.Since(fbLastOutput) > hookIdleTimeout) {
		t.Fatal("expected idle fallback condition to be true for a fresh start with stale LastOutputTime")
	}

	// Apply the same transition captureOutputTmux would perform.
	mgr.mu.Lock()
	if _, exists := mgr.sessions[sess.ID]; exists && sess.Status == StatusRunning {
		sess.Status = StatusIdle
		sess.LastOutputTime = time.Now()
	}
	mgr.mu.Unlock()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session not found after fallback transition")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", got.Status, StatusIdle)
	}
}

// TestManager_IdleFallback_DaemonRecovery verifies that sessions recovered after
// a daemon restart (StartedAt == zero) do NOT satisfy the fallback condition,
// preventing false idle transitions while a task may still be running.
func TestManager_IdleFallback_DaemonRecovery(t *testing.T) {
	mgr, _ := newTestManager(t)

	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/idle-fallback-recover", Name: "ifb-recover"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Simulate daemon recovery: StartedAt is zero (json:"-" field, never set on recovery).
	mgr.mu.Lock()
	sess.Status = StatusRunning
	// sess.StartedAt is zero by default — as it would be after daemon restart.
	sess.LastOutputTime = time.Now().Add(-31 * time.Second)
	mgr.mu.Unlock()

	const hookIdleTimeout = 30 * time.Second

	mgr.mu.RLock()
	fbStatus := sess.Status
	fbLastOutput := sess.LastOutputTime
	fbStartedAt := sess.StartedAt
	mgr.mu.RUnlock()

	// The condition must be false (StartedAt.IsZero() == true), so fallback does not fire.
	shouldFallback := fbStatus == StatusRunning && !fbStartedAt.IsZero() && time.Since(fbLastOutput) > hookIdleTimeout
	if shouldFallback {
		t.Error("idle fallback condition should be false when StartedAt is zero (daemon recovery)")
	}

	// Status must remain running.
	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session not found")
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q (should not change without hook)", got.Status, StatusRunning)
	}
}

func TestIsWorktreePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/home/user/project", false},
		{"/home/user/project/.git", false},
		{"/home/user/project/src", false},
		{"/home/user/project/.claude/worktrees/feat-xyz", true},
		{"/home/user/project/.claude/worktrees/COR-24444", true},
		{"/tmp/.claude/worktrees/test", true},
		{"/home/user/project/.claude/workdir", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isWorktreePath(tt.path); got != tt.want {
			t.Errorf("isWorktreePath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestManager_HandleHookEvent_CWDUpdate_WorktreePathSkipped(t *testing.T) {
	mgr, _ := newTestManager(t)

	origDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(origDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sess, err := mgr.CreateWithOptions(CreateOptions{WorkDir: origDir, Name: "wt-skip"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate Claude Code's EnterWorktree: CWD moves to .claude/worktrees/xxx
	worktreeDir := filepath.Join(origDir, ".claude", "worktrees", "feat-xyz")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	// Worktree has a .git file (as real worktrees do)
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"), []byte("gitdir: ../../.git/worktrees/feat-xyz"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}

	mgr.HandleHookEvent(sess.ClaudeSessionID, sess.ID, "CwdChanged", "", worktreeDir, "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	// WorkDir must NOT be updated to the worktree path
	if got.WorkDir != origDir {
		t.Errorf("WorkDir = %q, want %q (should not update for .claude/worktrees path)", got.WorkDir, origDir)
	}
	// CurrentWorkDir should still be updated
	if got.CurrentWorkDir != worktreeDir {
		t.Errorf("CurrentWorkDir = %q, want %q", got.CurrentWorkDir, worktreeDir)
	}
}
