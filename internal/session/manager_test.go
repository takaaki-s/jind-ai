package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/takaaki-s/honjin/internal/config"
	"github.com/takaaki-s/honjin/internal/git"
)

// newTestManager creates a Manager backed by temporary directories, a mock
// tmux runner, and a mock hook runner. Both mocks are pre-wired via their
// setters; tests that don't care about the hook runner discard it with `_`.
func newTestManager(t *testing.T) (*Manager, *mockTmuxRunner, *mockHookRunner) {
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
	tmuxMock := newMockTmuxRunner()
	mgr.SetTmuxClient(tmuxMock)
	hookMock := newMockHookRunner()
	mgr.SetHookRunner(hookMock)
	return mgr, tmuxMock, hookMock
}

// ---------------------------------------------------------------------------
// CreateWithOptions tests
// ---------------------------------------------------------------------------

func TestManager_CreateWithOptions_Success(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-default", Name: "fd"})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	if sess.Fleet != DefaultFleet {
		t.Errorf("Fleet = %q, want %q", sess.Fleet, DefaultFleet)
	}
}

func TestManager_CreateWithOptions_ExplicitFleet(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-named", Name: "fn", Fleet: "backend"})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	if sess.Fleet != "backend" {
		t.Errorf("Fleet = %q, want %q", sess.Fleet, "backend")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dup-dir", Name: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dup-dir", Name: "second"})
	if err == nil {
		t.Fatal("expected error for duplicate WorkDir, got nil")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_SkipWorktreeSession(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Name: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	// Simulate the session moving into a worktree (CurrentWorkDir updated by daemon polling)
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/repo/.claude/worktrees/some-branch"
	mgr.mu.Unlock()

	// Creating a new session for the same WorkDir should succeed
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Name: "second"})
	if err != nil {
		t.Fatalf("expected success when existing session is in worktree, got: %v", err)
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_BlockNonWorktree(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Name: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	// Set CurrentWorkDir to repo root (not a worktree) — duplicate check should still block
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/repo"
	mgr.mu.Unlock()

	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Name: "second"})
	if err == nil {
		t.Fatal("expected error for duplicate WorkDir when session is not in worktree")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_StoppedSession(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// CurrentWorkDir defaults to "" for a freshly created (stopped) session
	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Name: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Name: "second"})
	if err == nil {
		t.Fatal("stopped session (CurrentWorkDir empty) should still block duplicate WorkDir")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_ReturnFromWorktree(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Name: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	// Session enters a worktree
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/repo/.claude/worktrees/some-branch"
	mgr.mu.Unlock()

	// Session exits worktree, CurrentWorkDir returns to repo root
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/repo"
	mgr.mu.Unlock()

	// Duplicate check should block again
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Name: "second"})
	if err == nil {
		t.Fatal("expected error after session returned from worktree to original WorkDir")
	}
}

func TestManager_CreateWithOptions_DuplicateName(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dir-a", Name: "samename"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dir-b", Name: "samename"})
	if err == nil {
		t.Fatal("expected error for duplicate Name, got nil")
	}
}

func TestManager_CreateWithOptions_EmptyWorkDir(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "", Name: "nodir"})
	if err == nil {
		t.Fatal("expected error for empty WorkDir, got nil")
	}
}

func TestManager_CreateWithOptions_DefaultName(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/home/user/my-project"})
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
	mgr, _, _ := newTestManager(t)

	created, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/get-test", Name: "getme"})
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
	mgr, _, _ := newTestManager(t)

	_, ok := mgr.Get("nonexistent-id")
	if ok {
		t.Fatal("Get returned ok=true for nonexistent session")
	}
}

// ---------------------------------------------------------------------------
// List tests
// ---------------------------------------------------------------------------

