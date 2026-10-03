package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/git"
	"github.com/takaaki-s/jind-ai/internal/session"
)

// TestHandleDelete_ReturnsBeforeFinalization pins the async property: even
// with worktree removal in flight, the handler returns quickly and the
// destructive work happens in a goroutine. We observe this by making the
// worktree removal deliberately slow (via a scripted git runner that sleeps
// in `worktree remove`) and asserting handleDelete returns in <1s while the
// removal is still in flight (session record still present, Status=Deleting).
func TestHandleDelete_ReturnsBeforeFinalization(t *testing.T) {
	s := newAsyncTestServer(t)

	// Register a session and a fake worktree the delete will operate on.
	sess, worktreeDir := seedWorktreeSession(t, s)

	runner := &slowRemoveRunner{delay: 300 * time.Millisecond, worktreeDir: worktreeDir}
	s.manager.SetGitClient(git.NewClientWithRunner(runner))

	data, _ := json.Marshal(DeleteRequest{ID: sess.ID, RemoveWorktree: true})

	start := time.Now()
	resp := s.handleDelete(data)
	elapsed := time.Since(start)
	if !resp.Success {
		t.Fatalf("Success = false: %s", resp.Error)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("handleDelete took %s, want <200ms (async path); finalize is 300ms", elapsed)
	}

	// While the removal is in flight, the record must be visible with
	// Status=Deleting so `get` surfaces the transition.
	info, ok := s.manager.GetInfo(sess.ID)
	if !ok {
		t.Fatal("session dropped immediately, want it visible during async finalize")
	}
	if info.Status != session.StatusDeleting {
		t.Errorf("Status during finalize = %q, want %q", info.Status, session.StatusDeleting)
	}

	// Eventually the finalize completes and the record disappears.
	waitFor(t, 3*time.Second, "session record to be dropped after finalize", func() bool {
		_, stillHere := s.manager.GetInfo(sess.ID)
		return !stillHere
	})
}

// TestHandleDelete_DirtyPreCheckReturnsSync pins that a dirty worktree with
// remove_worktree + no force fails synchronously on the response — the TUI
// depends on this to prompt the user for force confirmation.
func TestHandleDelete_DirtyPreCheckReturnsSync(t *testing.T) {
	s := newAsyncTestServer(t)
	sess, _ := seedWorktreeSession(t, s)

	// Runner reports a dirty tree on `status --porcelain`.
	s.manager.SetGitClient(git.NewClientWithRunner(dirtyStatusRunner{}))

	data, _ := json.Marshal(DeleteRequest{
		ID:             sess.ID,
		RemoveWorktree: true,
		// ForceRemoveWorktree left false — dirty must reject.
	})

	resp := s.handleDelete(data)
	if resp.Success {
		t.Fatal("Success = true, want false for dirty worktree without force")
	}
	if !strings.Contains(resp.Error, session.ErrWorktreeDirty.Error()) {
		t.Errorf("Error = %q, want it to contain %q", resp.Error, session.ErrWorktreeDirty.Error())
	}

	// Session status must be unchanged (no MarkDeleting on pre-check failure).
	info, ok := s.manager.GetInfo(sess.ID)
	if !ok {
		t.Fatal("session disappeared, want it preserved after pre-check failure")
	}
	if info.Status == session.StatusDeleting {
		t.Errorf("Status = %q, want it unchanged from before the request", info.Status)
	}
}

// TestHandleDelete_FinalizationFailure_LeavesSessionInErrorState uses a
// runner that fails `worktree remove` to trigger a finalize error, then
// asserts the async goroutine rolls Status back to Stopped with a
// populated ErrorMessage — the state the TUI surfaces to the user so they
// know delete needs a retry.
func TestHandleDelete_FinalizationFailure_LeavesSessionInErrorState(t *testing.T) {
	s := newAsyncTestServer(t)
	sess, _ := seedWorktreeSession(t, s)

	s.manager.SetGitClient(git.NewClientWithRunner(failingRemoveRunner{}))

	data, _ := json.Marshal(DeleteRequest{ID: sess.ID, RemoveWorktree: true})
	resp := s.handleDelete(data)
	if !resp.Success {
		t.Fatalf("Success = false: %s", resp.Error)
	}

	waitFor(t, 2*time.Second, "session to reach Stopped with ErrorMessage after finalize failure", func() bool {
		info, ok := s.manager.GetInfo(sess.ID)
		if !ok {
			return false
		}
		return info.Status == session.StatusStopped && info.ErrorMessage != ""
	})

	info, _ := s.manager.GetInfo(sess.ID)
	if !strings.Contains(info.ErrorMessage, "removing git worktree") {
		t.Errorf("ErrorMessage = %q, want it to mention worktree removal failure", info.ErrorMessage)
	}
}

