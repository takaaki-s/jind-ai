package session

import (
	"os"
	"testing"
)

func TestCheckFailureReceiptAttention(t *testing.T) {
	mgr, id, path, submission := checkReceiptFixture(t)
	submission.Status = CheckStatusFailed
	first, err := mgr.RecordChecks(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Session.Attention.Unseen || first.Session.Attention.CheckFailureGeneration != 1 || first.Session.Attention.Generation != 1 || first.Session.ReviewFacts.AttentionGeneration != 1 {
		t.Fatalf("first: %+v", first.Session)
	}
	seen, err := mgr.MarkSeen(id)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Attention.Unseen || seen.Attention.SeenCheckFailureGeneration != 1 {
		t.Fatalf("seen: %+v", seen.Attention)
	}
	loaded, err := mgr.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	stale := *loaded
	mgr.mu.Lock()
	mgr.sessions[id] = loaded
	mgr.mu.Unlock()
	retry, err := mgr.RecordChecks(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Session.Attention != seen.Attention {
		t.Fatalf("retry changed attention: %+v", retry.Session.Attention)
	}
	submission.IdempotencyKey = "new-run"
	next, err := mgr.RecordChecks(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Session.Attention.Unseen || next.Session.Attention.CheckFailureGeneration != 2 || next.Session.Status != seen.Status {
		t.Fatalf("next: %+v", next.Session)
	}
	if err := mgr.store.Save(stale); err != nil {
		t.Fatal(err)
	}
	loaded, err = mgr.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Attention.Unseen() || loaded.Attention.CheckFailureGeneration != 2 || loaded.Attention.SeenCheckFailureGeneration != 1 {
		t.Fatalf("stale save: %+v", loaded.Attention)
	}
	if err := os.WriteFile(path, []byte("new content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refreshed, err := mgr.RefreshReview(id)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Attention.Unseen || refreshed.Attention.State != AttentionReadyForReview {
		t.Fatalf("stale failure: %+v", refreshed.Attention)
	}
	submission.IdempotencyKey = "new-workspace"
	submission.WorkspaceFingerprint = refreshed.ReviewFacts.WorkspaceFingerprint
	next, err = mgr.RecordChecks(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Session.Attention.Unseen || next.Session.Attention.CheckFailureGeneration != 3 {
		t.Fatalf("new workspace: %+v", next.Session.Attention)
	}
	submission.IdempotencyKey = "recovery"
	submission.Status = CheckStatusPassed
	next, err = mgr.RecordChecks(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if next.Session.Attention.Unseen || next.Session.Attention.State != AttentionReadyForReview {
		t.Fatalf("pass: %+v", next.Session.Attention)
	}
}

func TestLegacyFailureAttentionDeduplicatesConsecutiveFailures(t *testing.T) {
	mgr, id, path, _ := checkReceiptFixture(t)
	for i := 0; i < 2; i++ {
		info, err := mgr.ReportChecks(id, CheckStatusFailed)
		if err != nil {
			t.Fatal(err)
		}
		if info.Attention.CheckFailureGeneration != 1 || info.Attention.Unseen != (i == 0) {
			t.Fatalf("report %d: %+v", i, info.Attention)
		}
		if _, err := mgr.MarkSeen(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("new content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := mgr.ReportChecks(id, CheckStatusFailed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Attention.CheckFailureGeneration != 2 || !info.Attention.Unseen {
		t.Fatalf("changed: %+v", info.Attention)
	}
}