func TestManager_List(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/list-1", Name: "first"})
	if err != nil {
		t.Fatalf("create first failed: %v", err)
	}
	// Ensure distinct CreatedAt timestamps.
	time.Sleep(2 * time.Millisecond)
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/list-2", Name: "second"})
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
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-1", Name: "s1", Fleet: "backend"})
	if err != nil {
		t.Fatalf("create s1 failed: %v", err)
	}
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-2", Name: "s2"}) // default
	if err != nil {
		t.Fatalf("create s2 failed: %v", err)
	}
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-3", Name: "s3", Fleet: "alpha"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/status-test", Name: "s1"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/err-test", Name: "e1"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-old", Name: "wd"})
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
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-dup", Name: "d1"})
	if err != nil {
		t.Fatalf("create first failed: %v", err)
	}
	s2, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-other", Name: "d2"})
	if err != nil {
		t.Fatalf("create second failed: %v", err)
	}

	err = mgr.SetWorkDir(s2.ID, "/tmp/wd-dup")
	if err == nil {
		t.Fatal("expected error when setting WorkDir to one already in use, got nil")
	}
}

func TestManager_SetWorkDir_DuplicateWorkDir_SkipWorktreeSession(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-dup", Name: "d1"})
	if err != nil {
		t.Fatalf("create first failed: %v", err)
	}
	s2, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-other", Name: "d2"})
	if err != nil {
		t.Fatalf("create second failed: %v", err)
	}

	// s1 is in a worktree — SetWorkDir should succeed
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/wd-dup/.claude/worktrees/some-branch"
	mgr.mu.Unlock()

	err = mgr.SetWorkDir(s2.ID, "/tmp/wd-dup")
	if err != nil {
		t.Fatalf("expected success when conflicting session is in worktree, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CountActive tests
// ---------------------------------------------------------------------------

func TestManager_CountActive(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-1", Name: "ca1"})
	s2, _, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-2", Name: "ca2"})
	s3, _, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-3", Name: "ca3"})

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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-ups", Name: "hups"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-stop", Name: "hstop"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-perm", Name: "hperm"})
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
	mgr, _, _ := newTestManager(t)

	// Should not panic when both IDs are unknown.
	mgr.HandleHookEvent("unknown-cc-id", "unknown-jin-id", "Stop", "", "", "")
}

func TestManager_HandleHookEvent_CWDUpdate(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-cwd", Name: "hcwd"})
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
	mgr, _, _ := newTestManager(t)

	origDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(origDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: origDir, Name: "hcwdch"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sf", Name: "hsf"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sfclr", Name: "hsfclr"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sfclr2", Name: "hsfclr2"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sf2", Name: "hsf2"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-ss", Name: "hss"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-se", Name: "hse"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sei", Name: "hsei"})
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
	path, err := ensureHooksSettingsFile(dir, "/usr/local/bin/jin")
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
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-test", Name: "killme"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate a running session with tmux integration.
	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-test", Name: "delme"})
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
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-live", Name: "rlive"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	innerName := "jin_" + sess.ID
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
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-dead", Name: "rdead"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	innerName := "jin_" + sess.ID
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
	mgr, _, _ := newTestManager(t)

	// Explicitly set tmuxClient to nil to simulate no tmux available.
	mgr.SetTmuxClient(nil)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-notmux", Name: "rnotmux"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/find-cc", Name: "findcc"})
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
	mgr, _, _ := newTestManager(t)

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
	mgr, mock, _ := newTestManager(t)

	// Use a real temp directory so os.Stat in startSessionTmux passes.
	workDir := t.TempDir()

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Name: "bg"})
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
	mgr, _, _ := newTestManager(t)

	err := mgr.StartBackground("nonexistent-id")
	if err == nil {
		t.Fatal("expected error for non-existent session ID, got nil")
	}
}

func TestManager_StartBackground_AlreadyRunning(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	workDir := t.TempDir()
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Name: "already"})
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
	mgr, _, _ := newTestManager(t)

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
	mgr, _, _ := newTestManager(t)

	// Initially, notification history should be empty
	history := mgr.NotificationHistory()
	if len(history) != 0 {
		t.Fatalf("initial NotificationHistory: got %d entries, want 0", len(history))
	}

	// Create sessions and trigger hook events that generate notifications
	sess1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/notify-1", Name: "n1"})
	if err != nil {
		t.Fatalf("create sess1 failed: %v", err)
	}
	sess2, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/notify-2", Name: "n2"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/setstatus-persist", Name: "sp"})
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
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Name: "notmux"})
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
	mgr, _, _ := newTestManager(t)

	err := mgr.Kill("nonexistent-session-id")
	if err == nil {
		t.Fatal("expected error for non-existent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not contain 'not found'", err.Error())
	}
}

