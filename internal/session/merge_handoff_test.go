package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testMergeCommit = "cccccccccccccccccccccccccccccccccccccccc"

func TestManagerMergeHandoff_PreflightExecuteAndDuplicate(t *testing.T) {
	mgr, sess := preparedMergeHandoffSession(t)
	target := MergeHandoffTarget{Plugin: "fake-merge", Action: "merge"}

	payload, state, err := mgr.PrepareMergeHandoff(sess.ID, target, "")
	if err != nil {
		t.Fatal(err)
	}
	if payload.Operation != MergeHandoffPreflight || payload.IdempotencyKey == "" || !state.IsZero() {
		t.Fatalf("prepare payload=%+v state=%+v", payload, state)
	}
	encoded, _ := json.Marshal(payload)
	if strings.Contains(string(encoded), sess.WorkDir) || strings.Contains(string(encoded), "change.txt") {
		t.Fatalf("merge payload leaked path or filename: %s", encoded)
	}
	preflight := readyMergePreflight(payload)
	mergePayload, running, run, err := mgr.BeginMergeHandoff(sess.ID, target, payload.IdempotencyKey, preflight)
	if err != nil || !run || running.Status != MergeHandoffRunning || mergePayload.Operation != MergeHandoffExecute {
		t.Fatalf("begin payload=%+v state=%+v run=%v err=%v", mergePayload, running, run, err)
	}
	_, duplicate, run, err := mgr.BeginMergeHandoff(sess.ID, target, payload.IdempotencyKey, preflight)
	if err != nil || run || duplicate.Status != MergeHandoffRunning {
		t.Fatalf("running duplicate state=%+v run=%v err=%v", duplicate, run, err)
	}

	result := successfulMergeResult(mergePayload)
	info, err := mgr.FinishMergeHandoff(sess.ID, payload.IdempotencyKey, result, nil)
	if err != nil || info.MergeHandoff.Status != MergeHandoffSucceeded || info.MergeHandoff.Result.TargetCommit != testMergeCommit {
		t.Fatalf("finish = %+v, %v", info.MergeHandoff, err)
	}
	_, duplicate, run, err = mgr.BeginMergeHandoff(sess.ID, target, payload.IdempotencyKey, preflight)
	if err != nil || run || duplicate.Status != MergeHandoffSucceeded {
		t.Fatalf("successful duplicate state=%+v run=%v err=%v", duplicate, run, err)
	}
	if _, _, _, err := mgr.BeginMergeHandoff(sess.ID, target, "different-key", preflight); err == nil || !strings.Contains(err.Error(), "already merged") {
		t.Fatalf("different key after merge error = %v", err)
	}

	restarted, _, _ := newTestManagerIn(t, mgr.store.dataDir, testIdentity())
	restartedInfo, ok := restarted.GetInfo(sess.ID)
	if !ok || restartedInfo.MergeHandoff.Status != MergeHandoffSucceeded || restartedInfo.MergeHandoff.Result.TargetCommit != testMergeCommit {
		t.Fatalf("restart projection = %+v, found=%v", restartedInfo.MergeHandoff, ok)
	}
}

func TestManagerMergeHandoff_RequiresExplicitKeyAndReadyProvider(t *testing.T) {
	mgr, sess := preparedMergeHandoffSession(t)
	target := MergeHandoffTarget{Plugin: "fake-merge", Action: "merge"}
	payload, _, err := mgr.PrepareMergeHandoff(sess.ID, target, "")
	if err != nil {
		t.Fatal(err)
	}
	preflight := readyMergePreflight(payload)
	if _, _, _, err := mgr.BeginMergeHandoff(sess.ID, target, "", preflight); err == nil || !strings.Contains(err.Error(), "explicit idempotency key") {
		t.Fatalf("missing key error = %v", err)
	}
	preflight.Status = "blocked"
	preflight.Mergeable = false
	preflight.RequiredChecks = MergeChecksPending
	if err := ValidateMergeHandoffPreflight(preflight, payload); err != nil {
		t.Fatalf("valid blocked preflight: %v", err)
	}
	if _, _, _, err := mgr.BeginMergeHandoff(sess.ID, target, payload.IdempotencyKey, preflight); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("blocked preflight error = %v", err)
	}
}

func TestManagerMergeHandoff_RejectsProviderHeadMismatch(t *testing.T) {
	mgr, sess := preparedMergeHandoffSession(t)
	target := MergeHandoffTarget{Plugin: "fake-merge", Action: "merge"}
	payload, _, err := mgr.PrepareMergeHandoff(sess.ID, target, "")
	if err != nil {
		t.Fatal(err)
	}
	preflight := readyMergePreflight(payload)
	preflight.Target.HeadCommit = "dddddddddddddddddddddddddddddddddddddddd"
	if _, _, _, err := mgr.BeginMergeHandoff(sess.ID, target, payload.IdempotencyKey, preflight); err == nil || !strings.Contains(err.Error(), "does not match reviewed head") {
		t.Fatalf("head mismatch error = %v", err)
	}
}

