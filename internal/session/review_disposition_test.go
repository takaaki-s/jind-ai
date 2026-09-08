package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewDisposition_CurrentAndStaleProjection(t *testing.T) {
	facts := ReviewFacts{Status: ReviewFactsAvailable, WorkspaceFingerprint: "workspace-a", ChangedFiles: 1}
	disposition := ReviewDisposition{
		Source: ReviewDispositionSourceReported, Decision: ReviewDecisionReviewed,
		WorkspaceFingerprint: "workspace-a", ReportedAt: time.Unix(20, 0),
	}
	if got := disposition.toInfo(facts); got.Stale {
		t.Fatalf("matching disposition projected stale: %+v", got)
	}
	facts.WorkspaceFingerprint = "workspace-b"
	if got := disposition.toInfo(facts); !got.Stale {
		t.Fatalf("changed workspace disposition = %+v, want stale", got)
	}
}

func TestMergeReviewDisposition_PrefersLatestDecision(t *testing.T) {
	older := ReviewDisposition{Decision: ReviewDecisionChangesRequested, ReportedAt: time.Unix(10, 0)}
	newer := ReviewDisposition{Decision: ReviewDecisionReviewed, ReportedAt: time.Unix(20, 0)}
	if got := mergeReviewDisposition(older, newer); got != newer {
		t.Fatalf("mergeReviewDisposition = %+v, want newer %+v", got, newer)
	}
	if got := mergeReviewDisposition(newer, older); got != newer {
		t.Fatalf("reverse mergeReviewDisposition = %+v, want newer %+v", got, newer)
	}
}

func TestManager_ReportReviewDispositionBindsFingerprintWithoutMarkingSeen(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, base := reviewTestRepo(t)
	runReviewTestGit(t, repo, "update-ref", "refs/remotes/origin/main", base)
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.CreateWithOptions(CreateOptions{
		WorkDir: repo, Description: "human review", Worktree: true, WorktreeBase: "main", NoHook: true,
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
	if _, err := mgr.ReportReviewDisposition(sess.ID, ReviewDecisionReviewed); err == nil {
		t.Fatal("ReportReviewDisposition accepted an empty workspace delta")
	}

	changedPath := filepath.Join(sess.WorkDir, "change.txt")
	if err := os.WriteFile(changedPath, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reviewed, err := mgr.ReportReviewDisposition(sess.ID, ReviewDecisionReviewed)
	if err != nil {
		t.Fatalf("ReportReviewDisposition(reviewed): %v", err)
	}
	if reviewed.ReviewDisposition.Decision != ReviewDecisionReviewed || reviewed.ReviewDisposition.Stale ||
		reviewed.ReviewDisposition.Source != ReviewDispositionSourceReported {
		t.Fatalf("ReviewDisposition = %+v", reviewed.ReviewDisposition)
	}
	if !reviewed.Attention.Unseen || reviewed.Attention.SeenGeneration != 0 {
		t.Fatalf("recording review changed seen receipt: %+v", reviewed.Attention)
	}
	oldFingerprint := reviewed.ReviewDisposition.WorkspaceFingerprint

	// The decision is about content, not counts or paths. Equal-length bytes at
	// the same path must therefore invalidate it.
	if err := os.WriteFile(changedPath, []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refreshed, err := mgr.RefreshReview(sess.ID)
	if err != nil {
		t.Fatalf("RefreshReview: %v", err)
	}
	if refreshed.ReviewFacts.WorkspaceFingerprint == oldFingerprint || !refreshed.ReviewDisposition.Stale {
		t.Fatalf("refreshed facts=%+v disposition=%+v", refreshed.ReviewFacts, refreshed.ReviewDisposition)
	}

	requested, err := mgr.ReportReviewDisposition(sess.ID, ReviewDecisionChangesRequested)
	if err != nil {
		t.Fatalf("ReportReviewDisposition(changes-requested): %v", err)
	}
	if requested.ReviewDisposition.Decision != ReviewDecisionChangesRequested || requested.ReviewDisposition.Stale {
		t.Fatalf("updated ReviewDisposition = %+v", requested.ReviewDisposition)
	}
	loaded, err := mgr.store.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ReviewDisposition.Decision != ReviewDecisionChangesRequested {
		t.Fatalf("persisted ReviewDisposition = %+v", loaded.ReviewDisposition)
	}
}

func TestManager_ReportReviewDispositionRequiresCompletedTurn(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "no completion")
	if _, err := mgr.ReportReviewDisposition(sess.ID, ReviewDecisionReviewed); err == nil {
		t.Fatal("ReportReviewDisposition accepted a session without a completed turn")
	}
}

func TestStoreSave_ReviewDispositionDoesNotRegress(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	newer := Session{ID: "review", ReviewDisposition: ReviewDisposition{
		Source: ReviewDispositionSourceReported, Decision: ReviewDecisionReviewed,
		WorkspaceFingerprint: "workspace", ReportedAt: time.Unix(20, 0),
	}}
	if err := store.Save(newer); err != nil {
		t.Fatal(err)
	}
	stale := newer
	stale.ReviewDisposition.Decision = ReviewDecisionChangesRequested
	stale.ReviewDisposition.ReportedAt = time.Unix(10, 0)
	if err := store.Save(stale); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(newer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ReviewDisposition.Decision != ReviewDecisionReviewed {
		t.Fatalf("loaded ReviewDisposition = %+v", loaded.ReviewDisposition)
	}
}

func TestReviewDisposition_ZeroOmittedFromJSON(t *testing.T) {
	for name, value := range map[string]any{"session": Session{ID: "s1"}, "info": Info{ID: "s1"}} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if containsJSONKey(data, "review_disposition") {
			t.Errorf("%s zero JSON = %s", name, data)
		}
	}
}