func TestManager_Kill_WithTmuxWindowOnly(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-win", Name: "killwin"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate session with TmuxWindowName but no TmuxPaneID (fallback path)
	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
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
	if !mock.hasCalledWith("KillSession", "jin_"+sess.ID) {
		t.Error("expected KillSession to be called with inner session name")
	}
}

func TestManager_Kill_NoTmux(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-notmux", Name: "killnotmux"})
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
	mgr, _, _ := newTestManager(t)

	err := mgr.Delete("nonexistent-session-id", false, false)
	if err == nil {
		t.Fatal("expected error for non-existent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not contain 'not found'", err.Error())
	}
}

func TestManager_Delete_Success(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// Create two sessions
	sess1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-s1", Name: "dels1"})
	if err != nil {
		t.Fatalf("create sess1 failed: %v", err)
	}
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-s2", Name: "dels2"})
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
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-tmux", Name: "deltmux"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Simulate session with active tmux
	mgr.mu.Lock()
	sess.TmuxWindowName = "jin_" + sess.ID
	sess.Status = StatusRunning
	mgr.mu.Unlock()

	mock.sessions[sess.TmuxWindowName] = true

	if err := mgr.Delete(sess.ID, false, false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Should have called KillSession on the inner tmux session
	if !mock.hasCalledWith("KillSession", "jin_"+sess.ID) {
		t.Error("expected KillSession to be called when deleting a session with tmux")
	}

	// Verify it's gone
	_, ok := mgr.Get(sess.ID)
	if ok {
		t.Fatal("Get returned ok=true after Delete")
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

func TestNewManager_LoadAll_RestoresIsWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	_, worktreeDir := setupTestWorktree(t)

	dataDir := t.TempDir()
	configDir := t.TempDir()

	// Persist a session pointing at the worktree. IsWorktree has json:"-"
	// so it's absent from the serialized form — mimicking a real restart.
	sessionJSON := `{"id":"wt-id","name":"wt","work_dir":"` + worktreeDir +
		`","created_at":"2025-01-01T00:00:00Z","status":"idle","claude_session_id":"cid","fleet":"default"}`
	if err := os.WriteFile(filepath.Join(dataDir, "wt-id.json"), []byte(sessionJSON), 0600); err != nil {
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

	sess, ok := mgr.Get("wt-id")
	if !ok {
		t.Fatal("session wt-id not found after load")
	}
	if !sess.IsWorktree {
		t.Errorf("IsWorktree = false, want true after restoring worktree session from disk")
	}
}

func TestManager_List_Empty(t *testing.T) {
	mgr, _, _ := newTestManager(t)

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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/idle-fallback-fresh", Name: "ifb-fresh"})
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
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/idle-fallback-recover", Name: "ifb-recover"})
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

func TestManager_HandleHookEvent_CWDUpdate_WorktreePathSkipped(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	origDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(origDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: origDir, Name: "wt-skip"})
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

// ---------------------------------------------------------------------------
// Worktree removal tests (Delete + removeGitWorktree)
// ---------------------------------------------------------------------------

// setupTestWorktree initializes a fresh git repo at a temp dir and adds a
// worktree under it. Returns (mainRepoDir, worktreeDir). Skips the test if
// `git` is not on PATH.
func setupTestWorktree(t *testing.T) (string, string) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := t.TempDir()

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
		}
	}

	runGit("init")
	runGit("config", "user.email", "test@test.com")
	runGit("config", "user.name", "test")

	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("init"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "init")

	worktreeDir := filepath.Join(repoDir, "wt")
	runGit("worktree", "add", worktreeDir, "-b", "test-branch")

	return repoDir, worktreeDir
}

func TestManager_Delete_RemovesWorktree(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	_, worktreeDir := setupTestWorktree(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: worktreeDir, Name: "wt"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := mgr.Delete(sess.ID, true, false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("worktree directory should be removed, but still exists: stat err=%v", err)
	}
}

func TestManager_Delete_PrefersCurrentWorkDirForWorktree(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mainRepo, worktreeDir := setupTestWorktree(t)

	// Reproduce the bug: WorkDir points at the main repo (because the
	// fix-worktree-workdir-overwrite guard prevents WorkDir from being
	// updated to a worktree path), while CurrentWorkDir tracks the actual
	// worktree the session is in.
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: mainRepo, Name: "wt-current"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.mu.Lock()
	sess.CurrentWorkDir = worktreeDir
	mgr.mu.Unlock()

	if err := mgr.Delete(sess.ID, true, false); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("worktree directory should be removed, but still exists: stat err=%v", err)
	}
	// Main repo must remain intact.
	if _, err := os.Stat(filepath.Join(mainRepo, ".git")); err != nil {
		t.Errorf("main repo .git should still exist: %v", err)
	}
}

func TestManager_Delete_NonWorktreeReturnsError(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mainRepo, _ := setupTestWorktree(t)

	// Both WorkDir and CurrentWorkDir point at the main repo (no worktree).
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: mainRepo, Name: "main-only"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	err = mgr.Delete(sess.ID, true, false)
	if !errors.Is(err, ErrNotWorktree) {
		t.Fatalf("expected ErrNotWorktree, got: %v", err)
	}

	// Session must still exist (Delete aborted before tmux kill / store removal).
	if _, ok := mgr.Get(sess.ID); !ok {
		t.Error("session should still exist after ErrNotWorktree")
	}
	// Main repo must remain intact.
	if _, err := os.Stat(filepath.Join(mainRepo, ".git")); err != nil {
		t.Errorf("main repo .git should still exist: %v", err)
	}
}

func TestManager_Delete_DirtyWorktreeReturnsErrWorktreeDirty(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	_, worktreeDir := setupTestWorktree(t)

	if err := os.WriteFile(filepath.Join(worktreeDir, "dirty.txt"), []byte("uncommitted"), 0644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: worktreeDir, Name: "wt-dirty"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	err = mgr.Delete(sess.ID, true, false)
	if !errors.Is(err, ErrWorktreeDirty) {
		t.Fatalf("expected ErrWorktreeDirty, got: %v", err)
	}

	if _, err := os.Stat(worktreeDir); err != nil {
		t.Errorf("worktree should still exist after dirty rejection: %v", err)
	}

	// Force removal should succeed.
	if err := mgr.Delete(sess.ID, true, true); err != nil {
		t.Fatalf("force Delete failed: %v", err)
	}
	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed after force delete: stat err=%v", err)
	}
}

func TestRemoveGitWorktree_AlreadyDeletedIsIdempotent(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	_, worktreeDir := setupTestWorktree(t)

	// Remove the worktree directory out-of-band.
	if err := os.RemoveAll(worktreeDir); err != nil {
		t.Fatalf("pre-remove worktree: %v", err)
	}

	if err := mgr.removeGitWorktree(worktreeDir, false); err != nil {
		t.Errorf("removeGitWorktree on missing dir should be nil, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateWithOptions worktree branch tests
// ---------------------------------------------------------------------------

// scriptedGitRunner is a git.Runner test double that dispatches on the args
// via a user-supplied handler and records every call for later assertions.
type scriptedGitRunner struct {
	mu      sync.Mutex
	calls   [][]string
	handler func(dir string, args []string) ([]byte, error)
}

func (r *scriptedGitRunner) Run(dir string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.mu.Unlock()
	if r.handler != nil {
		return r.handler(dir, args)
	}
	return nil, nil
}

// hadCall reports whether any recorded call starts with the given argv prefix.
func (r *scriptedGitRunner) hadCall(prefix ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if len(call) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if call[i] != p {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// findCall returns the first recorded argv that starts with prefix, or nil.
func (r *scriptedGitRunner) findCall(prefix ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, call := range r.calls {
		if len(call) < len(prefix) {
			continue
		}
		match := true
		for i, p := range prefix {
			if call[i] != p {
				match = false
				break
			}
		}
		if match {
			return call
		}
	}
	return nil
}

func TestManager_CreateWithOptions_Worktree_RejectsNonGitRepo(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:  t.TempDir(), // no .git present
		Name:     "nogit-wt",
		Worktree: true,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error %q should mention 'not a git repository'", err.Error())
	}
}

func TestManager_CreateWithOptions_Worktree_HappyPath(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// Redirect worktree base dir to a scratch location so the test does not
	// leak files into the user's real $XDG_STATE_HOME.
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	runner := &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case joined == "symbolic-ref refs/remotes/origin/HEAD":
				return []byte("refs/remotes/origin/main\n"), nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
				return nil, nil
			case len(args) >= 1 && args[0] == "rev-parse":
				// Branch does not exist — no collision.
				return nil, errors.New("exit status 1")
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected git call: %s", joined)
		},
	}
	mgr.gitClient = git.NewClientWithRunner(runner)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:  workDir,
		Name:     "wt-happy",
		Worktree: true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	wantPrefix := filepath.Join(stateDir, "honjin", "worktrees", "jin-")
	if !strings.HasPrefix(sess.WorkDir, wantPrefix) {
		t.Errorf("WorkDir = %q, want prefix %q", sess.WorkDir, wantPrefix)
	}
	suffix := strings.TrimPrefix(sess.WorkDir, wantPrefix)
	if len(suffix) != 8 {
		t.Errorf("worktree suffix = %q (len %d), want 8 hex chars", suffix, len(suffix))
	}

	// Assert that we resolved the default branch via symbolic-ref.
	if !runner.hadCall("symbolic-ref", "refs/remotes/origin/HEAD") {
		t.Error("expected `git symbolic-ref refs/remotes/origin/HEAD` to be called")
	}

	// Assert that we did NOT reach out to origin — worktree creation must
	// run offline so heavy repos aren't taxed on every session. Users who
	// want a fresh base should fetch via the post-create hook.
	if fetchCall := runner.findCall("fetch"); fetchCall != nil {
		t.Errorf("expected no `git fetch` during worktree creation, got %v", fetchCall)
	}

	// Assert AddWorktree used the auto-generated branch (jin/<8hex>),
	// the resolved worktree path, and origin/main as the base ref.
	addCall := runner.findCall("worktree", "add")
	if addCall == nil {
		t.Fatal("expected `git worktree add ...` to be called")
	}
	// Layout: worktree add -b <branch> <path> <baseRef>
	if len(addCall) != 6 {
		t.Fatalf("worktree add args len = %d, want 6: %v", len(addCall), addCall)
	}
	if addCall[2] != "-b" {
		t.Errorf("worktree add[2] = %q, want -b", addCall[2])
	}
	gotBranch := addCall[3]
	wantBranchPrefix := "jin/"
	if !strings.HasPrefix(gotBranch, wantBranchPrefix) {
		t.Errorf("worktree add branch = %q, want prefix %q", gotBranch, wantBranchPrefix)
	}
	if len(strings.TrimPrefix(gotBranch, wantBranchPrefix)) != 8 {
		t.Errorf("worktree add branch suffix = %q, want 8 hex chars", strings.TrimPrefix(gotBranch, wantBranchPrefix))
	}
	if addCall[4] != sess.WorkDir {
		t.Errorf("worktree add path = %q, want %q", addCall[4], sess.WorkDir)
	}
	if addCall[5] != "origin/main" {
		t.Errorf("worktree add baseRef = %q, want origin/main", addCall[5])
	}

	// Regression: IsWorktree must be set immediately (was previously only
	// populated by the 10s captureOutputTmux poll, hiding the TUI's `w`
	// delete option for the first 10 seconds after creation).
	if !sess.IsWorktree {
		t.Error("IsWorktree = false immediately after create, want true")
	}
}

func TestManager_CreateWithOptions_Worktree_RollsBackOnFailure(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	// Pre-create a session with the name the worktree create will re-use,
	// so the name-uniqueness check fails *after* the worktree/branch exist.
	if _, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir: t.TempDir(),
		Name:    "collide",
	}); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	runner := &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case joined == "symbolic-ref refs/remotes/origin/HEAD":
				return []byte("refs/remotes/origin/main\n"), nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
				return nil, nil
			case len(args) >= 1 && args[0] == "rev-parse":
				return nil, errors.New("exit status 1")
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
				// Populate the worktree layout so RemoveWorktree can
				// progress past its structural checks and actually call
				// the runner during rollback.
				worktreePath := args[4]
				mainGitDir := filepath.Join(dir, ".git", "worktrees", filepath.Base(worktreePath))
				if err := os.MkdirAll(mainGitDir, 0o755); err != nil {
					return nil, err
				}
				if err := os.MkdirAll(worktreePath, 0o755); err != nil {
					return nil, err
				}
				if err := os.WriteFile(
					filepath.Join(worktreePath, ".git"),
					[]byte("gitdir: "+mainGitDir+"\n"),
					0o644,
				); err != nil {
					return nil, err
				}
				return nil, nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "remove":
				_ = os.RemoveAll(args[len(args)-1])
				return nil, nil
			case len(args) >= 2 && args[0] == "branch" && args[1] == "-D":
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected git call: %s", joined)
		},
	}
	mgr.gitClient = git.NewClientWithRunner(runner)

	_, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:  workDir,
		Name:     "collide", // trips name-uniqueness after worktree exists
		Worktree: true,
	})
	if err == nil {
		t.Fatal("expected error from name collision, got nil")
	}

	if !runner.hadCall("worktree", "remove") {
		t.Error("expected RemoveWorktree runner call during rollback")
	}
	if !runner.hadCall("branch", "-D") {
		t.Error("expected DeleteBranch runner call during rollback")
	}
}

