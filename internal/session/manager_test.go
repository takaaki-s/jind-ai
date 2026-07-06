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

	"github.com/google/uuid"
	"github.com/takaaki-s/jindaiko/internal/config"
	"github.com/takaaki-s/jindaiko/internal/git"
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
	mgr.SetAgentResolver(newFakeAgentResolver())
	return mgr, tmuxMock, hookMock
}

// fakeAgent implements session.Agent with the CC hook vocabulary hard-wired
// so HandleHookEvent-driven tests still see the same status transitions as
// pre-refactor code. The concrete Claude Code adapter lives in
// internal/agent/claude/; importing it here would create a build cycle
// (agent/claude → session), so we hand-roll a duplicate mapping.
//
// enhancer is the Layer C enhancer the adapter returns via Description();
// tests that exercise Layer C swap it via installEnhancer (which reaches
// into the fakeAgentResolver held by newTestManager).
type fakeAgent struct {
	enhancer DescriptionEnhancer
}

func (a *fakeAgent) Kind() string                        { return "claude" }
func (a *fakeAgent) Setup(SetupContext) error            { return nil }
func (a *fakeAgent) SpawnCommand(SpawnOptions) SpawnPlan { return SpawnPlan{Command: "claude"} }
func (a *fakeAgent) Description() DescriptionEnhancer    { return a.enhancer }
func (a *fakeAgent) StatusSource() StatusSource          { return fakeStatusSource{} }

type fakeStatusSource struct{}

func (fakeStatusSource) Interpret(sig StatusSignal) (StatusUpdate, bool) {
	if sig.Kind != "hook" {
		return StatusUpdate{}, false
	}
	switch sig.Payload["event"] {
	case "UserPromptSubmit", "PreToolUse", "PostToolUse":
		return StatusUpdate{Status: StatusThinking, ClearError: true, Notify: NotifyNone}, true
	case "Stop":
		return StatusUpdate{Status: StatusIdle, ClearError: true, Notify: NotifyTaskComplete}, true
	case "StopFailure":
		return StatusUpdate{
			Status:       StatusIdle,
			ErrorMessage: sig.Payload["stop_reason"],
			Notify:       NotifyError,
		}, true
	case "SessionEnd":
		return StatusUpdate{Status: StatusStopped, Notify: NotifyNone}, true
	case "Notification":
		switch sig.Payload["notification_type"] {
		case "permission_prompt", "elicitation_dialog":
			return StatusUpdate{Status: StatusPermission, Notify: NotifyPermission}, true
		case "idle_prompt":
			return StatusUpdate{Status: StatusIdle, Notify: NotifyNone}, true
		}
	}
	return StatusUpdate{}, false
}

type fakeAgentResolver struct {
	agents map[string]Agent
}

func newFakeAgentResolver() *fakeAgentResolver {
	return &fakeAgentResolver{
		agents: map[string]Agent{"claude": &fakeAgent{}},
	}
}

func (r *fakeAgentResolver) Resolve(kind string) (Agent, error) {
	a, ok := r.agents[kind]
	if !ok {
		return nil, fmt.Errorf("unknown agent kind: %s", kind)
	}
	return a, nil
}

// installEnhancer swaps the Layer C enhancer the "claude" fake adapter
// returns via Description(). Tests that used to call
// Manager.SetDescriptionEnhancer(enh) now do installEnhancer(mgr, enh) —
// the effect (HandleHookEvent picks up enh on UserPromptSubmit / Stop) is
// identical.
func installEnhancer(t *testing.T, mgr *Manager, enh DescriptionEnhancer) {
	t.Helper()
	resolver, ok := mgr.agentResolver.(*fakeAgentResolver)
	if !ok {
		t.Fatalf("expected *fakeAgentResolver, got %T", mgr.agentResolver)
	}
	ag, ok := resolver.agents["claude"].(*fakeAgent)
	if !ok {
		t.Fatalf(`expected "claude" adapter to be *fakeAgent, got %T`, resolver.agents["claude"])
	}
	ag.enhancer = enh
}

// ---------------------------------------------------------------------------
// CreateWithOptions tests
// ---------------------------------------------------------------------------

func TestManager_CreateWithOptions_Success(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     "/tmp/project-alpha",
		Description: "alpha",
	})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	if sess.ID == "" {
		t.Error("expected non-empty ID")
	}
	if sess.Description != "alpha" {
		t.Errorf("Name = %q, want %q", sess.Description, "alpha")
	}
	if sess.WorkDir != "/tmp/project-alpha" {
		t.Errorf("WorkDir = %q, want %q", sess.WorkDir, "/tmp/project-alpha")
	}
	if sess.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", sess.Status, StatusStopped)
	}
	if sess.AgentSessionID == "" {
		t.Error("expected non-empty AgentSessionID")
	}
}

func TestManager_CreateWithOptions_DefaultFleet(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-default", Description: "fd"})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	if sess.Fleet != DefaultFleet {
		t.Errorf("Fleet = %q, want %q", sess.Fleet, DefaultFleet)
	}
}

