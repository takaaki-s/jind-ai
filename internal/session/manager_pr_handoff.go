package session

import (
	"context"
	"fmt"
	"time"
)

// BeginPRHandoff refreshes and validates the exact review evidence that will
// be handed to a provider. dryRun returns the same payload without changing
// session state. shouldRun is false for an already-running or already-
// successful idempotency key.
func (m *Manager) BeginPRHandoff(id string, target PRHandoffTarget, key string, dryRun bool) (payload PRHandoffRequest, current PRHandoffInfo, shouldRun bool, err error) {
	if target.Plugin == "" || target.Action == "" {
		return payload, current, false, fmt.Errorf("handoff plugin and action are required")
	}
	if key != "" {
		if err := validatePRHandoffKey(key); err != nil {
			return payload, current, false, err
		}
	}

	m.mu.RLock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return payload, current, false, fmt.Errorf("session not found: %s", id)
	}
	generation := sess.Attention.Generation
	m.mu.RUnlock()
	if generation == 0 {
		return payload, current, false, fmt.Errorf("session has no completed turn to hand off")
	}
	if _, err := m.RefreshReview(id); err != nil {
		return payload, current, false, fmt.Errorf("refresh PR handoff evidence: %w", err)
	}

	m.mu.RLock()
	sess, ok = m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return payload, current, false, fmt.Errorf("session not found: %s", id)
	}
	if err := validatePRHandoffEvidence(sess, generation); err != nil {
		m.mu.RUnlock()
		return payload, current, false, err
	}
	worktreePath := sess.ReviewBase.WorktreePath
	fingerprint := sess.ReviewFacts.WorkspaceFingerprint
	baseCommit := sess.ReviewFacts.BaseCommit
	headCommit := sess.ReviewFacts.HeadCommit
	branch := sess.ReviewFacts.Branch
	gitClient := m.gitClient
	m.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), reviewProbeTimeout)
	defer cancel()
	select {
	case m.reviewSem <- struct{}{}:
		defer func() { <-m.reviewSem }()
	case <-ctx.Done():
		return payload, current, false, fmt.Errorf("inspect PR handoff worktree: %w", ctx.Err())
	}
	clean, err := gitClient.ReviewWorktreeClean(ctx, worktreePath)
	if err != nil {
		return payload, current, false, err
	}
	if !clean {
		return payload, current, false, fmt.Errorf("worktree has uncommitted changes; commit or discard them before PR handoff")
	}
	// A clean worktree alone is insufficient: another process may have made a
	// commit after RefreshReview. Re-observe the immutable-base comparison
	// immediately before the provider call and require the approved identity.
	observed, err := gitClient.InspectReview(ctx, worktreePath, baseCommit)
	if err != nil {
		return payload, current, false, fmt.Errorf("revalidate PR handoff evidence: %w", err)
	}
	if observed.WorkspaceFingerprint != fingerprint || observed.HeadCommit != headCommit || observed.Branch != branch {
		return payload, current, false, fmt.Errorf("workspace changed while PR handoff was prepared; retry")
	}

	m.mu.Lock()
	live, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return payload, current, false, fmt.Errorf("session not found: %s", id)
	}
	if live.Attention.Generation != generation || live.ReviewFacts.WorkspaceFingerprint != fingerprint {
		m.mu.Unlock()
		return payload, current, false, fmt.Errorf("workspace changed while PR handoff was prepared; retry")
	}
	if err := validatePRHandoffEvidence(live, generation); err != nil {
		m.mu.Unlock()
		return payload, current, false, err
	}
	if key == "" {
		key = defaultPRHandoffKey(id, fingerprint, target)
	}
	payload = buildPRHandoffRequest(live, key)
	if dryRun {
		current = live.PRHandoff.toInfo(live.ReviewFacts)
		m.mu.Unlock()
		return payload, current, false, nil
	}

	existing := live.PRHandoff
	if !existing.IsZero() && existing.IdempotencyKey == key {
		if existing.Target != target || existing.WorkspaceFingerprint != fingerprint {
			m.mu.Unlock()
			return payload, current, false, fmt.Errorf("idempotency key %q was already used for different handoff evidence", key)
		}
		if existing.Status == PRHandoffRunning || existing.Status == PRHandoffSucceeded {
			current = existing.toInfo(live.ReviewFacts)
			m.mu.Unlock()
			return payload, current, false, nil
		}
	} else if existing.Target == target && existing.WorkspaceFingerprint == fingerprint {
		switch existing.Status {
		case PRHandoffSucceeded:
			m.mu.Unlock()
			return payload, current, false, fmt.Errorf("this workspace was already handed off successfully with idempotency key %q", existing.IdempotencyKey)
		case PRHandoffUnknown:
			m.mu.Unlock()
			return payload, current, false, fmt.Errorf("PR handoff outcome is unknown; retry with the existing idempotency key %q", existing.IdempotencyKey)
		}
	} else if existing.Status == PRHandoffRunning {
		m.mu.Unlock()
		return payload, current, false, fmt.Errorf("another PR handoff is already running")
	}

	now := time.Now()
	live.PRHandoff = PRHandoff{
		IdempotencyKey:       key,
		Target:               target,
		WorkspaceFingerprint: fingerprint,
		Status:               PRHandoffRunning,
		StartedAt:            now,
		UpdatedAt:            now,
	}
	saved := m.snapshotAndUnlock(live)
	if err := m.store.Save(saved); err != nil {
		return payload, saved.PRHandoff.toInfo(saved.ReviewFacts), false, err
	}
	return payload, saved.PRHandoff.toInfo(saved.ReviewFacts), true, nil
}

