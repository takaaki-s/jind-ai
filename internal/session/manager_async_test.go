package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/git"
	"github.com/takaaki-s/jind-ai/internal/worktreehook"
)

// TestReserveCreation_InsertsStatusCreating verifies the reservation path
// registers a session in the map with StatusCreating and persists it before
// any external I/O runs. This is what lets the daemon return an ID to the
// caller before provisioning completes.
func TestReserveCreation_InsertsStatusCreating(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	workDir := t.TempDir()
	sess, _, err := mgr.ReserveCreation(CreateOptions{
		WorkDir:     workDir,
		Description: "reserve-test",
	})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
	if sess.Status != StatusCreating {
		t.Errorf("Status = %q, want %q", sess.Status, StatusCreating)
	}

	// Present in the map and persisted to disk.
	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session not registered in map after ReserveCreation")
	}
	if got.Status != StatusCreating {
		t.Errorf("map Status = %q, want %q", got.Status, StatusCreating)
	}
	loaded, err := mgr.store.LoadAll()
	if err != nil {
		t.Fatalf("store.LoadAll: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("store has %d records, want 1", len(loaded))
	}
	if loaded[0].Status != StatusCreating {
		t.Errorf("persisted Status = %q, want %q", loaded[0].Status, StatusCreating)
	}
}

// TestReserveCreation_WorktreeSkipsWorkDirConflict pins that two concurrent
// worktree reservations against the same repo root do not trip the
// WorkDir-uniqueness check at reservation time. The placeholder path
// (opts.WorkDir = repo root) is intentionally shared; the final worktree
// paths are unique and checked by ProvisionAsync.
func TestReserveCreation_WorktreeSkipsWorkDirConflict(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	if _, _, err := mgr.ReserveCreation(CreateOptions{
		WorkDir:  repo,
		Worktree: true,
	}); err != nil {
		t.Fatalf("first ReserveCreation: %v", err)
	}
	if _, _, err := mgr.ReserveCreation(CreateOptions{
		WorkDir:  repo,
		Worktree: true,
	}); err != nil {
		t.Fatalf("second ReserveCreation for the same repo root: %v", err)
	}
}

// TestReserveCreation_NonWorktreeEnforcesWorkDirConflict pins the historical
// invariant "no two non-worktree sessions manage the same directory".
func TestReserveCreation_NonWorktreeEnforcesWorkDirConflict(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	workDir := t.TempDir()

	if _, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: workDir}); err != nil {
		t.Fatalf("first ReserveCreation: %v", err)
	}
	_, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: workDir})
	if err == nil {
		t.Fatal("expected WorkDir conflict on second ReserveCreation, got nil")
	}
	if !strings.Contains(err.Error(), "already exists for directory") {
		t.Errorf("error = %q, want it to mention directory conflict", err.Error())
	}
}

// TestProvisionAsync_MarkCreationFailedOnHookError follows the async handler
// contract: ProvisionAsync returns an error, the caller (mimicked here)
// invokes MarkCreationFailed, and the session record is left with
// Status=Stopped + ErrorMessage set so a `get` after acceptance can surface it.
func TestProvisionAsync_MarkCreationFailedOnHookError(t *testing.T) {
	mgr, hookMock, workDir := setupHookTest(t, hookFailGitRunner())
	scriptPath := filepath.Join(workDir, ".jin", "worktree-post-create.sh")
	hookMock.discoverExists[workDir] = true
	hookMock.verdictFor[scriptPath] = worktreehook.VerdictOK
	hookMock.runErr = fmt.Errorf("exit status 1")

	sess, _, err := mgr.ReserveCreation(CreateOptions{
		WorkDir:  workDir,
		Worktree: true,
	})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}

	_, provErr := mgr.ProvisionAsync(sess, CreateOptions{WorkDir: workDir, Worktree: true})
	if provErr == nil {
		t.Fatal("expected ProvisionAsync error from hook failure, got nil")
	}

	mgr.MarkCreationFailed(sess.ID, provErr)

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session record dropped on failure, want it preserved for async clients")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if got.ErrorMessage == "" {
		t.Error("ErrorMessage empty, want the provisioning failure message")
	}
	if !strings.Contains(got.ErrorMessage, "hook") {
		t.Errorf("ErrorMessage = %q, want it to mention hook", got.ErrorMessage)
	}
}