func TestManager_CreateWithOptions_ExplicitFleet(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-named", Description: "fn", Fleet: "backend"})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	if sess.Fleet != "backend" {
		t.Errorf("Fleet = %q, want %q", sess.Fleet, "backend")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dup-dir", Description: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/dup-dir", Description: "second"})
	if err == nil {
		t.Fatal("expected error for duplicate WorkDir, got nil")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_SkipWorktreeSession(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	// Simulate the session moving into a worktree (CurrentWorkDir updated by daemon polling)
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/repo/.claude/worktrees/some-branch"
	mgr.mu.Unlock()

	// Creating a new session for the same WorkDir should succeed
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "second"})
	if err != nil {
		t.Fatalf("expected success when existing session is in worktree, got: %v", err)
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_BlockNonWorktree(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	// Set CurrentWorkDir to repo root (not a worktree) — duplicate check should still block
	mgr.mu.Lock()
	s1.CurrentWorkDir = "/tmp/repo"
	mgr.mu.Unlock()

	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "second"})
	if err == nil {
		t.Fatal("expected error for duplicate WorkDir when session is not in worktree")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_StoppedSession(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// CurrentWorkDir defaults to "" for a freshly created (stopped) session
	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "first"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "second"})
	if err == nil {
		t.Fatal("stopped session (CurrentWorkDir empty) should still block duplicate WorkDir")
	}
}

func TestManager_CreateWithOptions_DuplicateWorkDir_ReturnFromWorktree(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "first"})
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
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/repo", Description: "second"})
	if err == nil {
		t.Fatal("expected error after session returned from worktree to original WorkDir")
	}
}

func TestManager_CreateWithOptions_EmptyWorkDir(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "", Description: "nodir"})
	if err == nil {
		t.Fatal("expected error for empty WorkDir, got nil")
	}
}

func TestManager_CreateWithOptions_DefaultDescription(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/home/user/my-project"})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	want := filepath.Base("/home/user/my-project")
	if sess.Description != want {
		t.Errorf("Description = %q, want %q (filepath.Base of WorkDir)", sess.Description, want)
	}
	if sess.DescriptionLocked {
		t.Error("DescriptionLocked = true, want false when auto-generated")
	}
}

func TestManager_CreateWithOptions_WhitespaceOnlyDescription_UsesBaseline(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     "/home/user/whitespace-project",
		Description: "   \t  ",
	})
	if err != nil {
		t.Fatalf("CreateWithOptions failed: %v", err)
	}
	want := filepath.Base("/home/user/whitespace-project")
	if sess.Description != want {
		t.Errorf("Description = %q, want %q (whitespace-only should fall back to baseline)", sess.Description, want)
	}
	if sess.DescriptionLocked {
		t.Error("DescriptionLocked = true, want false when input trimmed to empty")
	}
}

// ---------------------------------------------------------------------------
// Get tests
// ---------------------------------------------------------------------------

func TestManager_Get_Found(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	created, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/get-test", Description: "getme"})
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

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/list-1", Description: "first"})
	if err != nil {
		t.Fatalf("create first failed: %v", err)
	}
	// Ensure distinct CreatedAt timestamps.
	time.Sleep(2 * time.Millisecond)
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/list-2", Description: "second"})
	if err != nil {
		t.Fatalf("create second failed: %v", err)
	}

	infos := mgr.List()
	if len(infos) != 2 {
		t.Fatalf("List returned %d items, want 2", len(infos))
	}
	// Sorted by CreatedAt ascending
	if infos[0].Description != "first" {
		t.Errorf("first item Name = %q, want %q", infos[0].Description, "first")
	}
	if infos[1].Description != "second" {
		t.Errorf("second item Name = %q, want %q", infos[1].Description, "second")
	}
}

func TestManager_List_SortedByFleet(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-1", Description: "s1", Fleet: "backend"})
	if err != nil {
		t.Fatalf("create s1 failed: %v", err)
	}
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-2", Description: "s2"}) // default
	if err != nil {
		t.Fatalf("create s2 failed: %v", err)
	}
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/fleet-sort-3", Description: "s3", Fleet: "alpha"})
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/status-test", Description: "s1"})
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/err-test", Description: "e1"})
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-old", Description: "wd"})
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

	_, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-dup", Description: "d1"})
	if err != nil {
		t.Fatalf("create first failed: %v", err)
	}
	s2, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-other", Description: "d2"})
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

	s1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-dup", Description: "d1"})
	if err != nil {
		t.Fatalf("create first failed: %v", err)
	}
	s2, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/wd-other", Description: "d2"})
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

	s1, _, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-1", Description: "ca1"})
	s2, _, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-2", Description: "ca2"})
	s3, _, _ := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/ca-3", Description: "ca3"})

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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-ups", Description: "hups"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")

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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-stop", Description: "hstop"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Set to thinking first
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-perm", Description: "hperm"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Notification", "permission_prompt", "", "")

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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-cwd", Description: "hcwd"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// CWD to a non-git-root path: WorkDir should NOT update, only CurrentWorkDir
	nonGitCwd := "/tmp/hook-cwd-subdir"
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", nonGitCwd, "")

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
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", gitRootCwd, "")

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
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: origDir, Description: "hcwdch"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// CWD to a non-git-root: only CurrentWorkDir updates
	subDir := filepath.Join(origDir, "subdir")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "CwdChanged", "", subDir, "")

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
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "CwdChanged", "", worktreeDir, "")

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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sf", Description: "hsf"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "rate_limit")

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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sfclr", Description: "hsfclr"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	// First: StopFailure sets ErrorMessage
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "rate_limit")
	got, _ := mgr.Get(sess.ID)
	if got.ErrorMessage != "rate_limit" {
		t.Fatalf("ErrorMessage after StopFailure = %q, want %q", got.ErrorMessage, "rate_limit")
	}

	// Then: Stop clears ErrorMessage
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")
	got, _ = mgr.Get(sess.ID)
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage after Stop = %q, want empty", got.ErrorMessage)
	}
}