func validatePRHandoffEvidence(sess *Session, generation uint64) error {
	facts := sess.ReviewFacts
	if sess.Attention.Generation != generation {
		return fmt.Errorf("session completed another turn while PR handoff was prepared; retry")
	}
	if facts.Status != ReviewFactsAvailable || facts.WorkspaceFingerprint == "" || facts.ChangedFiles == 0 {
		return fmt.Errorf("current non-empty review evidence is required for PR handoff")
	}
	if facts.Branch == "" || facts.HeadCommit == "" || facts.BaseCommit == "" || facts.CommitCount == 0 {
		return fmt.Errorf("PR handoff requires a named branch with committed changes")
	}
	disposition := sess.ReviewDisposition
	if disposition.Decision != ReviewDecisionReviewed || disposition.stale(facts) {
		return fmt.Errorf("current reviewed disposition is required for PR handoff")
	}
	if !sess.CheckReport.IsZero() {
		if sess.CheckReport.stale(facts) {
			return fmt.Errorf("reported checks are stale; report checks for the current workspace before PR handoff")
		}
		if sess.CheckReport.Status == CheckStatusFailed {
			return fmt.Errorf("reported checks failed for the current workspace")
		}
	}
	return nil
}

func buildPRHandoffRequest(sess *Session, key string) PRHandoffRequest {
	facts := sess.ReviewFacts
	payload := PRHandoffRequest{
		SchemaVersion:  PRHandoffSchemaVersion,
		Kind:           "pull-request",
		IdempotencyKey: key,
		SessionID:      sess.ID,
		Repository:     sess.RepoName,
		Review: PRHandoffReviewEvidence{
			BaseCommit: facts.BaseCommit, HeadCommit: facts.HeadCommit,
			Branch: facts.Branch, WorkspaceFingerprint: facts.WorkspaceFingerprint,
			ChangedFiles: facts.ChangedFiles, Additions: facts.Additions,
			Deletions: facts.Deletions, BinaryFiles: facts.BinaryFiles,
			CommitCount: facts.CommitCount,
		},
		ReviewedAt: sess.ReviewDisposition.ReportedAt,
	}
	if sess.CheckReport.current(facts) {
		payload.Checks = &PRHandoffCheckEvidence{
			Source: sess.CheckReport.Source, Status: sess.CheckReport.Status,
			ReportedAt: sess.CheckReport.ReportedAt,
		}
	}
	return payload
}

// FinishPRHandoff records a bounded provider result or an unknown outcome.
func (m *Manager) FinishPRHandoff(id, key string, result PRHandoffProviderResult, runErr error) (Info, error) {
	m.mu.Lock()
	live, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return Info{}, fmt.Errorf("session not found: %s", id)
	}
	if live.PRHandoff.IdempotencyKey != key || live.PRHandoff.Status != PRHandoffRunning {
		m.mu.Unlock()
		return Info{}, fmt.Errorf("PR handoff %q is not running", key)
	}
	if runErr != nil {
		live.PRHandoff.Status = PRHandoffUnknown
		live.PRHandoff.Error = boundedHandoffError(runErr.Error())
	} else {
		if err := ValidatePRHandoffProviderResult(result); err != nil {
			live.PRHandoff.Status = PRHandoffUnknown
			live.PRHandoff.Error = boundedHandoffError(err.Error())
		} else {
			live.PRHandoff.Status = result.Status
			live.PRHandoff.Result = result
			live.PRHandoff.Error = ""
		}
	}
	live.PRHandoff.UpdatedAt = time.Now()
	saved := m.snapshotAndUnlock(live)
	err := m.store.Save(saved)
	return saved.ToInfo(), err
}

func boundedHandoffError(message string) string {
	const limit = 512
	if len(message) > limit {
		return message[:limit]
	}
	return message
}
