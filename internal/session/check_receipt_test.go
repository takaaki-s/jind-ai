package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func checkReceiptFixture(t *testing.T) (*Manager, string, string, CheckReportSubmission) {
	t.Helper()
	repo, base := reviewTestRepo(t)
	runReviewTestGit(t, repo, "update-ref", "refs/remotes/origin/main", base)
	mgr, _, _ := newTestManager(t)
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: repo, Worktree: true, WorktreeBase: "main", NoHook: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", sess.WorkDir).Run() })
	path := filepath.Join(sess.WorkDir, "change.txt")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	mgr.sessions[sess.ID].Status = StatusIdle
	mgr.sessions[sess.ID].Attention = Attention{State: AttentionDone, Generation: 1, SeenGeneration: 1}
	mgr.mu.Unlock()
	info, err := mgr.RefreshReview(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	s := CheckReportSubmission{Status: CheckStatusPassed, WorkspaceFingerprint: info.ReviewFacts.WorkspaceFingerprint,
		CheckReportMetadata: CheckReportMetadata{IdempotencyKey: "run-1", Name: "unit", Reporter: "local-user", Summary: "tests passed", StartedAt: time.Unix(10, 0).UTC(), FinishedAt: time.Unix(20, 0).UTC()},
	}
	return mgr, sess.ID, path, s
}

func TestRecordChecksRetriesDoNotReplaceNewerReportAfterRestart(t *testing.T) {
	mgr, id, _, submission := checkReceiptFixture(t)
	first, err := mgr.RecordChecks(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reused || first.Stale || first.Session.CheckReceipt != first.Receipt || first.Session.Attention.Unseen {
		t.Fatalf("first receipt = %+v", first)
	}
	secondSubmission := submission
	secondSubmission.IdempotencyKey = "run-2"
	secondSubmission.Status = CheckStatusFailed
	second, err := mgr.RecordChecks(id, secondSubmission)
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
	retry, err := mgr.RecordChecks(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Reused || retry.Receipt != first.Receipt || retry.Session.CheckReport.Status != CheckStatusFailed || retry.Session.CheckReceipt != second.Receipt {
		t.Fatalf("retry replaced result: %+v", retry)
	}
	conflict := submission
	conflict.Summary = "different"
	if _, err := mgr.RecordChecks(id, conflict); err == nil {
		t.Fatal("same key accepted changed payload")
	}
	loaded, err = mgr.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.CheckReceipts) != 2 {
		t.Fatalf("receipts = %d", len(loaded.CheckReceipts))
	}
	legacy, err := mgr.ReportChecks(id, CheckStatusPassed)
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.CheckReceipt.IsZero() {
		t.Fatal("legacy report inherited metadata from a different run")
	}
	retry, err = mgr.RecordChecks(id, secondSubmission)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Reused || retry.Receipt != second.Receipt || retry.Session.CheckReport.Status != CheckStatusPassed || !retry.Session.CheckReceipt.IsZero() {
		t.Fatalf("receipt retry replaced legacy report: %+v", retry)
	}
}

func TestRecordChecksRejectsOldFingerprintWithoutRebinding(t *testing.T) {
	mgr, id, path, submission := checkReceiptFixture(t)
	first, err := mgr.RecordChecks(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := submission
	next.IdempotencyKey = "run-2"
	if _, err := mgr.RecordChecks(id, next); err == nil {
		t.Fatal("accepted old fingerprint for changed workspace")
	}
	retry, err := mgr.RecordChecks(id, submission)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Reused || !retry.Stale || retry.Receipt != first.Receipt || !retry.Session.CheckReport.Stale {
		t.Fatalf("old receipt rebound: %+v", retry)
	}
	next.WorkspaceFingerprint = retry.Session.ReviewFacts.WorkspaceFingerprint
	if _, err := mgr.RecordChecks(id, next); err != nil {
		t.Fatal(err)
	}
}

func TestRecordChecksConcurrentRetriesShareDurableReceipt(t *testing.T) {
	mgr, id, _, submission := checkReceiptFixture(t)
	submission.Status = CheckStatusFailed
	var wg sync.WaitGroup
	results := make(chan CheckReportRecordResult, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := mgr.RecordChecks(id, submission)
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
	if len(loaded.CheckReceipts) != 1 {
		t.Fatalf("receipts = %d", len(loaded.CheckReceipts))
	}
	for result := range results {
		if result.Receipt != loaded.CheckReceipts[0] {
			t.Fatal("retry changed receipt")
		}
		if result.Session.Attention.CheckFailureGeneration != 1 {
			t.Fatal("concurrent retry advanced failure cursor")
		}
	}
}

func TestCheckReceiptsSurviveStaleSavesAndLimitDoesNotEvict(t *testing.T) {
	mgr, id, _, submission := checkReceiptFixture(t)
	stale, err := mgr.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	first, err := mgr.RecordChecks(id, submission)
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
	if len(loaded.CheckReceipts) != 1 || loaded.CheckReceipts[0] != first.Receipt {
		t.Fatal("stale snapshot lost receipt")
	}
	mgr.mu.Lock()
	full := make([]CheckReportReceipt, CheckReceiptLimit)
	for i := range full {
		full[i] = first.Receipt
		full[i].IdempotencyKey = strings.Repeat("x", i+1)
	}
	full[0] = first.Receipt
	mgr.sessions[id].CheckReceipts = full
	mgr.mu.Unlock()
	next := submission
	next.IdempotencyKey = "new-key"
	if _, err := mgr.RecordChecks(id, next); err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("full journal error = %v", err)
	}
	if _, err := mgr.RecordChecks(id, submission); err != nil {
		t.Fatalf("existing retry at capacity: %v", err)
	}
}

func TestCheckSubmissionValidation(t *testing.T) {
	valid := CheckReportSubmission{Status: CheckStatusPassed, WorkspaceFingerprint: strings.Repeat("a", 64), CheckReportMetadata: CheckReportMetadata{
		IdempotencyKey: "run", Name: "test", Reporter: "agent", StartedAt: time.Unix(1, 0), FinishedAt: time.Unix(2, 0),
	}}
	for name, mutate := range map[string]func(*CheckReportSubmission){
		"status":        func(s *CheckReportSubmission) { s.Status = "unknown" },
		"key":           func(s *CheckReportSubmission) { s.IdempotencyKey = "" },
		"name":          func(s *CheckReportSubmission) { s.Name = strings.Repeat("n", 129) },
		"reporter":      func(s *CheckReportSubmission) { s.Reporter = "\x1b[31m" },
		"summary":       func(s *CheckReportSubmission) { s.Summary = strings.Repeat("s", 2049) },
		"newline":       func(s *CheckReportSubmission) { s.Summary = "one\ntwo" },
		"bidi":          func(s *CheckReportSubmission) { s.Reporter = "actor\u202e" },
		"invalid utf8":  func(s *CheckReportSubmission) { s.Summary = string([]byte{0xff}) },
		"fingerprint":   func(s *CheckReportSubmission) { s.WorkspaceFingerprint = "old" },
		"missing time":  func(s *CheckReportSubmission) { s.StartedAt = time.Time{} },
		"reversed time": func(s *CheckReportSubmission) { s.FinishedAt = s.StartedAt.Add(-time.Second) },
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