func TestManager_HandleHookEvent_StopFailure_ThenUserPrompt_ClearsError(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sfclr2", Description: "hsfclr2"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	// StopFailure sets ErrorMessage
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "auth_error")
	got, _ := mgr.Get(sess.ID)
	if got.ErrorMessage != "auth_error" {
		t.Fatalf("ErrorMessage after StopFailure = %q, want %q", got.ErrorMessage, "auth_error")
	}

	// UserPromptSubmit clears ErrorMessage
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	got, _ = mgr.Get(sess.ID)
	if got.ErrorMessage != "" {
		t.Errorf("ErrorMessage after UserPromptSubmit = %q, want empty", got.ErrorMessage)
	}
}

// SessionEnd on a session that still has an error message must preserve
// that message: pre-refactor SessionEnd never touched ErrorMessage, so a
// StopFailure that fired just before the process died should still surface
// after the session is stopped. This guards F002 from regressing back to
// "any adapter verdict clears the error field".
func TestManager_HandleHookEvent_SessionEnd_PreservesError(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sessend", Description: "sessend"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "rate_limit")
	got, _ := mgr.Get(sess.ID)
	if got.ErrorMessage != "rate_limit" {
		t.Fatalf("ErrorMessage after StopFailure = %q, want %q", got.ErrorMessage, "rate_limit")
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionEnd", "", "", "")
	got, _ = mgr.Get(sess.ID)
	if got.ErrorMessage != "rate_limit" {
		t.Errorf("ErrorMessage after SessionEnd = %q, want %q (SessionEnd must preserve)", got.ErrorMessage, "rate_limit")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status after SessionEnd = %q, want %q", got.Status, StatusStopped)
	}
}

// Notification hooks (permission_prompt / elicitation_dialog / idle_prompt)
// must not touch ErrorMessage either — F002 regression guard.
func TestManager_HandleHookEvent_Notification_PreservesError(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-notif", Description: "notif"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "auth_error")
	got, _ := mgr.Get(sess.ID)
	if got.ErrorMessage != "auth_error" {
		t.Fatalf("ErrorMessage after StopFailure = %q, want %q", got.ErrorMessage, "auth_error")
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Notification", "permission_prompt", "", "")
	got, _ = mgr.Get(sess.ID)
	if got.ErrorMessage != "auth_error" {
		t.Errorf("ErrorMessage after Notification = %q, want %q (Notification must preserve)", got.ErrorMessage, "auth_error")
	}
	if got.Status != StatusPermission {
		t.Errorf("Status after Notification permission_prompt = %q, want %q", got.Status, StatusPermission)
	}
}

// F001 regression guard: SessionEnd delivered to an already-stopped session
// must not silently mutate in-memory-only fields (LastOutputTime /
// LastActiveAt). The early-return path handles CWD persistence only.
func TestManager_HandleHookEvent_SessionEnd_AlreadyStopped_NoOp(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sess-noop", Description: "noop"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Prime the session as already-stopped with a fixed LastActiveAt so we
	// can prove SessionEnd did not shift it.
	fixed := time.Now().Add(-1 * time.Hour)
	mgr.mu.Lock()
	sess.Status = StatusStopped
	sess.LastActiveAt = fixed
	mgr.mu.Unlock()

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionEnd", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if !got.LastActiveAt.Equal(fixed) {
		t.Errorf("LastActiveAt drifted: got %v, want %v", got.LastActiveAt, fixed)
	}
}

func TestManager_HandleHookEvent_StopFailure_EmptyReason(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sf2", Description: "hsf2"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "")

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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-ss", Description: "hss"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Ensure AgentSessionStarted is false initially
	sess.AgentSessionStarted = false

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if !got.AgentSessionStarted {
		t.Error("AgentSessionStarted should be true after SessionStart hook")
	}
}

func TestManager_HandleHookEvent_SessionEnd(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-se", Description: "hse"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionEnd", "", "", "")

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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-sei", Description: "hsei"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Session is already stopped (default from creation)
	if sess.Status != StatusStopped {
		t.Fatalf("precondition: Status = %q, want %q", sess.Status, StatusStopped)
	}

	// Should not panic or change anything
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionEnd", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
}

// ensureHooksSettingsFile lives under internal/agent/claude/ now; its tests
// moved with it (see hooks_settings_test.go there).

// ---------------------------------------------------------------------------
// Kill tests
// ---------------------------------------------------------------------------