// TestManager_CreateWithOptions_Worktree_RollsBackOnWorkDirCollision covers a
// different failure path from the name-uniqueness test above: the WorkDir
// uniqueness check re-runs under the re-acquired lock after AddWorktree
// succeeds. Using --worktree-name gives us a predictable worktree path so we
// can pre-register a session at that exact location.
func TestManager_CreateWithOptions_Worktree_RollsBackOnWorkDirCollision(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	// Default base_dir template resolves to $XDG_STATE_HOME/honjin/worktrees/{name}.
	predictablePath := filepath.Join(stateDir, "honjin", "worktrees", "collide-wt")

	// Pre-create a session whose WorkDir is exactly the worktree path we'll
	// try to create below, so the re-lock WorkDir uniqueness check trips.
	if _, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir: predictablePath,
		Name:    "pre-existing",
	}); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	runner := &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case joined == "symbolic-ref refs/remotes/origin/HEAD":
				return []byte("refs/remotes/origin/main\n"), nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
				return nil, nil
			case len(args) >= 1 && args[0] == "rev-parse":
				// Branch does not exist — override-path pre-check passes.
				return nil, errors.New("exit status 1")
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
				worktreePath := args[4]
				mainGitDir := filepath.Join(dir, ".git", "worktrees", filepath.Base(worktreePath))
				if err := os.MkdirAll(mainGitDir, 0o755); err != nil {
					return nil, err
				}
				if err := os.MkdirAll(worktreePath, 0o755); err != nil {
					return nil, err
				}
				if err := os.WriteFile(
					filepath.Join(worktreePath, ".git"),
					[]byte("gitdir: "+mainGitDir+"\n"),
					0o644,
				); err != nil {
					return nil, err
				}
				return nil, nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "remove":
				_ = os.RemoveAll(args[len(args)-1])
				return nil, nil
			case len(args) >= 2 && args[0] == "branch" && args[1] == "-D":
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected git call: %s", joined)
		},
	}
	mgr.gitClient = git.NewClientWithRunner(runner)

	_, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:      workDir,
		Name:         "wt-collide",
		Worktree:     true,
		WorktreeName: "collide-wt",
	})
	if err == nil {
		t.Fatal("expected error from WorkDir collision, got nil")
	}
	if !strings.Contains(err.Error(), "already exists for directory") {
		t.Errorf("error %q should mention directory conflict", err.Error())
	}

	if !runner.hadCall("worktree", "remove") {
		t.Error("expected RemoveWorktree runner call during rollback")
	}
	if !runner.hadCall("branch", "-D") {
		t.Error("expected DeleteBranch runner call during rollback")
	}
}
