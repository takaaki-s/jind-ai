package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckReport_CurrentFailureAndStalenessDriveAttention(t *testing.T) {
	facts := ReviewFacts{
		Status: ReviewFactsAvailable, AttentionGeneration: 2,
		WorkspaceFingerprint: "workspace-a", ChangedFiles: 1,
	}
	report := CheckReport{
		Source: CheckSourceReported, Status: CheckStatusFailed,
		WorkspaceFingerprint: "workspace-a", ReportedAt: time.Unix(20, 0),
	}
	attention := Attention{State: AttentionReadyForReview, Generation: 2}

	failed := reconcileCheckAttention(attention, facts, report)
	if failed.State != AttentionChecksFailed || !failed.Unseen() {
		t.Fatalf("current failed report attention = %+v", failed)
	}
	if got := report.toInfo(facts); got.Stale {
		t.Fatalf("current report projected stale: %+v", got)
	}

	facts.WorkspaceFingerprint = "workspace-b"
	ready := reconcileCheckAttention(failed, facts, report)
	if ready.State != AttentionReadyForReview {
		t.Fatalf("stale failed report attention = %+v, want ready-for-review", ready)
	}
	if got := report.toInfo(facts); !got.Stale {
		t.Fatalf("changed workspace report = %+v, want stale", got)
	}

	facts.Status = ReviewFactsUnavailable
	done := reconcileCheckAttention(failed, facts, report)
	if done.State != AttentionDone {
		t.Fatalf("unknown workspace attention = %+v, want done", done)
	}
}

func TestMergeCheckReport_PrefersLatestObservation(t *testing.T) {
	older := CheckReport{Status: CheckStatusFailed, ReportedAt: time.Unix(10, 0)}
	newer := CheckReport{Status: CheckStatusPassed, ReportedAt: time.Unix(20, 0)}
	if got := mergeCheckReport(older, newer); got != newer {
		t.Fatalf("mergeCheckReport = %+v, want newer %+v", got, newer)
	}
	if got := mergeCheckReport(newer, older); got != newer {
		t.Fatalf("reverse mergeCheckReport = %+v, want newer %+v", got, newer)
	}
}

func TestManager_ReportChecksBindsFingerprintAndRefreshMakesItStale(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, base := reviewTestRepo(t)
	runReviewTestGit(t, repo, "update-ref", "refs/remotes/origin/main", base)
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir: repo, Description: "reported checks", Worktree: true, WorktreeBase: "main", NoHook: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", sess.WorkDir).Run()
	})
	changedPath := filepath.Join(sess.WorkDir, "change.txt")
	if err := os.WriteFile(changedPath, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	live := mgr.sessions[sess.ID]
	live.Attention = Attention{State: AttentionDone, Generation: 1}
	live.Status = StatusIdle
	mgr.mu.Unlock()

	failed, err := mgr.ReportChecks(sess.ID, CheckStatusFailed)
	if err != nil {
		t.Fatalf("ReportChecks(failed): %v", err)
	}
	if failed.CheckReport.Status != CheckStatusFailed || failed.CheckReport.Stale ||
		failed.CheckReport.Source != CheckSourceReported {
		t.Fatalf("failed CheckReport = %+v", failed.CheckReport)
	}
	if failed.Attention.State != AttentionChecksFailed {
		t.Fatalf("failed Attention = %+v", failed.Attention)
	}
	oldFingerprint := failed.CheckReport.WorkspaceFingerprint

	// Same path and line counts, different bytes: only the content-bound
	// fingerprint proves the old externally-run checks are stale.
	if err := os.WriteFile(changedPath, []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refreshed, err := mgr.RefreshReview(sess.ID)
	if err != nil {
		t.Fatalf("RefreshReview: %v", err)
	}
	if refreshed.ReviewFacts.WorkspaceFingerprint == oldFingerprint {
		t.Fatal("workspace fingerprint did not change")
	}
	if !refreshed.CheckReport.Stale || refreshed.Attention.State != AttentionReadyForReview {
		t.Fatalf("refreshed = checks:%+v attention:%+v", refreshed.CheckReport, refreshed.Attention)
	}

	passed, err := mgr.ReportChecks(sess.ID, CheckStatusPassed)
	if err != nil {
		t.Fatalf("ReportChecks(passed): %v", err)
	}
	if passed.CheckReport.Status != CheckStatusPassed || passed.CheckReport.Stale {
		t.Fatalf("passed CheckReport = %+v", passed.CheckReport)
	}
	if passed.Attention.State != AttentionReadyForReview {
		t.Fatalf("passed Attention = %+v", passed.Attention)
	}

	loaded, err := mgr.store.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CheckReport.Status != CheckStatusPassed || loaded.Attention.State != AttentionReadyForReview {
		t.Fatalf("persisted checks=%+v attention=%+v", loaded.CheckReport, loaded.Attention)
	}
}

func TestManager_ReportChecksRequiresCompletedTurn(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "no completion")
	if _, err := mgr.ReportChecks(sess.ID, CheckStatusPassed); err == nil {
		t.Fatal("ReportChecks accepted a session without a completed turn")
	}
}

func TestStoreSave_CheckReportAndAttentionDoNotRegress(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	facts := ReviewFacts{
		Status: ReviewFactsAvailable, AttentionGeneration: 1,
		WorkspaceFingerprint: "workspace", ChangedFiles: 1, ObservedAt: time.Unix(5, 0),
	}
	newer := Session{
		ID: "checks", ReviewFacts: facts,
		Attention: Attention{State: AttentionReadyForReview, Generation: 1},
		CheckReport: CheckReport{
			Source: CheckSourceReported, Status: CheckStatusPassed,
			WorkspaceFingerprint: "workspace", ReportedAt: time.Unix(20, 0),
		},
	}
	if err := store.Save(newer); err != nil {
		t.Fatal(err)
	}
	stale := newer
	stale.Attention.State = AttentionChecksFailed
	stale.CheckReport.Status = CheckStatusFailed
	stale.CheckReport.ReportedAt = time.Unix(10, 0)
	if err := store.Save(stale); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(newer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CheckReport.Status != CheckStatusPassed || loaded.Attention.State != AttentionReadyForReview {
		t.Fatalf("loaded checks=%+v attention=%+v", loaded.CheckReport, loaded.Attention)
	}
}

func TestCheckReport_ZeroOmittedFromJSON(t *testing.T) {
	for name, value := range map[string]any{
		"session": Session{ID: "s1"},
		"info":    Info{ID: "s1"},
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) == "" || containsJSONKey(data, "check_report") {
			t.Errorf("%s zero JSON = %s", name, data)
		}
	}
}

func containsJSONKey(data []byte, key string) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return false
	}
	_, ok := object[key]
	return ok
}