func TestManager_Kill(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-test", Description: "killme"})
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-test", Description: "delme"})
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-live", Description: "rlive"})
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-dead", Description: "rdead"})
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/recover-notmux", Description: "rnotmux"})
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
// FindByAgentSessionID tests
// ---------------------------------------------------------------------------

func TestManager_FindByAgentSessionID(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/find-cc", Description: "findcc"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Find by the AgentSessionID that was auto-generated during creation.
	got, ok := mgr.FindByAgentSessionID(sess.AgentSessionID)
	if !ok {
		t.Fatal("FindByAgentSessionID returned ok=false for existing session")
	}
	if got.ID != sess.ID {
		t.Errorf("ID = %q, want %q", got.ID, sess.ID)
	}

	// Find with a non-existent AgentSessionID should return nil.
	got2, ok2 := mgr.FindByAgentSessionID("nonexistent-cc-id")
	if ok2 {
		t.Fatal("FindByAgentSessionID returned ok=true for non-existent AgentSessionID")
	}
	if got2 != nil {
		t.Errorf("expected nil session, got %+v", got2)
	}
}

func TestManager_FindByAgentSessionID_NotFound(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// Empty manager: should return nil, false.
	got, ok := mgr.FindByAgentSessionID("does-not-exist")
	if ok {
		t.Fatal("FindByAgentSessionID returned ok=true on empty manager")
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Description: "bg"})
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
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Description: "already"})
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
	sess1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/notify-1", Description: "n1"})
	if err != nil {
		t.Fatalf("create sess1 failed: %v", err)
	}
	sess2, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/notify-2", Description: "n2"})
	if err != nil {
		t.Fatalf("create sess2 failed: %v", err)
	}

	// Set sessions to thinking first (Stop hook transitions from thinking to idle)
	mgr.SetStatus(sess1.ID, StatusThinking)
	mgr.SetStatus(sess2.ID, StatusThinking)

	// Trigger Stop event (generates task_complete notification)
	mgr.HandleHookEvent(sess1.AgentSessionID, sess1.ID, "Stop", "", "", "")

	// Trigger Notification/permission event (generates permission notification)
	mgr.HandleHookEvent(sess2.AgentSessionID, sess2.ID, "Notification", "permission_prompt", "", "")

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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/setstatus-persist", Description: "sp"})
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
	// Deliberately do NOT call SetTmuxClient — tmux client remains nil so
	// ensureTmuxClient exercises the auto-init path.
	//
	// Isolate the auto-init to a unique tmux socket name and register a
	// cleanup that kills the resulting server. Without this, running this
	// test on a machine with tmux installed leaves a stray "-L jin" server
	// behind, and the next daemon start reuses it — the server's tmux env
	// (including CLAUDE_CODE_CHILD_SESSION inherited from whatever launched
	// `go test`) propagates to every CC subsequently spawned in that
	// daemon, silently breaking Layer C description enhancement. See the
	// spawn.go doc comment on the CLAUDE_CODE_* unset list.
	socketName := "jin-test-" + uuid.New().String()[:8]
	mgr.SetTmuxSocketName(socketName)
	mgr.SetAgentResolver(newFakeAgentResolver())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socketName, "kill-server").Run()
	})

	workDir := t.TempDir()
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Description: "notmux"})
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-win", Description: "killwin"})
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/kill-notmux", Description: "killnotmux"})
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
	sess1, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-s1", Description: "dels1"})
	if err != nil {
		t.Fatalf("create sess1 failed: %v", err)
	}
	_, _, err = mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-s2", Description: "dels2"})
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
	if infos[0].Description != "dels2" {
		t.Errorf("remaining session Name = %q, want %q", infos[0].Description, "dels2")
	}
}

func TestManager_Delete_WithTmuxSession(t *testing.T) {
	mgr, mock, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/del-tmux", Description: "deltmux"})
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

	// Write a session JSON without the fleet field (simulates old data).
	// The same fixture also exercises the name → description migration.
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
	// Legacy "name" value must be preserved as the new Description.
	if infos[0].Description != "old" {
		t.Errorf("Description = %q, want %q (legacy name should migrate into description)", infos[0].Description, "old")
	}
	// Migrated descriptions are conservatively locked so Layer C doesn't
	// overwrite a value the user had already chosen manually.
	if !infos[0].DescriptionLocked {
		t.Error("DescriptionLocked = false, want true (migrated name should be locked)")
	}
}