// TestSetCreationWarning_PersistsOnRecord verifies non-fatal warnings from
// async provisioning survive on the session record and appear on Info.
func TestSetCreationWarning_PersistsOnRecord(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}

	mgr.SetCreationWarning(sess.ID, "hook script not allowed")

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session missing after SetCreationWarning")
	}
	if got.CreationWarning != "hook script not allowed" {
		t.Errorf("CreationWarning = %q, want %q", got.CreationWarning, "hook script not allowed")
	}
	if got.ToInfo().CreationWarning != "hook script not allowed" {
		t.Errorf("Info.CreationWarning = %q, want it propagated", got.ToInfo().CreationWarning)
	}
}

// TestPreCheckDelete_DirtyReturnsErrWorktreeDirty runs the dirty pre-check
// synchronously via a scripted git runner that reports uncommitted changes.
// The daemon relies on this staying synchronous — the TUI's dirty-confirm
// UX depends on the error surfacing on the request response.
func TestPreCheckDelete_DirtyReturnsErrWorktreeDirty(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	// Set up a fake worktree directory with a regular .git file so
	// IsGitWorktreeDir returns true.
	worktreeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}

	runner := &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain" {
				return []byte(" M foo.go\n"), nil // one modified file -> dirty
			}
			return nil, fmt.Errorf("unexpected git call: %v", args)
		},
	}
	mgr.gitClient = git.NewClientWithRunner(runner)

	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: worktreeDir, Worktree: true})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}
	// ReserveCreation places WorkDir at opts.WorkDir; for this pre-check
	// test we want the session's persisted WorkDir to point at the fake
	// worktree so ResolveWorktreeDir picks it up.
	mgr.mu.Lock()
	mgr.sessions[sess.ID].WorkDir = worktreeDir
	mgr.mu.Unlock()

	_, err = mgr.PreCheckDelete(sess.ID, true, false)
	if !errors.Is(err, ErrWorktreeDirty) {
		t.Fatalf("err = %v, want ErrWorktreeDirty", err)
	}
}

// TestPreCheckDelete_NotWorktreeReturnsErrNotWorktree covers the case where
// remove_worktree is requested but the target isn't a git worktree.
func TestPreCheckDelete_NotWorktreeReturnsErrNotWorktree(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	plainDir := t.TempDir() // no .git file → IsGitWorktreeDir=false

	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: plainDir})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}

	_, err = mgr.PreCheckDelete(sess.ID, true, false)
	if !errors.Is(err, ErrNotWorktree) {
		t.Fatalf("err = %v, want ErrNotWorktree", err)
	}
}

// TestPreCheckDelete_MissingSessionErrors covers the not-found early return.
func TestPreCheckDelete_MissingSessionErrors(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	_, err := mgr.PreCheckDelete("no-such-id", false, false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a 'not found' error", err)
	}
}

// TestPreCheckDelete_ForceWithoutRemoveWorktreeRejected keeps the CLI's
// invariant enforced at the Manager level too — non-CLI callers hit this.
func TestPreCheckDelete_ForceWithoutRemoveWorktreeRejected(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}
	_, err = mgr.PreCheckDelete(sess.ID, false, true)
	if err == nil || !strings.Contains(err.Error(), "forceRemoveWorktree requires removeWorktree") {
		t.Fatalf("err = %v, want the force/remove invariant error", err)
	}
}