// TestHandleDelete_ConcurrentSameSessionRejectsDuplicate covers the F003
// fix: two concurrent delete requests against the same session must not
// spawn two competing DeleteFinalize goroutines. The second request has
// to see the first's StatusDeleting flip and get rejected on the response,
// so only one goroutine touches `removeGitWorktree`/`KillSession`/`store.Delete`.
func TestHandleDelete_ConcurrentSameSessionRejectsDuplicate(t *testing.T) {
	s := newAsyncTestServer(t)
	sess, _ := seedWorktreeSession(t, s)

	// Slow runner so the first goroutine's finalize is still in flight
	// when the second request lands. The second must reject before the
	// first goroutine's removal completes.
	s.manager.SetGitClient(git.NewClientWithRunner(&slowRemoveRunner{
		delay:       300 * time.Millisecond,
		worktreeDir: sess.WorkDir,
	}))

	data, _ := json.Marshal(DeleteRequest{ID: sess.ID, RemoveWorktree: true})

	first := s.handleDelete(data)
	if !first.Success {
		t.Fatalf("first handleDelete Success=false: %s", first.Error)
	}

	// Second request while finalize is in flight — must be rejected
	// synchronously with ErrDeleteInFlight, not accepted.
	second := s.handleDelete(data)
	if second.Success {
		t.Fatal("second handleDelete Success=true, want rejection while first finalize is in flight")
	}
	if !strings.Contains(second.Error, "already in progress") {
		t.Errorf("second Error = %q, want it to mention 'already in progress'", second.Error)
	}

	// Let the first finalize complete.
	waitFor(t, 3*time.Second, "first finalize to complete", func() bool {
		_, stillHere := s.manager.GetInfo(sess.ID)
		return !stillHere
	})
}

// TestHandleDelete_MissingSessionReturnsSync pins the not-found early
// return: it must fail synchronously so the caller sees the reason on the
// same response instead of getting a bogus "accepted" and puzzling over a
// non-existent record.
func TestHandleDelete_MissingSessionReturnsSync(t *testing.T) {
	s := newAsyncTestServer(t)

	data, _ := json.Marshal(DeleteRequest{ID: "does-not-exist"})
	resp := s.handleDelete(data)
	if resp.Success {
		t.Fatal("Success = true, want false for missing session")
	}
	if !strings.Contains(resp.Error, "not found") {
		t.Errorf("Error = %q, want it to say 'not found'", resp.Error)
	}
}

// seedWorktreeSession registers a session pointing at a directory that
// looks like a git worktree (has a regular .git file with a `gitdir:`
// pointer). Returns the session and the worktree directory. Uses
// ReserveCreation directly rather than going through handleNew so the test
// avoids second-guessing the async provisioning path.
func seedWorktreeSession(t *testing.T, s *Server) (*session.Session, string) {
	t.Helper()
	worktreeDir := t.TempDir()
	mainGitDir := filepath.Join(t.TempDir(), ".git", "worktrees", "seeded")
	if err := os.MkdirAll(mainGitDir, 0o755); err != nil {
		t.Fatalf("mkdir mainGitDir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(worktreeDir, ".git"),
		[]byte("gitdir: "+mainGitDir+"\n"),
		0o644,
	); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	sess, _, err := s.manager.ReserveCreation(session.CreateOptions{
		WorkDir:     worktreeDir,
		Description: "delete-target",
		AgentKind:   "claude",
	})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}
	// Move Status off Creating so it looks like a normal running session
	// (matches what a real session would be in when the user hits delete).
	s.manager.SetStatus(sess.ID, session.StatusStopped)
	return sess, worktreeDir
}

// slowRemoveRunner sleeps in `worktree remove` so tests can observe the
// window when finalize is still in flight after handleDelete returns.
// Reports non-dirty for the pre-check.
type slowRemoveRunner struct {
	delay       time.Duration
	worktreeDir string
}

func (r slowRemoveRunner) Run(dir string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain":
		return nil, nil // clean
	case len(args) >= 2 && args[0] == "worktree" && args[1] == "remove":
		time.Sleep(r.delay)
		_ = os.RemoveAll(r.worktreeDir)
		return nil, nil
	}
	if len(args) > 0 && args[0] == "ls-files" {
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected git call: %s", joined)
}

// dirtyStatusRunner reports a dirty worktree so pre-check refuses delete.
type dirtyStatusRunner struct{}

func (dirtyStatusRunner) Run(dir string, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain" {
		return []byte(" M foo.go\n"), nil
	}
	if len(args) > 0 && args[0] == "ls-files" {
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected git call: %v", args)
}

// failingRemoveRunner passes the dirty pre-check but fails the actual
// removal, exercising the async goroutine's MarkDeletionFailed path.
type failingRemoveRunner struct{}

func (failingRemoveRunner) Run(dir string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case len(args) >= 2 && args[0] == "status" && args[1] == "--porcelain":
		return nil, nil
	case len(args) >= 2 && args[0] == "worktree" && args[1] == "remove":
		return []byte("permission denied"), errors.New("exit status 128")
	}
	if len(args) > 0 && args[0] == "ls-files" {
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected git call: %s", joined)
}