// TestNewManager_LoadAll_WritesBackMigratedJSON verifies that the on-disk
// fixture is rewritten in place after migration: the legacy "name" key is
// removed and "description" / "description_locked" replace it. This locks in
// the spec-accepted behaviour that the migration is idempotent and observable
// on disk (spec receipt criterion 13).
func TestNewManager_LoadAll_WritesBackMigratedJSON(t *testing.T) {
	dataDir := t.TempDir()
	configDir := t.TempDir()

	fixturePath := filepath.Join(dataDir, "old-id.json")
	oldJSON := `{"id":"old-id","name":"old","work_dir":"/tmp/old","created_at":"2025-01-01T00:00:00Z","status":"idle","claude_session_id":"cid"}`
	if err := os.WriteFile(fixturePath, []byte(oldJSON), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	configMgr, err := config.NewManager(configDir)
	if err != nil {
		t.Fatalf("config.NewManager failed: %v", err)
	}
	if _, err := NewManager(dataDir, configDir, configMgr); err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture back: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	if _, hasName := got["name"]; hasName {
		t.Error(`migrated JSON still contains "name" key`)
	}
	if desc, _ := got["description"].(string); desc != "old" {
		t.Errorf(`"description" = %q, want "old"`, desc)
	}
	if locked, _ := got["description_locked"].(bool); !locked {
		t.Error(`"description_locked" = false, want true`)
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

// ensureClaudeTrustState moved to internal/agent/claude/ as EnsureTrustState;
// see internal/agent/claude/trust_test.go for the tests.

// ---------------------------------------------------------------------------
// Idle fallback tests (hook-timeout detection in captureOutputTmux)
// ---------------------------------------------------------------------------

// TestManager_IdleFallback_FreshStart verifies that a session in StatusRunning
// with a non-zero StartedAt and stale LastOutputTime satisfies the fallback
// condition that captureOutputTmux uses to transition running → idle.
func TestManager_IdleFallback_FreshStart(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/idle-fallback-fresh", Description: "ifb-fresh"})
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/idle-fallback-recover", Description: "ifb-recover"})
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
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: origDir, Description: "wt-skip"})
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

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "CwdChanged", "", worktreeDir, "")

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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: worktreeDir, Description: "wt"})
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
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: mainRepo, Description: "wt-current"})
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
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: mainRepo, Description: "main-only"})
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

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: worktreeDir, Description: "wt-dirty"})
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
		WorkDir:     t.TempDir(), // no .git present
		Description: "nogit-wt",
		Worktree:    true,
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
			case len(args) >= 1 && args[0] == "fetch":
				return nil, nil
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
		WorkDir:     workDir,
		Description: "wt-happy",
		Worktree:    true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	wantPrefix := filepath.Join(stateDir, "jindaiko", "worktrees", "jin-")
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

	// Auto-fetch on worktree creation was removed (feat/worktree-offline-creation);
	// the local origin/<base> tip is used as-is, and users refresh manually or via
	// the post-create hook.
	if runner.findCall("fetch") != nil {
		t.Error("expected no `git fetch` call (auto-fetch is disabled)")
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
}

// TestManager_CreateWithOptions_Worktree_RollsBackOnWorkDirCollision verifies
// that the worktree/branch created before the sessions map re-lock are cleaned
// up when the post-lock WorkDir uniqueness check fails. Using --worktree-name
// gives us a predictable worktree path so we can pre-register a session at
// that exact location.
func TestManager_CreateWithOptions_Worktree_RollsBackOnWorkDirCollision(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	// Default base_dir template resolves to $XDG_STATE_HOME/jindaiko/worktrees/{name}.
	predictablePath := filepath.Join(stateDir, "jindaiko", "worktrees", "collide-wt")

	// Pre-create a session whose WorkDir is exactly the worktree path we'll
	// try to create below, so the re-lock WorkDir uniqueness check trips.
	if _, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     predictablePath,
		Description: "pre-existing",
	}); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	runner := &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case joined == "symbolic-ref refs/remotes/origin/HEAD":
				return []byte("refs/remotes/origin/main\n"), nil
			case len(args) >= 1 && args[0] == "fetch":
				return nil, nil
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
		Description:  "wt-collide",
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

// ---------------------------------------------------------------------------
// TryUpgradeDescription tests
// ---------------------------------------------------------------------------

// stubEnhancer is a minimal DescriptionEnhancer whose response can be scripted
// per test case. It also records how many times TryGenerate was called so the
// "no-op" cases can assert the enhancer was never consulted.
//
// layer defaults to DescriptionLayerBaseline (zero); tests that expect a
// successful promotion must set it to a layer strictly greater than the
// session's current layer.
type stubEnhancer struct {
	response string
	ok       bool
	layer    DescriptionLayer
	calls    int
}

func (s *stubEnhancer) TryGenerate(sess *Session) (string, DescriptionLayer, bool) {
	s.calls++
	return s.response, s.layer, s.ok
}

func TestManager_TryUpgradeDescription_Locked_NoOp(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     "/tmp/upgrade-locked",
		Description: "manual label",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if !sess.DescriptionLocked {
		t.Fatal("precondition: session created with Description should be locked")
	}

	enh := &stubEnhancer{response: "candidate", ok: true}
	mgr.TryUpgradeDescription(sess.ID, enh)

	got, _ := mgr.Get(sess.ID)
	if got.Description != "manual label" {
		t.Errorf("Description = %q, want %q (locked → no upgrade)", got.Description, "manual label")
	}
	if enh.calls != 0 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 0", enh.calls)
	}
}