// TestMarkDeleting_TransitionsAndPersists pins the state flip that a `get`
// between accept and finalize relies on.
func TestMarkDeleting_TransitionsAndPersists(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}
	req, err := mgr.PreCheckDelete(sess.ID, false, false)
	if err != nil {
		t.Fatalf("PreCheckDelete: %v", err)
	}
	if err := mgr.MarkDeleting(&req); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}
	if req.previousStatus != StatusCreating {
		t.Errorf("req.previousStatus = %q, want %q (captured under MarkDeleting's lock)", req.previousStatus, StatusCreating)
	}
	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session missing after MarkDeleting")
	}
	if got.Status != StatusDeleting {
		t.Errorf("Status = %q, want %q", got.Status, StatusDeleting)
	}

	// Store must reflect the flip so a daemon restart during the window
	// observes it in PersistedStatus.
	loaded, err := mgr.store.LoadAll()
	if err != nil {
		t.Fatalf("store.LoadAll: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Status != StatusDeleting {
		t.Fatalf("persisted status = %v (records=%d), want StatusDeleting", func() Status {
			if len(loaded) == 0 {
				return ""
			}
			return loaded[0].Status
		}(), len(loaded))
	}
}

// TestMarkDeletionFailed_RestoresPreviousStatus_Stopped covers the common
// case: a stopped session's delete fails and the record returns to Stopped
// (as it was before MarkDeleting).
func TestMarkDeletionFailed_RestoresPreviousStatus_Stopped(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}
	// Move to Stopped so MarkDeleting captures previousStatus=Stopped.
	mgr.SetStatus(sess.ID, StatusStopped)

	req, err := mgr.PreCheckDelete(sess.ID, false, false)
	if err != nil {
		t.Fatalf("PreCheckDelete: %v", err)
	}
	if err := mgr.MarkDeleting(&req); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}
	mgr.MarkDeletionFailed(req, fmt.Errorf("permission denied"))

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session missing after MarkDeletionFailed, want it preserved")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q (previousStatus)", got.Status, StatusStopped)
	}
	if got.ErrorMessage != "permission denied" {
		t.Errorf("ErrorMessage = %q, want %q", got.ErrorMessage, "permission denied")
	}
}

// TestMarkDeletionFailed_RestoresPreviousStatus_LivePane exercises the
// F001 fix: a live session (idle/thinking/permission) whose delete fails
// must not silently degrade to Stopped — the pane is still running and
// the UI must show that.
func TestMarkDeletionFailed_RestoresPreviousStatus_LivePane(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusIdle) // pretend the session was idle

	req, err := mgr.PreCheckDelete(sess.ID, false, false)
	if err != nil {
		t.Fatalf("PreCheckDelete: %v", err)
	}
	if err := mgr.MarkDeleting(&req); err != nil {
		t.Fatalf("MarkDeleting: %v", err)
	}
	mgr.MarkDeletionFailed(req, fmt.Errorf("permission denied"))

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session missing after MarkDeletionFailed")
	}
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q (previous idle preserved on failure)", got.Status, StatusIdle)
	}
	if got.ErrorMessage != "permission denied" {
		t.Errorf("ErrorMessage = %q, want %q", got.ErrorMessage, "permission denied")
	}
}

// TestMarkDeleting_RejectsWhenAlreadyInFlight pins the F003 fix: a second
// MarkDeleting against a session already in StatusDeleting returns
// ErrDeleteInFlight so the daemon handler rejects the duplicate before
// spawning a competing finalize goroutine.
func TestMarkDeleting_RejectsWhenAlreadyInFlight(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}
	first := DeleteRequest{ID: sess.ID}
	if err := mgr.MarkDeleting(&first); err != nil {
		t.Fatalf("first MarkDeleting: %v", err)
	}
	second := DeleteRequest{ID: sess.ID}
	err = mgr.MarkDeleting(&second)
	if !errors.Is(err, ErrDeleteInFlight) {
		t.Fatalf("second MarkDeleting err = %v, want ErrDeleteInFlight", err)
	}
}

