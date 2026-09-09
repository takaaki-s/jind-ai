package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagerPRHandoff_DryRunExecuteAndIdempotentSuccess(t *testing.T) {
	mgr, sess := preparedPRHandoffSession(t)
	target := PRHandoffTarget{Plugin: "fake-pr", Action: "create"}

	payload, state, run, err := mgr.BeginPRHandoff(sess.ID, target, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if run || !state.IsZero() {
		t.Fatalf("dry run = state %+v run %v, want no mutation", state, run)
	}
	if payload.IdempotencyKey == "" || payload.Review.Branch == "" || payload.Review.CommitCount != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	encoded, _ := json.Marshal(payload)
	if strings.Contains(string(encoded), sess.WorkDir) || strings.Contains(string(encoded), "change.txt") {
		t.Fatalf("payload leaked path or filename: %s", encoded)
	}

	_, running, run, err := mgr.BeginPRHandoff(sess.ID, target, payload.IdempotencyKey, false)
	if err != nil || !run || running.Status != PRHandoffRunning {
		t.Fatalf("begin = state %+v run %v err %v", running, run, err)
	}
	_, duplicate, run, err := mgr.BeginPRHandoff(sess.ID, target, payload.IdempotencyKey, false)
	if err != nil || run || duplicate.Status != PRHandoffRunning {
		t.Fatalf("running duplicate = state %+v run %v err %v", duplicate, run, err)
	}

	result := PRHandoffProviderResult{Status: PRHandoffSucceeded, Provider: "fake", ID: "42", URL: "https://example.test/pulls/42"}
	info, err := mgr.FinishPRHandoff(sess.ID, payload.IdempotencyKey, result, nil)
	if err != nil || info.PRHandoff.Status != PRHandoffSucceeded {
		t.Fatalf("finish = %+v, %v", info.PRHandoff, err)
	}
	_, duplicate, run, err = mgr.BeginPRHandoff(sess.ID, target, payload.IdempotencyKey, false)
	if err != nil || run || duplicate.Status != PRHandoffSucceeded {
		t.Fatalf("successful duplicate = state %+v run %v err %v", duplicate, run, err)
	}
	loaded, err := mgr.store.Load(sess.ID)
	if err != nil || loaded.PRHandoff.Status != PRHandoffSucceeded {
		t.Fatalf("persisted = %+v, %v", loaded.PRHandoff, err)
	}
	restarted, _, _ := newTestManagerIn(t, mgr.store.dataDir, testIdentity())
	restartedInfo, ok := restarted.GetInfo(sess.ID)
	if !ok || restartedInfo.PRHandoff.Status != PRHandoffSucceeded || restartedInfo.PRHandoff.Result.URL != result.URL {
		t.Fatalf("restart projection = %+v, found=%v", restartedInfo.PRHandoff, ok)
	}
	if _, _, _, err := mgr.BeginPRHandoff(sess.ID, target, "different-key", false); err == nil || !strings.Contains(err.Error(), "already handed off") {
		t.Fatalf("different key after success error = %v", err)
	}
}

func TestManagerPRHandoff_UnknownCanRetrySameKey(t *testing.T) {
	mgr, sess := preparedPRHandoffSession(t)
	target := PRHandoffTarget{Plugin: "fake-pr", Action: "create"}
	payload, _, run, err := mgr.BeginPRHandoff(sess.ID, target, "retry-key", false)
	if err != nil || !run {
		t.Fatalf("begin: run=%v err=%v", run, err)
	}
	info, err := mgr.FinishPRHandoff(sess.ID, payload.IdempotencyKey, PRHandoffProviderResult{}, contextDeadlineError{})
	if err != nil || info.PRHandoff.Status != PRHandoffUnknown {
		t.Fatalf("unknown finish = %+v, %v", info.PRHandoff, err)
	}
	_, retried, run, err := mgr.BeginPRHandoff(sess.ID, target, "retry-key", false)
	if err != nil || !run || retried.Status != PRHandoffRunning {
		t.Fatalf("retry = %+v run=%v err=%v", retried, run, err)
	}
}

func TestManagerPRHandoff_UnknownRequiresSameKey(t *testing.T) {
	mgr, sess := preparedPRHandoffSession(t)
	target := PRHandoffTarget{Plugin: "fake-pr", Action: "create"}
	if _, _, _, err := mgr.BeginPRHandoff(sess.ID, target, "original-key", false); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.FinishPRHandoff(sess.ID, "original-key", PRHandoffProviderResult{}, contextDeadlineError{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := mgr.BeginPRHandoff(sess.ID, target, "new-key", false); err == nil || !strings.Contains(err.Error(), "existing idempotency key") {
		t.Fatalf("new key after unknown error = %v", err)
	}
}

type contextDeadlineError struct{}

func (contextDeadlineError) Error() string { return "provider timeout; outcome unknown" }

func TestManagerPRHandoff_FailsClosedOnDirtyReviewedWorkspace(t *testing.T) {
	mgr, sess := preparedPRHandoffSession(t)
	dirty := filepath.Join(sess.WorkDir, "dirty.txt")
	if err := os.WriteFile(dirty, []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ReportReviewDisposition(sess.ID, ReviewDecisionReviewed); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := mgr.BeginPRHandoff(sess.ID, PRHandoffTarget{Plugin: "fake", Action: "create"}, "", true)
	if err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty preflight error = %v", err)
	}
}

func TestManagerPRHandoff_SeenAloneIsNotReview(t *testing.T) {
	mgr, sess := preparedPRHandoffSession(t)
	mgr.mu.Lock()
	mgr.sessions[sess.ID].Attention.SeenGeneration = mgr.sessions[sess.ID].Attention.Generation
	mgr.sessions[sess.ID].ReviewDisposition = ReviewDisposition{}
	mgr.mu.Unlock()

	_, _, _, err := mgr.BeginPRHandoff(sess.ID, PRHandoffTarget{Plugin: "fake", Action: "create"}, "", true)
	if err == nil || !strings.Contains(err.Error(), "current reviewed disposition") {
		t.Fatalf("seen-only error = %v", err)
	}
}

func TestManagerPRHandoff_FailsClosedOnStaleReviewDisposition(t *testing.T) {
	mgr, sess := preparedPRHandoffSession(t)
	if err := os.WriteFile(filepath.Join(sess.WorkDir, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runReviewTestGit(t, sess.WorkDir, "add", "--", "later.txt")
	runReviewTestGit(t, sess.WorkDir, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "-m", "later")

	_, _, _, err := mgr.BeginPRHandoff(sess.ID, PRHandoffTarget{Plugin: "fake", Action: "create"}, "", true)
	if err == nil || !strings.Contains(err.Error(), "current reviewed disposition") {
		t.Fatalf("stale disposition error = %v", err)
	}
}

func TestManagerPRHandoff_FailsClosedOnReportedCheckFailure(t *testing.T) {
	mgr, sess := preparedPRHandoffSession(t)
	if _, err := mgr.ReportChecks(sess.ID, CheckStatusFailed); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := mgr.BeginPRHandoff(sess.ID, PRHandoffTarget{Plugin: "fake", Action: "create"}, "", true)
	if err == nil || !strings.Contains(err.Error(), "reported checks failed") {
		t.Fatalf("failed-check error = %v", err)
	}
}

func TestValidatePRHandoffProviderResult(t *testing.T) {
	valid := PRHandoffProviderResult{Status: PRHandoffSucceeded, ID: "1", URL: "https://example.test/pulls/1"}
	if err := ValidatePRHandoffProviderResult(valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.URL = "https://token@example.test/pulls/1?secret=yes"
	if err := ValidatePRHandoffProviderResult(invalid); err == nil {
		t.Fatal("credential/query URL should be rejected")
	}
}

func preparedPRHandoffSession(t *testing.T) (*Manager, *Session) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, base := reviewTestRepo(t)
	runReviewTestGit(t, repo, "update-ref", "refs/remotes/origin/main", base)
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir: repo, Description: "handoff", Worktree: true, WorktreeBase: "main", NoHook: true,
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
	runReviewTestGit(t, sess.WorkDir, "add", "--", "change.txt")
	runReviewTestGit(t, sess.WorkDir, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "-m", "change")
	mgr.mu.Lock()
	mgr.sessions[sess.ID].Attention = Attention{State: AttentionDone, Generation: 1}
	mgr.sessions[sess.ID].Status = StatusIdle
	mgr.mu.Unlock()
	if _, err := mgr.ReportReviewDisposition(sess.ID, ReviewDecisionReviewed); err != nil {
		t.Fatal(err)
	}
	return mgr, sess
}

func TestMergePRHandoff_DoesNotRestoreRunning(t *testing.T) {
	now := time.Unix(10, 0)
	running := PRHandoff{Status: PRHandoffRunning, UpdatedAt: now}
	done := PRHandoff{Status: PRHandoffSucceeded, UpdatedAt: now}
	if got := mergePRHandoff(done, running); got.Status != PRHandoffSucceeded {
		t.Fatalf("merge = %+v", got)
	}
}

func TestStoreSave_PRHandoffDoesNotRegress(t *testing.T) {
	store, _ := newTestStore(t)
	now := time.Unix(20, 0)
	completed := Session{ID: "handoff", PRHandoff: PRHandoff{
		IdempotencyKey: "stable-key", Target: PRHandoffTarget{Plugin: "fake", Action: "create"},
		WorkspaceFingerprint: "workspace", Status: PRHandoffSucceeded,
		Result:    PRHandoffProviderResult{Status: PRHandoffSucceeded, ID: "42"},
		StartedAt: now.Add(-time.Second), UpdatedAt: now,
	}}
	if err := store.Save(completed); err != nil {
		t.Fatal(err)
	}
	stale := completed
	stale.PRHandoff.Status = PRHandoffRunning
	stale.PRHandoff.Result = PRHandoffProviderResult{}
	if err := store.Save(stale); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PRHandoff.Status != PRHandoffSucceeded || loaded.PRHandoff.Result.ID != "42" {
		t.Fatalf("loaded PR handoff = %+v", loaded.PRHandoff)
	}
}

func TestManagerRestart_MarksRunningPRHandoffUnknown(t *testing.T) {
	store, dir := newTestStore(t)
	running := Session{ID: "interrupted", PRHandoff: PRHandoff{
		IdempotencyKey: "stable-key", Target: PRHandoffTarget{Plugin: "fake", Action: "create"},
		WorkspaceFingerprint: "workspace", Status: PRHandoffRunning,
		StartedAt: time.Unix(20, 0), UpdatedAt: time.Unix(20, 0),
	}}
	if err := store.Save(running); err != nil {
		t.Fatal(err)
	}
	restarted, _, _ := newTestManagerIn(t, dir, testIdentity())
	info, ok := restarted.GetInfo(running.ID)
	if !ok || info.PRHandoff.Status != PRHandoffUnknown || !strings.Contains(info.PRHandoff.Error, "outcome is unknown") {
		t.Fatalf("restarted PR handoff = %+v, found=%v", info.PRHandoff, ok)
	}
	persisted, err := store.Load(running.ID)
	if err != nil || persisted.PRHandoff.Status != PRHandoffUnknown {
		t.Fatalf("persisted PR handoff = %+v, err=%v", persisted.PRHandoff, err)
	}
}

func TestPRHandoff_ZeroOmittedFromJSON(t *testing.T) {
	for name, value := range map[string]any{"session": Session{ID: "s1"}, "info": Info{ID: "s1"}} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if containsJSONKey(data, "pr_handoff") {
			t.Errorf("%s zero JSON = %s", name, data)
		}
	}
}