// TestManager_TryUpgradeDescription_DescriptionDrift_NoOp exercises Guard 1
// from the "direct drift" angle: Description is off-baseline while
// DescriptionLayer is still zero, without staging any prior enhancer promotion.
// The companion RestartGuard test covers the same guard reached through the
// "layer→restart→zero" path; keep both to document the two ways state can
// drift, even though they hit the same short-circuit.
func TestManager_TryUpgradeDescription_DescriptionDrift_NoOp(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-drift"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Simulate an out-of-band edit that moved the description off the baseline
	// without staging a corresponding DescriptionLayer promotion.
	mgr.mu.Lock()
	sess.Description = "already upgraded"
	mgr.mu.Unlock()

	enh := &stubEnhancer{response: "should not apply", ok: true}
	mgr.TryUpgradeDescription(sess.ID, enh)

	got, _ := mgr.Get(sess.ID)
	if got.Description != "already upgraded" {
		t.Errorf("Description = %q, want %q (baseline mismatch → no upgrade)", got.Description, "already upgraded")
	}
	if enh.calls != 0 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 0", enh.calls)
	}
}

func TestManager_TryUpgradeDescription_Success_ApplyCandidate(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-ok"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if sess.DescriptionLocked {
		t.Fatal("precondition: auto-generated description should be unlocked")
	}
	baseline := GenerateBaselineDescription(sess.WorkDir, "", false, "")
	if sess.Description != baseline {
		t.Fatalf("precondition: Description=%q should match baseline %q", sess.Description, baseline)
	}

	enh := &stubEnhancer{response: "auth refactor", ok: true, layer: DescriptionLayerTranscript}
	mgr.TryUpgradeDescription(sess.ID, enh)

	got, _ := mgr.Get(sess.ID)
	if got.Description != "auth refactor" {
		t.Errorf("Description = %q, want %q", got.Description, "auth refactor")
	}
	if got.DescriptionLocked {
		t.Error("DescriptionLocked = true, want false (Layer C must not lock)")
	}
	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}

	// The store must reflect the new description so a daemon restart preserves it.
	reloaded, err := mgr.store.Load(sess.ID)
	if err != nil {
		t.Fatalf("store.Load failed: %v", err)
	}
	if reloaded.Description != "auth refactor" {
		t.Errorf("persisted Description = %q, want %q", reloaded.Description, "auth refactor")
	}
}

func TestManager_TryUpgradeDescription_EnhancerPending_NoOp(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-pending"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	before := sess.Description

	enh := &stubEnhancer{response: "", ok: false}
	mgr.TryUpgradeDescription(sess.ID, enh)

	got, _ := mgr.Get(sess.ID)
	if got.Description != before {
		t.Errorf("Description = %q, want %q (enhancer pending → keep baseline)", got.Description, before)
	}
	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
}

func TestManager_TryUpgradeDescription_NilEnhancer_NoPanic(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-nil"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	before := sess.Description

	// Must not panic even when Manager has no enhancer wired.
	mgr.TryUpgradeDescription(sess.ID, nil)

	got, _ := mgr.Get(sess.ID)
	if got.Description != before {
		t.Errorf("Description = %q, want %q (nil enhancer → no-op)", got.Description, before)
	}
}

func TestManager_TryUpgradeDescription_UnknownSession_NoPanic(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	enh := &stubEnhancer{response: "x", ok: true}
	mgr.TryUpgradeDescription("does-not-exist", enh)

	if enh.calls != 0 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 0 (unknown session should short-circuit)", enh.calls)
	}
}

// TestManager_HandleHookEvent_UserPromptSubmit_UpgradesDescription verifies that
// the hook path calls the installed enhancer for the UserPromptSubmit event.
func TestManager_HandleHookEvent_UserPromptSubmit_UpgradesDescription(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	enh := &stubEnhancer{response: "hook-derived", ok: true, layer: DescriptionLayerTranscript}
	installEnhancer(t, mgr, enh)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-upgrade"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "hook-derived" {
		t.Errorf("Description = %q, want %q (UserPromptSubmit should trigger Layer C)", got.Description, "hook-derived")
	}
	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
}

// TestManager_HandleHookEvent_Stop_UpgradesDescription verifies that the Stop
// hook path also invokes Layer C. Stop is our reliable fallback when
// UserPromptSubmit races the transcript flush and finds an empty file — see
// the comment in HandleHookEvent for the ~10ms skew observed in practice.
func TestManager_HandleHookEvent_Stop_UpgradesDescription(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	enh := &stubEnhancer{response: "stop-derived", ok: true, layer: DescriptionLayerTranscript}
	installEnhancer(t, mgr, enh)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-stop-upgrade"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Stop only fires after a UserPromptSubmit transitioned status away from
	// stopped, so mirror that ordering to keep the setup realistic.
	mgr.SetStatus(sess.ID, StatusThinking)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "stop-derived" {
		t.Errorf("Description = %q, want %q (Stop should trigger Layer C as a flush-safe fallback)", got.Description, "stop-derived")
	}
	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
}