// TestPreCheckDelete_RejectsWhenAlreadyDeleting pins the F015 fix: an
// in-flight delete must block a second PreCheckDelete before it touches
// the on-disk worktree with `git status --porcelain` — that probe on a
// checkout being rm -rf'd would produce spurious ErrNotWorktree /
// ErrWorktreeDirty depending on timing.
func TestPreCheckDelete_RejectsWhenAlreadyDeleting(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}
	// Simulate an in-flight delete by flipping status directly.
	mgr.SetStatus(sess.ID, StatusDeleting)

	_, err = mgr.PreCheckDelete(sess.ID, false, false)
	if !errors.Is(err, ErrDeleteInFlight) {
		t.Fatalf("PreCheckDelete err = %v, want ErrDeleteInFlight", err)
	}
}

// TestDelete_TwoFailedThenSucceed pins the F014 stuck-state fix: two
// serial delete attempts that each fail must NOT permanently pin the
// session at StatusDeleting. Without atomic snapshot in MarkDeleting, the
// second MarkDeletionFailed would restore to a stale previousStatus
// (StatusDeleting itself), and the session would be un-deleteable.
func TestDelete_TwoFailedThenSucceed(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	worktreeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: worktreeDir, Worktree: true})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}
	// Baseline pre-delete status is StatusCreating (fresh reservation);
	// move to Stopped so restore semantics are clearly observable.
	mgr.SetStatus(sess.ID, StatusStopped)

	// Runner that reports clean status but fails removal.
	failCount := 0
	failThenSucceed := &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain":
				return nil, nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "remove":
				failCount++
				if failCount <= 2 {
					return []byte("simulated failure"), fmt.Errorf("exit status 128")
				}
				// Third attempt succeeds — clean up so DeleteFinalize's
				// map/store removal follows.
				_ = os.RemoveAll(worktreeDir)
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected git call: %s", joined)
		},
	}
	mgr.gitClient = git.NewClientWithRunner(failThenSucceed)

	// Attempt 1: expect failure, status restored to Stopped.
	if err := mgr.Delete(sess.ID, true, false); err == nil {
		t.Fatal("attempt 1 succeeded, want failure from mocked runner")
	}
	if got, _ := mgr.Get(sess.ID); got.Status != StatusStopped {
		t.Fatalf("after attempt 1 Status = %q, want %q", got.Status, StatusStopped)
	}

	// Attempt 2: expect failure, status restored to Stopped again (not
	// stuck at Deleting, which would happen if MarkDeleting relied on a
	// pre-check-time snapshot).
	if err := mgr.Delete(sess.ID, true, false); err == nil {
		t.Fatal("attempt 2 succeeded, want failure from mocked runner")
	}
	if got, _ := mgr.Get(sess.ID); got.Status != StatusStopped {
		t.Fatalf("after attempt 2 Status = %q, want %q (stuck at Deleting = F014 regression)", got.Status, StatusStopped)
	}

	// Attempt 3: succeeds → session gone.
	if err := mgr.Delete(sess.ID, true, false); err != nil {
		t.Fatalf("attempt 3: %v", err)
	}
	if _, ok := mgr.Get(sess.ID); ok {
		t.Fatal("session still registered after successful delete")
	}
}

// TestRecoverTmuxSessions_CreatingInterrupted_MarkedWithErrorMessage pins
// the recovery bug-fix: a session that was mid-provisioning when the daemon
// went down must be marked Stopped + ErrorMessage so the user sees why
// nothing is running.
func TestRecoverTmuxSessions_CreatingInterrupted_MarkedWithErrorMessage(t *testing.T) {
	mgr, tmuxMock, _ := newTestManager(t)

	// Manually inject a session that looks like it was mid-provisioning.
	// TmuxWindowName is intentionally empty — no tmux ever spawned.
	sess := &Session{
		ID:              "interrupted-creating",
		Description:     "interrupted",
		WorkDir:         t.TempDir(),
		Status:          StatusStopped, // in-memory normalization
		PersistedStatus: StatusCreating,
		AgentKind:       "claude",
	}
	mgr.mu.Lock()
	mgr.sessions[sess.ID] = sess
	mgr.mu.Unlock()

	// tmuxMock reports HasSession/IsPaneDead false for whatever we ask.
	_ = tmuxMock

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session missing after RecoverTmuxSessions")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if got.ErrorMessage == "" || !strings.Contains(got.ErrorMessage, "provisioning was interrupted") {
		t.Errorf("ErrorMessage = %q, want it to mention interrupted provisioning", got.ErrorMessage)
	}
}

