package session

import (
	"os"
	"strings"
	"sync"
	"testing"
)

func reviewReceiptFixture(t *testing.T) (*Manager, string, string, ReviewDispositionSubmission) {
	t.Helper()
	m, id, path, check := checkReceiptFixture(t)
	return m, id, path, ReviewDispositionSubmission{Decision: ReviewDecisionReviewed,
		WorkspaceFingerprint:      check.WorkspaceFingerprint,
		ReviewDispositionMetadata: ReviewDispositionMetadata{IdempotencyKey: "review-1", Actor: "local-user", Note: "inspected diff"}}
}

func TestReviewSubmissionValidation(t *testing.T) {
	valid := ReviewDispositionSubmission{Decision: ReviewDecisionReviewed, WorkspaceFingerprint: strings.Repeat("a", 64),
		ReviewDispositionMetadata: ReviewDispositionMetadata{IdempotencyKey: "review-1", Actor: "reviewer"}}
	for name, mutate := range map[string]func(*ReviewDispositionSubmission){
		"decision":      func(s *ReviewDispositionSubmission) { s.Decision = "approved" },
		"empty key":     func(s *ReviewDispositionSubmission) { s.IdempotencyKey = " " },
		"long key":      func(s *ReviewDispositionSubmission) { s.IdempotencyKey = strings.Repeat("a", 129) },
		"empty actor":   func(s *ReviewDispositionSubmission) { s.Actor = "" },
		"long actor":    func(s *ReviewDispositionSubmission) { s.Actor = strings.Repeat("a", 129) },
		"control actor": func(s *ReviewDispositionSubmission) { s.Actor = "actor\n" },
		"format note":   func(s *ReviewDispositionSubmission) { s.Note = "\u202e" },
		"long note":     func(s *ReviewDispositionSubmission) { s.Note = strings.Repeat("a", 2049) },
		"invalid utf8":  func(s *ReviewDispositionSubmission) { s.Note = "\xff" },
		"fingerprint":   func(s *ReviewDispositionSubmission) { s.WorkspaceFingerprint = "old" },
		"uppercase":     func(s *ReviewDispositionSubmission) { s.WorkspaceFingerprint = strings.Repeat("A", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			s := valid
			mutate(&s)
			if s.Validate() == nil {
				t.Fatal("accepted invalid submission")
			}
		})
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewReceiptDoesNotAcknowledgeAndRequiresNonEmptyEvidence(t *testing.T) {
	m, id, path, submission := reviewReceiptFixture(t)
	m.mu.Lock()
	m.sessions[id].Attention.SeenGeneration = 0
	before := m.sessions[id].Attention
	m.mu.Unlock()
	result, err := m.RecordReviewDisposition(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if result.Session.Attention != before.toInfo() {
		t.Fatalf("review acknowledged attention: %+v", result.Session.Attention)
	}
	seen, err := m.MarkSeen(id)
	if err != nil {
		t.Fatal(err)
	}
	if seen.ReviewReceipt != result.Receipt {
		t.Fatal("seen changed review")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fresh, err := m.RefreshReview(id)
	if err != nil {
		t.Fatal(err)
	}
	submission.IdempotencyKey = "empty"
	submission.WorkspaceFingerprint = fresh.ReviewFacts.WorkspaceFingerprint
	if _, err := m.RecordReviewDisposition(id, submission); err == nil {
		t.Fatal("reviewed empty workspace")
	}
}

func TestRecordReviewDispositionRetriesDoNotReplaceNewerReportAfterRestart(t *testing.T) {
	mgr, id, _, submission := reviewReceiptFixture(t)
	first, err := mgr.RecordReviewDisposition(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reused || first.Stale || first.Session.ReviewReceipt != first.Receipt || first.Session.Attention.Unseen {
		t.Fatalf("first receipt = %+v", first)
	}
	secondSubmission := submission
	secondSubmission.IdempotencyKey = "run-2"
	secondSubmission.Decision = ReviewDecisionChangesRequested
	second, err := mgr.RecordReviewDisposition(id, secondSubmission)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := mgr.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	mgr.sessions[id] = loaded
	mgr.mu.Unlock()
	retry, err := mgr.RecordReviewDisposition(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Reused || retry.Receipt != first.Receipt || retry.Session.ReviewDisposition.Decision != ReviewDecisionChangesRequested || retry.Session.ReviewReceipt != second.Receipt {
		t.Fatalf("retry replaced result: %+v", retry)
	}
	conflict := submission
	conflict.Note = "different"
	if _, err := mgr.RecordReviewDisposition(id, conflict); err == nil {
		t.Fatal("same key accepted changed payload")
	}
	loaded, err = mgr.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.ReviewReceipts) != 2 {
		t.Fatalf("receipts = %d", len(loaded.ReviewReceipts))
	}
	legacy, err := mgr.ReportReviewDisposition(id, ReviewDecisionReviewed)
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.ReviewReceipt.IsZero() {
		t.Fatal("legacy report inherited metadata from a different run")
	}
	retry, err = mgr.RecordReviewDisposition(id, secondSubmission)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Reused || retry.Receipt != second.Receipt || retry.Session.ReviewDisposition.Decision != ReviewDecisionReviewed || !retry.Session.ReviewReceipt.IsZero() {
		t.Fatalf("receipt retry replaced legacy report: %+v", retry)
	}
}

func TestRecordReviewDispositionRejectsOldFingerprintWithoutRebinding(t *testing.T) {
	mgr, id, path, submission := reviewReceiptFixture(t)
	first, err := mgr.RecordReviewDisposition(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := submission
	next.IdempotencyKey = "run-2"
	if _, err := mgr.RecordReviewDisposition(id, next); err == nil {
		t.Fatal("accepted old fingerprint for changed workspace")
	}
	retry, err := mgr.RecordReviewDisposition(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Reused || !retry.Stale || retry.Receipt != first.Receipt || !retry.Session.ReviewDisposition.Stale {
		t.Fatalf("old receipt rebound: %+v", retry)
	}
	next.WorkspaceFingerprint = retry.Session.ReviewFacts.WorkspaceFingerprint
	if _, err := mgr.RecordReviewDisposition(id, next); err != nil {
		t.Fatal(err)
	}
}

func TestRecordReviewDispositionConcurrentRetriesShareDurableReceipt(t *testing.T) {
	mgr, id, _, submission := reviewReceiptFixture(t)
	submission.Decision = ReviewDecisionChangesRequested
	var wg sync.WaitGroup
	results := make(chan ReviewDispositionRecordResult, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := mgr.RecordReviewDisposition(id, submission)
			if err != nil {
				t.Error(err)
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	loaded, err := mgr.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.ReviewReceipts) != 1 {
		t.Fatalf("receipts = %d", len(loaded.ReviewReceipts))
	}
	for result := range results {
		if result.Receipt != loaded.ReviewReceipts[0] {
			t.Fatal("retry changed receipt")
		}
	}
}

func TestReviewReceiptsSurviveStaleSavesAndLimitDoesNotEvict(t *testing.T) {
	mgr, id, _, submission := reviewReceiptFixture(t)
	stale, err := mgr.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	first, err := mgr.RecordReviewDisposition(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.store.Save(*stale); err != nil {
		t.Fatal(err)
	}
	loaded, err := mgr.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.ReviewReceipts) != 1 || loaded.ReviewReceipts[0] != first.Receipt {
		t.Fatal("stale snapshot lost receipt")
	}
	mgr.mu.Lock()
	full := make([]ReviewDispositionReceipt, ReviewReceiptLimit)
	for i := range full {
		full[i] = first.Receipt
		full[i].IdempotencyKey = strings.Repeat("x", i+1)
	}
	full[0] = first.Receipt
	mgr.sessions[id].ReviewReceipts = full
	mgr.mu.Unlock()
	next := submission
	next.IdempotencyKey = "new-key"
	if _, err := mgr.RecordReviewDisposition(id, next); err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("full journal error = %v", err)
	}
	if _, err := mgr.RecordReviewDisposition(id, submission); err != nil {
		t.Fatalf("existing retry at capacity: %v", err)
	}
}
