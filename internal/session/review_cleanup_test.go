package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/config"
	gitpkg "github.com/takaaki-s/jind-ai/internal/git"
)

func TestReviewCleanup_CleanPlanAndIdempotentExecution(t *testing.T) {
	mgr, sess := completedMergeCleanupSession(t)
	plan, journal, err := mgr.PrepareReviewCleanup(sess.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || len(plan.Blockers) != 0 || plan.Branch == "" || plan.RepositoryPath == "" || !journal.IsZero() {
		t.Fatalf("plan=%+v journal=%+v", plan, journal)
	}

	done, err := mgr.ExecuteReviewCleanup(sess.ID, plan.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != ReviewCleanupSucceeded || len(done.Steps) != 4 {
		t.Fatalf("journal = %+v", done)
	}
	for _, step := range done.Steps {
		if step.Status != ReviewCleanupStepSucceeded {
			t.Fatalf("step = %+v", step)
		}
	}
	if _, ok := mgr.GetInfo(sess.ID); ok {
		t.Fatal("session remained after successful cleanup")
	}
	if _, err := os.Stat(plan.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
	if mgr.gitClient.BranchExists(plan.RepositoryPath, plan.Branch) {
		t.Fatal("local branch remained after successful cleanup")
	}

	again, err := mgr.ExecuteReviewCleanup(sess.ID, plan.IdempotencyKey)
	if err != nil || again.Status != ReviewCleanupSucceeded {
		t.Fatalf("idempotent retry = %+v, %v", again, err)
	}
	preview, persisted, err := mgr.PrepareReviewCleanup(sess.ID, plan.IdempotencyKey)
	if err != nil || preview.IdempotencyKey != done.Plan.IdempotencyKey || preview.SessionID != done.Plan.SessionID ||
		preview.WorktreePath != done.Plan.WorktreePath || persisted.Status != ReviewCleanupSucceeded {
		t.Fatalf("post-delete audit plan=%+v journal=%+v err=%v", preview, persisted, err)
	}
}

func TestReviewCleanup_BlocksUnverifiedDirtyActiveAndBroadTargets(t *testing.T) {
	t.Run("unverified merge", func(t *testing.T) {
		mgr, sess := preparedMergeHandoffSession(t)
		plan, _, err := mgr.PrepareReviewCleanup(sess.ID, "")
		if err != nil || plan.Ready || !cleanupHasBlocker(plan, "verified merge") {
			t.Fatalf("plan=%+v err=%v", plan, err)
		}
	})

	t.Run("dirty and untracked", func(t *testing.T) {
		mgr, sess := completedMergeCleanupSession(t)
		if err := os.WriteFile(filepath.Join(sess.WorkDir, "untracked.txt"), []byte("keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		plan, _, err := mgr.PrepareReviewCleanup(sess.ID, "")
		if err != nil || plan.Ready || !cleanupHasBlocker(plan, "untracked") {
			t.Fatalf("plan=%+v err=%v", plan, err)
		}
	})

	t.Run("commit after verified head", func(t *testing.T) {
		mgr, sess := completedMergeCleanupSession(t)
		if err := os.WriteFile(filepath.Join(sess.WorkDir, "later.txt"), []byte("later\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runReviewTestGit(t, sess.WorkDir, "add", "--", "later.txt")
		runReviewTestGit(t, sess.WorkDir, "commit", "-m", "later local commit")
		plan, _, err := mgr.PrepareReviewCleanup(sess.ID, "")
		if err != nil || plan.Ready || plan.UnpushedCommits != 1 || !cleanupHasBlocker(plan, "after the verified merge head") {
			t.Fatalf("plan=%+v err=%v", plan, err)
		}
	})

	t.Run("active pane", func(t *testing.T) {
		mgr, sess := completedMergeCleanupSession(t)
		mgr.mu.Lock()
		live := mgr.sessions[sess.ID]
		live.TmuxWindowName = "jin-active"
		live.TmuxPaneID = "%7"
		mgr.mu.Unlock()
		tmuxMock := mgr.tmuxClient.(*mockTmuxRunner)
		tmuxMock.sessions["jin-active"] = true
		tmuxMock.deadPanes["%7"] = false
		plan, _, err := mgr.PrepareReviewCleanup(sess.ID, "")
		if err != nil || plan.Ready || !cleanupHasBlocker(plan, "pane is active") {
			t.Fatalf("plan=%+v err=%v", plan, err)
		}
	})

	t.Run("broad path", func(t *testing.T) {
		mgr, sess := completedMergeCleanupSession(t)
		mgr.mu.Lock()
		mgr.sessions[sess.ID].ReviewBase.WorktreePath = string(filepath.Separator)
		mgr.mu.Unlock()
		plan, _, err := mgr.PrepareReviewCleanup(sess.ID, "")
		if err != nil || plan.Ready || !cleanupHasBlocker(plan, "absolute non-root") {
			t.Fatalf("plan=%+v err=%v", plan, err)
		}
	})

	t.Run("primary repository is not the managed worktree", func(t *testing.T) {
		mgr, sess := completedMergeCleanupSession(t)
		valid, _, err := mgr.PrepareReviewCleanup(sess.ID, "")
		if err != nil || !valid.Ready {
			t.Fatalf("initial plan=%+v err=%v", valid, err)
		}
		mgr.mu.Lock()
		mgr.sessions[sess.ID].ReviewBase.WorktreePath = valid.RepositoryPath
		mgr.mu.Unlock()
		plan, _, err := mgr.PrepareReviewCleanup(sess.ID, "")
		if err != nil || plan.Ready || !cleanupHasBlocker(plan, "ownership") {
			t.Fatalf("plan=%+v err=%v", plan, err)
		}
	})
}

func TestReviewCleanup_PartialFailurePersistsAcrossRestartAndRetries(t *testing.T) {
	mgr, sess := completedMergeCleanupSession(t)
	plan, _, err := mgr.PrepareReviewCleanup(sess.ID, "")
	if err != nil || !plan.Ready {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	mgr.SetGitClient(gitpkg.NewClientWithRunner(cleanupExecRunner{failBranchDelete: true}))
	partial, err := mgr.ExecuteReviewCleanup(sess.ID, plan.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Status != ReviewCleanupFailed || partial.Steps[1].Status != ReviewCleanupStepSucceeded ||
		partial.Steps[2].Status != ReviewCleanupStepFailed || partial.Steps[3].Status != ReviewCleanupStepPending {
		t.Fatalf("partial journal = %+v", partial)
	}
	if _, err := os.Stat(plan.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree should already be gone: %v", err)
	}
	if !mgr.gitClient.BranchExists(plan.RepositoryPath, plan.Branch) {
		t.Fatal("branch should remain after injected branch failure")
	}

	cfg, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManager(mgr.store.dataDir, mgr.stateDir, testIdentity(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.SetTmuxClient(newMockTmuxRunner())
	restarted.SetGitClient(gitpkg.NewClient())
	if info, ok := restarted.GetInfo(sess.ID); !ok || info.Status != StatusDeleting {
		t.Fatalf("partial cleanup was not retained as deleting after restart: %+v found=%t", info, ok)
	}
	if _, err := restarted.PreCheckDelete(sess.ID, false, false); !errors.Is(err, ErrDeleteInFlight) {
		t.Fatalf("generic delete entered cleanup-owned session: %v", err)
	}
	done, err := restarted.ExecuteReviewCleanup(sess.ID, plan.IdempotencyKey)
	if err != nil || done.Status != ReviewCleanupSucceeded {
		t.Fatalf("retry = %+v, %v", done, err)
	}
	if done.Steps[1].Status != ReviewCleanupStepSucceeded || done.Steps[2].Status != ReviewCleanupStepSucceeded || done.Steps[3].Status != ReviewCleanupStepSucceeded {
		t.Fatalf("retry steps = %+v", done.Steps)
	}
}

func TestReviewCleanup_RestartMarksRunningJournalRetryable(t *testing.T) {
	mgr, sess := completedMergeCleanupSession(t)
	plan, _, err := mgr.PrepareReviewCleanup(sess.ID, "")
	if err != nil || !plan.Ready {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if err := mgr.cleanupStore.Save(newReviewCleanupJournal(plan, plan.PlannedAt)); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(mgr.store.dataDir, mgr.stateDir, testIdentity(), cfg); err != nil {
		t.Fatal(err)
	}
	journal, err := mgr.cleanupStore.Load(sess.ID)
	if err != nil || journal.Status != ReviewCleanupFailed || !strings.Contains(journal.Error, "same idempotency key") {
		t.Fatalf("journal=%+v err=%v", journal, err)
	}
}

func completedMergeCleanupSession(t *testing.T) (*Manager, *Session) {
	t.Helper()
	mgr, sess := preparedMergeHandoffSession(t)
	target := MergeHandoffTarget{Plugin: "fake-merge", Action: "merge"}
	payload, _, err := mgr.PrepareMergeHandoff(sess.ID, target, "")
	if err != nil {
		t.Fatal(err)
	}
	mergePayload, _, run, err := mgr.BeginMergeHandoff(sess.ID, target, payload.IdempotencyKey, readyMergePreflight(payload))
	if err != nil || !run {
		t.Fatalf("begin merge: run=%t err=%v", run, err)
	}
	if _, err := mgr.FinishMergeHandoff(sess.ID, payload.IdempotencyKey, successfulMergeResult(mergePayload), nil); err != nil {
		t.Fatal(err)
	}
	return mgr, sess
}

func cleanupHasBlocker(plan ReviewCleanupPlan, contains string) bool {
	for _, blocker := range plan.Blockers {
		if strings.Contains(blocker, contains) {
			return true
		}
	}
	return false
}

type cleanupExecRunner struct {
	failBranchDelete bool
}

func (r cleanupExecRunner) Run(dir string, args ...string) ([]byte, error) {
	if r.failBranchDelete && len(args) >= 2 && args[0] == "branch" && args[1] == "-D" {
		return []byte("injected branch delete failure"), fmt.Errorf("injected failure")
	}
	return exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
}