// TestRecoverTmuxSessions_DeletingInterrupted_MarkedWithErrorMessage mirrors
// the above for the delete side.
func TestRecoverTmuxSessions_DeletingInterrupted_MarkedWithErrorMessage(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess := &Session{
		ID:              "interrupted-deleting",
		Description:     "interrupted-del",
		WorkDir:         t.TempDir(),
		Status:          StatusStopped,
		PersistedStatus: StatusDeleting,
		AgentKind:       "claude",
	}
	mgr.mu.Lock()
	mgr.sessions[sess.ID] = sess
	mgr.mu.Unlock()

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session missing after RecoverTmuxSessions")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
	if !strings.Contains(got.ErrorMessage, "deletion was interrupted") {
		t.Errorf("ErrorMessage = %q, want it to mention interrupted deletion", got.ErrorMessage)
	}
}

// TestRecoverTmuxSessions_DeletingInterrupted_LivePane pins the F002 fix:
// a StatusDeleting session whose pane is still alive when the daemon
// restarts must NOT be resumed (that would silently reverse the user's
// delete). recoverResume converts it to Stopped + interruptedAsyncMessage
// so a retry via `jin session delete` is obvious.
func TestRecoverTmuxSessions_DeletingInterrupted_LivePane(t *testing.T) {
	mgr, tmuxMock, _ := newTestManager(t)

	sess := &Session{
		ID:              "interrupted-deleting-alive",
		Description:     "interrupted-del-alive",
		WorkDir:         t.TempDir(),
		Status:          StatusStopped, // in-memory normalization
		PersistedStatus: StatusDeleting,
		AgentKind:       "claude",
		TmuxWindowName:  "jin_interrupted",
		TmuxPaneID:      "%1",
	}
	mgr.mu.Lock()
	mgr.sessions[sess.ID] = sess
	mgr.mu.Unlock()

	// Pane is alive: HasSession=true (mock's `sessions` map), IsPaneDead=false (default).
	tmuxMock.mu.Lock()
	tmuxMock.sessions["jin_interrupted"] = true
	tmuxMock.mu.Unlock()

	mgr.RecoverTmuxSessions()

	got, ok := mgr.Get(sess.ID)
	if !ok {
		t.Fatal("session missing after RecoverTmuxSessions")
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q (delete intent wins over live pane)", got.Status, StatusStopped)
	}
	if !strings.Contains(got.ErrorMessage, "deletion was interrupted") {
		t.Errorf("ErrorMessage = %q, want it to mention interrupted deletion", got.ErrorMessage)
	}
}

// hookFailGitRunner returns a git runner wired for the worktree-creation
// path a hook-failure test walks: prune → detect base → add → remove-on-rollback.
func hookFailGitRunner() *scriptedGitRunner {
	return &scriptedGitRunner{
		handler: func(dir string, args []string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case joined == "symbolic-ref refs/remotes/origin/HEAD":
				return []byte("refs/remotes/origin/main\n"), nil
			case len(args) >= 2 && args[0] == "worktree" && args[1] == "prune":
				return nil, nil
			case isTestResolveCommit(args):
				return []byte(testReviewBaseOID + "\n"), nil
			case len(args) >= 1 && args[0] == "rev-parse":
				return nil, fmt.Errorf("exit status 1")
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
}