// TestManager_HandleHookEvent_OtherEvents_DoNotUpgrade guards against wiring
// Layer C to hook events that don't imply a completed transcript write.
// SessionStart is intentionally omitted: it is now a Layer C trigger (via the
// LayerAgentName path) since Claude Code writes ~/.claude/sessions/<PID>.json
// before that hook fires. See TestManager_HandleHookEvent_SessionStart* for the
// positive assertion.
func TestManager_HandleHookEvent_OtherEvents_DoNotUpgrade(t *testing.T) {
	events := []string{"SessionEnd", "CwdChanged", "PostToolUse"}
	for _, ev := range events {
		t.Run(ev, func(t *testing.T) {
			mgr, _, _ := newTestManager(t)
			enh := &stubEnhancer{response: "should-not-apply", ok: true}
			installEnhancer(t, mgr, enh)

			sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-" + ev})
			if err != nil {
				t.Fatalf("create failed: %v", err)
			}
			before := sess.Description

			mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, ev, "", "", "")

			got, _ := mgr.Get(sess.ID)
			if got.Description != before {
				t.Errorf("%s: Description = %q, want %q (Layer C should only fire on UserPromptSubmit/Stop)", ev, got.Description, before)
			}
			if enh.calls != 0 {
				t.Errorf("%s: enhancer.TryGenerate calls = %d, want 0", ev, enh.calls)
			}
		})
	}
}

// TestManager_HandleHookEvent_SessionStart_TriggersLayerC is the positive
// counterpart to TestManager_HandleHookEvent_OtherEvents_DoNotUpgrade:
// SessionStart is a Layer C trigger via the LayerAgentName path, matching
// Claude Code writing ~/.claude/sessions/<PID>.json before the hook fires.
func TestManager_HandleHookEvent_SessionStart_TriggersLayerC(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	installEnhancer(t, mgr, &stubEnhancer{response: "cc-name-42", ok: true, layer: DescriptionLayerAgentName})

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-session-start-layerc"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "cc-name-42" {
		t.Errorf("Description = %q, want %q", got.Description, "cc-name-42")
	}
	if got.DescriptionLayer != DescriptionLayerAgentName {
		t.Errorf("DescriptionLayer = %d, want %d", got.DescriptionLayer, DescriptionLayerAgentName)
	}
}

// TestManager_TryUpgradeDescription_LayerPromotion locks in the two-hop
// promotion path a real Claude Code session takes: SessionStart plants
// LayerAgentName from the session-name file, then a later UserPromptSubmit
// swaps in the higher-quality LayerTranscript candidate once the transcript
// has been flushed.
func TestManager_TryUpgradeDescription_LayerPromotion(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	installEnhancer(t, mgr, &stubEnhancer{response: "cc-name", ok: true, layer: DescriptionLayerAgentName})

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-layer-promotion"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "cc-name" || got.DescriptionLayer != DescriptionLayerAgentName {
		t.Fatalf("after SessionStart: Description=%q Layer=%d, want %q/%d",
			got.Description, got.DescriptionLayer, "cc-name", DescriptionLayerAgentName)
	}

	installEnhancer(t, mgr, &stubEnhancer{response: "user prompt", ok: true, layer: DescriptionLayerTranscript})

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")

	got, _ = mgr.Get(sess.ID)
	if got.Description != "user prompt" {
		t.Errorf("Description = %q, want %q", got.Description, "user prompt")
	}
	if got.DescriptionLayer != DescriptionLayerTranscript {
		t.Errorf("DescriptionLayer = %d, want %d", got.DescriptionLayer, DescriptionLayerTranscript)
	}
}

// TestManager_TryUpgradeDescription_RejectsDowngrade locks in Guard 2: once a
// higher-layer description is in place, a lower-layer candidate must not
// overwrite it, even though the enhancer offers a non-empty candidate. This
// is what keeps a stray SessionStart delivered after Stop from clobbering an
// already-good LayerTranscript description.
func TestManager_TryUpgradeDescription_RejectsDowngrade(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	installEnhancer(t, mgr, &stubEnhancer{response: "prompt-desc", ok: true, layer: DescriptionLayerTranscript})

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-reject-downgrade"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "prompt-desc" || got.DescriptionLayer != DescriptionLayerTranscript {
		t.Fatalf("after UserPromptSubmit: Description=%q Layer=%d, want %q/%d",
			got.Description, got.DescriptionLayer, "prompt-desc", DescriptionLayerTranscript)
	}

	installEnhancer(t, mgr, &stubEnhancer{response: "name-desc", ok: true, layer: DescriptionLayerAgentName})

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	got, _ = mgr.Get(sess.ID)
	if got.Description != "prompt-desc" {
		t.Errorf("Description = %q, want %q (lower-layer candidate must not downgrade)", got.Description, "prompt-desc")
	}
	if got.DescriptionLayer != DescriptionLayerTranscript {
		t.Errorf("DescriptionLayer = %d, want %d (unchanged)", got.DescriptionLayer, DescriptionLayerTranscript)
	}
}