func TestManagerMergeHandoff_RejectsHeadRaceAfterProviderPreflight(t *testing.T) {
	mgr, sess := preparedMergeHandoffSession(t)
	target := MergeHandoffTarget{Plugin: "fake-merge", Action: "merge"}
	payload, _, err := mgr.PrepareMergeHandoff(sess.ID, target, "race-key")
	if err != nil {
		t.Fatal(err)
	}
	preflight := readyMergePreflight(payload)
	if err := os.WriteFile(filepath.Join(sess.WorkDir, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runReviewTestGit(t, sess.WorkDir, "add", "--", "later.txt")
	runReviewTestGit(t, sess.WorkDir, "-c", "user.email=test@example.com", "-c", "user.name=Test User", "commit", "-m", "later")

	if _, _, _, err := mgr.BeginMergeHandoff(sess.ID, target, "race-key", preflight); err == nil ||
		(!strings.Contains(err.Error(), "current reviewed disposition") && !strings.Contains(err.Error(), "workspace changed")) {
		t.Fatalf("head race error = %v", err)
	}
}

func TestManagerMergeHandoff_UnknownRetriesOnlySameKey(t *testing.T) {
	mgr, sess := preparedMergeHandoffSession(t)
	target := MergeHandoffTarget{Plugin: "fake-merge", Action: "merge"}
	payload, _, _ := mgr.PrepareMergeHandoff(sess.ID, target, "retry-key")
	preflight := readyMergePreflight(payload)
	if _, _, run, err := mgr.BeginMergeHandoff(sess.ID, target, "retry-key", preflight); err != nil || !run {
		t.Fatalf("begin run=%v err=%v", run, err)
	}
	if _, err := mgr.FinishMergeHandoff(sess.ID, "retry-key", MergeHandoffProviderResult{}, contextDeadlineError{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := mgr.BeginMergeHandoff(sess.ID, target, "new-key", preflight); err == nil || !strings.Contains(err.Error(), "existing idempotency key") {
		t.Fatalf("new key after unknown error = %v", err)
	}
	_, state, run, err := mgr.BeginMergeHandoff(sess.ID, target, "retry-key", preflight)
	if err != nil || !run || state.Status != MergeHandoffRunning {
		t.Fatalf("same-key retry state=%+v run=%v err=%v", state, run, err)
	}
}

func TestManagerMergeHandoff_FailsClosedOnReviewAndWorktreeState(t *testing.T) {
	t.Run("seen alone", func(t *testing.T) {
		mgr, sess := preparedMergeHandoffSession(t)
		mgr.mu.Lock()
		mgr.sessions[sess.ID].Attention.SeenGeneration = mgr.sessions[sess.ID].Attention.Generation
		mgr.sessions[sess.ID].ReviewDisposition = ReviewDisposition{}
		mgr.mu.Unlock()
		_, _, err := mgr.PrepareMergeHandoff(sess.ID, MergeHandoffTarget{Plugin: "fake", Action: "merge"}, "")
		if err == nil || !strings.Contains(err.Error(), "current reviewed disposition") {
			t.Fatalf("seen-only error = %v", err)
		}
	})

	t.Run("changes requested", func(t *testing.T) {
		mgr, sess := preparedMergeHandoffSession(t)
		if _, err := mgr.ReportReviewDisposition(sess.ID, ReviewDecisionChangesRequested); err != nil {
			t.Fatal(err)
		}
		_, _, err := mgr.PrepareMergeHandoff(sess.ID, MergeHandoffTarget{Plugin: "fake", Action: "merge"}, "")
		if err == nil || !strings.Contains(err.Error(), "current reviewed disposition") {
			t.Fatalf("changes-requested error = %v", err)
		}
	})

	t.Run("stale review", func(t *testing.T) {
		mgr, sess := preparedMergeHandoffSession(t)
		mgr.mu.Lock()
		mgr.sessions[sess.ID].ReviewDisposition.WorkspaceFingerprint = "stale-fingerprint"
		mgr.mu.Unlock()
		_, _, err := mgr.PrepareMergeHandoff(sess.ID, MergeHandoffTarget{Plugin: "fake", Action: "merge"}, "")
		if err == nil || !strings.Contains(err.Error(), "current reviewed disposition") {
			t.Fatalf("stale-review error = %v", err)
		}
	})

	t.Run("dirty", func(t *testing.T) {
		mgr, sess := preparedMergeHandoffSession(t)
		if err := os.WriteFile(filepath.Join(sess.WorkDir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.ReportReviewDisposition(sess.ID, ReviewDecisionReviewed); err != nil {
			t.Fatal(err)
		}
		// Rebind the PR handoff to the dirty fingerprint so the independent
		// cleanliness guard, rather than stale evidence, is what refuses.
		mgr.mu.Lock()
		mgr.sessions[sess.ID].PRHandoff.WorkspaceFingerprint = mgr.sessions[sess.ID].ReviewFacts.WorkspaceFingerprint
		mgr.mu.Unlock()
		_, _, err := mgr.PrepareMergeHandoff(sess.ID, MergeHandoffTarget{Plugin: "fake", Action: "merge"}, "")
		if err == nil || !strings.Contains(err.Error(), "uncommitted") {
			t.Fatalf("dirty error = %v", err)
		}
	})
}

func TestFinishMergeHandoff_RejectsSpoofedSuccessAsUnknown(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*MergeHandoffProviderResult)
		wantError string
	}{
		{
			name: "head",
			mutate: func(result *MergeHandoffProviderResult) {
				result.HeadCommit = "dddddddddddddddddddddddddddddddddddddddd"
			},
			wantError: "reviewed head",
		},
		{
			name: "target commit",
			mutate: func(result *MergeHandoffProviderResult) {
				result.TargetCommit = "not-a-full-commit"
			},
			wantError: "target commit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, sess := preparedMergeHandoffSession(t)
			target := MergeHandoffTarget{Plugin: "fake-merge", Action: "merge"}
			payload, _, _ := mgr.PrepareMergeHandoff(sess.ID, target, "stable-key")
			preflight := readyMergePreflight(payload)
			mergePayload, _, _, err := mgr.BeginMergeHandoff(sess.ID, target, "stable-key", preflight)
			if err != nil {
				t.Fatal(err)
			}
			spoofed := successfulMergeResult(mergePayload)
			tt.mutate(&spoofed)
			info, err := mgr.FinishMergeHandoff(sess.ID, "stable-key", spoofed, nil)
			if err != nil || info.MergeHandoff.Status != MergeHandoffUnknown || !strings.Contains(info.MergeHandoff.Error, tt.wantError) {
				t.Fatalf("spoofed result = %+v, err=%v", info.MergeHandoff, err)
			}
		})
	}
}

func preparedMergeHandoffSession(t *testing.T) (*Manager, *Session) {
	t.Helper()
	mgr, sess := preparedPRHandoffSession(t)
	target := PRHandoffTarget{Plugin: "fake-pr", Action: "create"}
	payload, _, run, err := mgr.BeginPRHandoff(sess.ID, target, "pr-key", false)
	if err != nil || !run {
		t.Fatalf("begin PR handoff: run=%v err=%v", run, err)
	}
	result := PRHandoffProviderResult{
		Status: PRHandoffSucceeded, Provider: "fake", ID: "42", URL: "https://example.test/pulls/42",
	}
	if _, err := mgr.FinishPRHandoff(sess.ID, payload.IdempotencyKey, result, nil); err != nil {
		t.Fatal(err)
	}
	return mgr, sess
}

func readyMergePreflight(payload MergeHandoffRequest) MergeHandoffPreflightResult {
	target := payload.PullRequest
	target.BaseRef = "main"
	return MergeHandoffPreflightResult{
		Status: "ready", Target: target, Mergeable: true, RequiredChecks: MergeChecksPassed,
	}
}

func successfulMergeResult(payload MergeHandoffRequest) MergeHandoffProviderResult {
	return MergeHandoffProviderResult{
		Status: MergeHandoffSucceeded, Provider: payload.PullRequest.Provider,
		ID: payload.PullRequest.ID, URL: payload.PullRequest.URL,
		HeadCommit: payload.Review.HeadCommit, TargetCommit: testMergeCommit, Method: "squash",
	}
}

func TestStoreAndRestart_MergeHandoffCannotRegress(t *testing.T) {
	store, dir := newTestStore(t)
	now := time.Unix(20, 0)
	completed := Session{ID: "merge", MergeHandoff: MergeHandoff{
		IdempotencyKey: "stable-key", Status: MergeHandoffSucceeded,
		Result:    MergeHandoffProviderResult{Status: MergeHandoffSucceeded, TargetCommit: testMergeCommit},
		StartedAt: now.Add(-time.Second), UpdatedAt: now,
	}}
	if err := store.Save(completed); err != nil {
		t.Fatal(err)
	}
	stale := completed
	stale.MergeHandoff.Status = MergeHandoffRunning
	stale.MergeHandoff.Result = MergeHandoffProviderResult{}
	if err := store.Save(stale); err != nil {
		t.Fatal(err)
	}
	loaded, _ := store.Load(completed.ID)
	if loaded.MergeHandoff.Status != MergeHandoffSucceeded {
		t.Fatalf("regressed merge = %+v", loaded.MergeHandoff)
	}

	running := Session{ID: "interrupted-merge", MergeHandoff: MergeHandoff{
		IdempotencyKey: "retry-key", Status: MergeHandoffRunning, StartedAt: now, UpdatedAt: now,
	}}
	if err := store.Save(running); err != nil {
		t.Fatal(err)
	}
	restarted, _, _ := newTestManagerIn(t, dir, testIdentity())
	info, ok := restarted.GetInfo(running.ID)
	if !ok || info.MergeHandoff.Status != MergeHandoffUnknown {
		t.Fatalf("interrupted merge = %+v, found=%v", info.MergeHandoff, ok)
	}
}

func TestMergeHandoff_ZeroOmittedFromJSON(t *testing.T) {
	for name, value := range map[string]any{"session": Session{ID: "s1"}, "info": Info{ID: "s1"}} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if containsJSONKey(data, "merge_handoff") {
			t.Errorf("%s zero JSON = %s", name, data)
		}
	}
}
