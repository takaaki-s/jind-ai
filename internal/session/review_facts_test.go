package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMergeReviewFacts_PrefersGenerationThenObservation(t *testing.T) {
	oldTime := time.Unix(10, 0)
	newTime := time.Unix(20, 0)
	oldGeneration := ReviewFacts{Status: ReviewFactsAvailable, AttentionGeneration: 1, ObservedAt: newTime, ChangedFiles: 9}
	newGeneration := ReviewFacts{Status: ReviewFactsPending, AttentionGeneration: 2, ObservedAt: oldTime}
	if got := mergeReviewFacts(oldGeneration, newGeneration); got != newGeneration {
		t.Fatalf("generation merge = %+v, want %+v", got, newGeneration)
	}
	older := ReviewFacts{Status: ReviewFactsPending, AttentionGeneration: 2, ObservedAt: oldTime}
	newer := ReviewFacts{Status: ReviewFactsAvailable, AttentionGeneration: 2, ObservedAt: newTime, ChangedFiles: 3}
	if got := mergeReviewFacts(older, newer); got != newer {
		t.Fatalf("observation merge = %+v, want %+v", got, newer)
	}
}

func TestManager_RefreshReviewPromotesNonEmptyDelta(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, base := reviewTestRepo(t)
	runReviewTestGit(t, repo, "update-ref", "refs/remotes/origin/main", base)

	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir: repo, Description: "review", Worktree: true, WorktreeBase: "main", NoHook: true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	managedWorktree := sess.ReviewBase.WorktreePath
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", managedWorktree).Run()
	})
	if err := os.WriteFile(filepath.Join(managedWorktree, "change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	live := mgr.sessions[sess.ID]
	// WorkDir is allowed to follow an agent into another git root. Review must
	// still inspect the managed checkout captured in ReviewBase.
	live.WorkDir = t.TempDir()
	live.Attention = Attention{State: AttentionDone, Generation: 1}
	live.Status = StatusIdle
	saved := *live
	mgr.mu.Unlock()
	if err := mgr.store.Save(saved); err != nil {
		t.Fatal(err)
	}

	info, err := mgr.RefreshReview(sess.ID)
	if err != nil {
		t.Fatalf("RefreshReview: %v", err)
	}
	if info.ReviewFacts.Status != ReviewFactsAvailable || info.ReviewFacts.ChangedFiles != 1 {
		t.Fatalf("ReviewFacts = %+v", info.ReviewFacts)
	}
	if info.Attention.State != AttentionReadyForReview || !info.Attention.Unseen {
		t.Fatalf("Attention = %+v, want unseen ready-for-review", info.Attention)
	}
	loaded, err := mgr.store.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ReviewFacts.Status != ReviewFactsAvailable || loaded.Attention.State != AttentionReadyForReview {
		t.Fatalf("persisted review = %+v attention = %+v", loaded.ReviewFacts, loaded.Attention)
	}
}

func TestManager_RefreshReviewWithoutManagedBaseStaysDone(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "ordinary")
	mgr.mu.Lock()
	live := mgr.sessions[sess.ID]
	live.Attention = Attention{State: AttentionDone, Generation: 1}
	live.Status = StatusIdle
	mgr.mu.Unlock()

	info, err := mgr.RefreshReview(sess.ID)
	if err != nil {
		t.Fatalf("RefreshReview: %v", err)
	}
	if info.ReviewFacts.Status != ReviewFactsUnavailable ||
		info.ReviewFacts.UnavailableReason != ReviewBaseUnavailableNotManagedWorktree {
		t.Fatalf("ReviewFacts = %+v", info.ReviewFacts)
	}
	if info.Attention.State != AttentionDone {
		t.Fatalf("Attention = %+v, want done", info.Attention)
	}
}

func TestManager_RefreshReviewDoesNotGuessPathForProtocolV4Base(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := &Session{
		ID: "protocol-v4", Description: "old managed record", WorkDir: t.TempDir(),
		Status: StatusIdle, Fleet: DefaultFleet,
		ReviewBase: ReviewBase{RequestedRef: "origin/main", CommitOID: testReviewBaseOID},
		Attention:  Attention{State: AttentionDone, Generation: 1},
	}
	mgr.mu.Lock()
	mgr.sessions[sess.ID] = sess
	mgr.mu.Unlock()
	if err := mgr.store.Save(*sess); err != nil {
		t.Fatal(err)
	}

	info, err := mgr.RefreshReview(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.ReviewFacts.UnavailableReason != ReviewUnavailableWorktreePath {
		t.Fatalf("ReviewFacts = %+v", info.ReviewFacts)
	}
}

func TestManager_RefreshReviewWithNoDeltaStaysDone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, base := reviewTestRepo(t)
	runReviewTestGit(t, repo, "update-ref", "refs/remotes/origin/main", base)
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir: repo, Description: "no delta", Worktree: true, WorktreeBase: "main", NoHook: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", sess.WorkDir).Run()
	})
	mgr.mu.Lock()
	live := mgr.sessions[sess.ID]
	live.Attention = Attention{State: AttentionDone, Generation: 1}
	live.Status = StatusIdle
	mgr.mu.Unlock()

	info, err := mgr.RefreshReview(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.ReviewFacts.Status != ReviewFactsAvailable || info.ReviewFacts.ChangedFiles != 0 {
		t.Fatalf("ReviewFacts = %+v", info.ReviewFacts)
	}
	if info.Attention.State != AttentionDone {
		t.Fatalf("Attention = %+v, want done", info.Attention)
	}
}

func TestManager_CompletionAssessesManagedWorktreeAsynchronously(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, base := reviewTestRepo(t)
	runReviewTestGit(t, repo, "update-ref", "refs/remotes/origin/main", base)
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir: repo, Description: "automatic review", Worktree: true, WorktreeBase: "main", NoHook: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", sess.WorkDir).Run()
	})
	if err := os.WriteFile(filepath.Join(sess.WorkDir, "change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	deadline := time.Now().Add(3 * time.Second)
	for {
		info, ok := mgr.GetInfo(sess.ID)
		if !ok {
			t.Fatal("session disappeared")
		}
		if info.ReviewFacts.Status == ReviewFactsAvailable {
			if info.Attention.State != AttentionReadyForReview || info.ReviewFacts.ChangedFiles != 1 {
				t.Fatalf("completed assessment: attention=%+v facts=%+v", info.Attention, info.ReviewFacts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("review did not settle: %+v", info.ReviewFacts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func reviewTestRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runReviewTestGit(t, repo, "init")
	runReviewTestGit(t, repo, "config", "user.email", "test@example.com")
	runReviewTestGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runReviewTestGit(t, repo, "add", "--", "base.txt")
	runReviewTestGit(t, repo, "commit", "-m", "base")
	return repo, runReviewTestGit(t, repo, "rev-parse", "HEAD")
}

func runReviewTestGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