// TestManager_TryUpgradeDescription_RestartGuard locks in Guard 1: a daemon
// restart loses the runtime-only DescriptionLayer (json:"-") back to zero,
// but the persisted Description may already carry a prior Layer C upgrade.
// The guard must treat that drift as "already upgraded, provenance unknown"
// and refuse to consult the enhancer at all, rather than let a fresh
// SessionStart clobber it.
func TestManager_TryUpgradeDescription_RestartGuard(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	installEnhancer(t, mgr, &stubEnhancer{response: "prior-upgrade", ok: true, layer: DescriptionLayerAgentName})

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/hook-restart-guard"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Description != "prior-upgrade" || got.DescriptionLayer != DescriptionLayerAgentName {
		t.Fatalf("precondition: Description=%q Layer=%d, want %q/%d",
			got.Description, got.DescriptionLayer, "prior-upgrade", DescriptionLayerAgentName)
	}

	// Simulate the daemon restart: reset the in-memory layer to zero while
	// leaving the persisted Description drifted from baseline, exactly what
	// a freshly loaded Manager would observe.
	mgr.mu.Lock()
	got.DescriptionLayer = DescriptionLayerBaseline
	mgr.mu.Unlock()

	enh := &stubEnhancer{response: "should-not-apply", ok: true, layer: DescriptionLayerAgentName}
	installEnhancer(t, mgr, enh)

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionStart", "", "", "")

	final, _ := mgr.Get(sess.ID)
	if final.Description != "prior-upgrade" {
		t.Errorf("Description = %q, want %q (Guard 1 should have blocked the overwrite)", final.Description, "prior-upgrade")
	}
	if enh.calls != 0 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 0 (Guard 1 short-circuits before consulting the enhancer)", enh.calls)
	}
}

// TestManager_TryUpgradeDescription_BranchPopulated_StillFires locks in the
// F001 fix: once captureOutputTmux populates CurrentBranch / IsWorktree, the
// baseline used for the equality guard must still match the value stored at
// create time (which knew nothing about branch/worktree). If the two baselines
// diverge, Layer C is silently disabled for every real Claude session.
func TestManager_TryUpgradeDescription_BranchPopulated_StillFires(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/upgrade-branch-populated"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if sess.DescriptionLocked {
		t.Fatal("precondition: auto-generated description should be unlocked")
	}

	// Simulate the poller having filled in git/worktree metadata. Prior to the
	// F001 fix these would flip the guard's baseline to a value the stored
	// Description no longer matched, aborting the upgrade.
	mgr.mu.Lock()
	sess.CurrentBranch = "main"
	sess.IsWorktree = true
	sess.TmuxWindowName = "jin-abc"
	mgr.mu.Unlock()

	enh := &stubEnhancer{response: "post-poll upgrade", ok: true, layer: DescriptionLayerTranscript}
	mgr.TryUpgradeDescription(sess.ID, enh)

	got, _ := mgr.Get(sess.ID)
	if got.Description != "post-poll upgrade" {
		t.Errorf("Description = %q, want %q (baseline guard must ignore runtime branch/worktree fields)", got.Description, "post-poll upgrade")
	}
	if enh.calls != 1 {
		t.Errorf("enhancer.TryGenerate calls = %d, want 1", enh.calls)
	}
}

// TestManager_SetDescription_WhitespaceOnly_Unlocks confirms F006: a value
// consisting only of whitespace behaves like the empty-string unlock path
// rather than being persisted verbatim.
func TestManager_SetDescription_WhitespaceOnly_Unlocks(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     "/tmp/set-description-whitespace",
		Description: "manual label",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if !sess.DescriptionLocked {
		t.Fatal("precondition: session with explicit Description should be locked")
	}

	if err := mgr.SetDescription(sess.ID, "   \t \n "); err != nil {
		t.Fatalf("SetDescription: %v", err)
	}

	got, _ := mgr.Get(sess.ID)
	baseline := GenerateBaselineDescription(sess.WorkDir, "", false, "")
	if got.Description != baseline {
		t.Errorf("Description = %q, want baseline %q (whitespace-only value should reset to baseline)", got.Description, baseline)
	}
	if got.DescriptionLocked {
		t.Error("DescriptionLocked = true, want false (whitespace-only value should unlock)")
	}
}

// TestBuildAgentShellCmd_SnapshotIsolatesFromConcurrentWrites is the F402
// regression guard: buildAgentShellCmd must operate on a value snapshot so
// callers can invoke it after m.mu.Unlock() while HandleHookEvent (or any
// other write path) mutates the same session under lock.
//
// The test spins a writer goroutine that keeps flipping session.AgentKind /
// AgentSessionID under lock, and a reader loop that snapshots + builds
// commands with the lock released. Under -race, any read of session.*
// inside buildAgentShellCmd would fire.
func TestBuildAgentShellCmd_SnapshotIsolatesFromConcurrentWrites(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir:     "/tmp/spawn-race",
		Description: "spawn-race",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	// Writer: flip fields under lock as fast as it can. Mirrors the write
	// pattern in HandleHookEvent (AgentSessionID / AgentSessionStarted /
	// WorkDir change under m.mu.Lock).
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			mgr.mu.Lock()
			sess.AgentSessionID = "written-under-lock"
			sess.AgentSessionStarted = !sess.AgentSessionStarted
			sess.WorkDir = "/tmp/spawn-race"
			mgr.mu.Unlock()
		}
	}()

	// Reader: snapshot under lock (mirrors the retry path) then run the
	// builder outside the lock. If snapshotForSpawn / buildAgentShellCmd
	// touched the session pointer instead of the value copy, -race would
	// catch it here.
	for i := 0; i < 500; i++ {
		mgr.mu.Lock()
		snap := snapshotForSpawn(sess, sess.WorkDir, sess.WorkDir)
		mgr.mu.Unlock()

		if _, err := mgr.buildAgentShellCmd(snap); err != nil {
			t.Fatalf("build failed at iter %d: %v", i, err)
		}
	}
	close(stop)
	<-writerDone
}
