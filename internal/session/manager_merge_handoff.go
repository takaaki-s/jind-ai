package session

import (
	"context"
	"fmt"
	"time"
)

// PrepareMergeHandoff refreshes and validates local evidence, then builds the
// read-only provider preflight request. It never mutates merge-handoff state.
func (m *Manager) PrepareMergeHandoff(id string, target MergeHandoffTarget, key string) (MergeHandoffRequest, MergeHandoffInfo, error) {
	var payload MergeHandoffRequest
	if target.Plugin == "" || target.Action == "" {
		return payload, MergeHandoffInfo{}, fmt.Errorf("merge handoff plugin and action are required")
	}
	if key != "" {
		if err := validatePRHandoffKey(key); err != nil {
			return payload, MergeHandoffInfo{}, err
		}
	}

	m.mu.RLock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return payload, MergeHandoffInfo{}, fmt.Errorf("session not found: %s", id)
	}
	generation := sess.Attention.Generation
	m.mu.RUnlock()
	if generation == 0 {
		return payload, MergeHandoffInfo{}, fmt.Errorf("session has no completed turn to merge")
	}
	if _, err := m.RefreshReview(id); err != nil {
		return payload, MergeHandoffInfo{}, fmt.Errorf("refresh merge handoff evidence: %w", err)
	}

	m.mu.RLock()
	sess, ok = m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return payload, MergeHandoffInfo{}, fmt.Errorf("session not found: %s", id)
	}
	if err := validateMergeHandoffEvidence(sess, generation); err != nil {
		m.mu.RUnlock()
		return payload, MergeHandoffInfo{}, err
	}
	worktreePath := sess.ReviewBase.WorktreePath
	baseCommit := sess.ReviewFacts.BaseCommit
	headCommit := sess.ReviewFacts.HeadCommit
	branch := sess.ReviewFacts.Branch
	fingerprint := sess.ReviewFacts.WorkspaceFingerprint
	gitClient := m.gitClient
	m.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), reviewProbeTimeout)
	defer cancel()
	select {
	case m.reviewSem <- struct{}{}:
		defer func() { <-m.reviewSem }()
	case <-ctx.Done():
		return payload, MergeHandoffInfo{}, fmt.Errorf("inspect merge handoff worktree: %w", ctx.Err())
	}
	clean, err := gitClient.ReviewWorktreeClean(ctx, worktreePath)
	if err != nil {
		return payload, MergeHandoffInfo{}, err
	}
	if !clean {
		return payload, MergeHandoffInfo{}, fmt.Errorf("worktree has uncommitted changes; commit or discard them before merge handoff")
	}
	observed, err := gitClient.InspectReview(ctx, worktreePath, baseCommit)
	if err != nil {
		return payload, MergeHandoffInfo{}, fmt.Errorf("revalidate merge handoff evidence: %w", err)
	}
	if observed.WorkspaceFingerprint != fingerprint || observed.HeadCommit != headCommit || observed.Branch != branch {
		return payload, MergeHandoffInfo{}, fmt.Errorf("workspace changed while merge handoff was prepared; retry")
	}

	m.mu.RLock()
	live, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return payload, MergeHandoffInfo{}, fmt.Errorf("session not found: %s", id)
	}
	if live.Attention.Generation != generation || live.ReviewFacts.WorkspaceFingerprint != fingerprint {
		m.mu.RUnlock()
		return payload, MergeHandoffInfo{}, fmt.Errorf("workspace changed while merge handoff was prepared; retry")
	}
	if err := validateMergeHandoffEvidence(live, generation); err != nil {
		m.mu.RUnlock()
		return payload, MergeHandoffInfo{}, err
	}
	prTarget := mergeTargetFromSession(live)
	if key == "" {
		key = defaultMergeHandoffKey(id, fingerprint, prTarget, target)
	}
	payload = buildMergeHandoffRequest(live, key, prTarget)
	current := live.MergeHandoff.toInfo(live.ReviewFacts, live.PRHandoff)
	m.mu.RUnlock()
	return payload, current, nil
}

// BeginMergeHandoff repeats local validation after provider preflight and
// persists running before the caller invokes the mutating provider operation.
func (m *Manager) BeginMergeHandoff(id string, target MergeHandoffTarget, key string, preflight MergeHandoffPreflightResult) (MergeHandoffRequest, MergeHandoffInfo, bool, error) {
	if key == "" {
		return MergeHandoffRequest{}, MergeHandoffInfo{}, false, fmt.Errorf("explicit idempotency key is required for merge confirmation")
	}
	payload, _, err := m.PrepareMergeHandoff(id, target, key)
	if err != nil {
		return payload, MergeHandoffInfo{}, false, err
	}
	if err := ValidateMergeHandoffPreflight(preflight, payload); err != nil {
		return payload, MergeHandoffInfo{}, false, err
	}
	if !preflight.ready() {
		return payload, MergeHandoffInfo{}, false, fmt.Errorf("merge provider preflight is blocked: mergeable=%t required_checks=%s", preflight.Mergeable, preflight.RequiredChecks)
	}
	payload.Operation = MergeHandoffExecute
	payload.PullRequest = preflight.Target
	payload.Preflight = &preflight

	m.mu.Lock()
	live, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return payload, MergeHandoffInfo{}, false, fmt.Errorf("session not found: %s", id)
	}
	fingerprint := payload.Review.WorkspaceFingerprint
	if live.ReviewFacts.WorkspaceFingerprint != fingerprint || live.ReviewFacts.HeadCommit != payload.Review.HeadCommit {
		m.mu.Unlock()
		return payload, MergeHandoffInfo{}, false, fmt.Errorf("workspace changed after merge provider preflight; retry")
	}
	if err := validateMergeHandoffEvidence(live, live.Attention.Generation); err != nil {
		m.mu.Unlock()
		return payload, MergeHandoffInfo{}, false, err
	}

	existing := live.MergeHandoff
	if !existing.IsZero() && existing.IdempotencyKey == key {
		if existing.Target != target || existing.WorkspaceFingerprint != fingerprint || !samePullRequestTarget(existing.PRTarget, preflight.Target) {
			m.mu.Unlock()
			return payload, MergeHandoffInfo{}, false, fmt.Errorf("idempotency key %q was already used for different merge evidence", key)
		}
		if existing.Status == MergeHandoffRunning || existing.Status == MergeHandoffSucceeded {
			current := existing.toInfo(live.ReviewFacts, live.PRHandoff)
			m.mu.Unlock()
			return payload, current, false, nil
		}
	} else if existing.Target == target && existing.WorkspaceFingerprint == fingerprint {
		switch existing.Status {
		case MergeHandoffSucceeded:
			m.mu.Unlock()
			return payload, MergeHandoffInfo{}, false, fmt.Errorf("this workspace was already merged with idempotency key %q", existing.IdempotencyKey)
		case MergeHandoffUnknown:
			m.mu.Unlock()
			return payload, MergeHandoffInfo{}, false, fmt.Errorf("merge outcome is unknown; retry with the existing idempotency key %q", existing.IdempotencyKey)
		}
	} else if existing.Status == MergeHandoffRunning {
		m.mu.Unlock()
		return payload, MergeHandoffInfo{}, false, fmt.Errorf("another merge handoff is already running")
	}

	now := time.Now()
	live.MergeHandoff = MergeHandoff{
		IdempotencyKey: key, Target: target, WorkspaceFingerprint: fingerprint,
		PRTarget: preflight.Target, Preflight: preflight, Status: MergeHandoffRunning,
		StartedAt: now, UpdatedAt: now,
	}
	saved := m.snapshotAndUnlock(live)
	if err := m.store.Save(saved); err != nil {
		return payload, saved.MergeHandoff.toInfo(saved.ReviewFacts, saved.PRHandoff), false, err
	}
	return payload, saved.MergeHandoff.toInfo(saved.ReviewFacts, saved.PRHandoff), true, nil
}

func validateMergeHandoffEvidence(sess *Session, generation uint64) error {
	if err := validatePRHandoffEvidence(sess, generation); err != nil {
		return err
	}
	if sess.PRHandoff.Status != PRHandoffSucceeded || sess.PRHandoff.stale(sess.ReviewFacts) {
		return fmt.Errorf("current successful PR handoff is required before merge handoff")
	}
	if sess.PRHandoff.Result.Provider == "" || (sess.PRHandoff.Result.ID == "" && sess.PRHandoff.Result.URL == "") {
		return fmt.Errorf("successful PR handoff lacks a provider target identity")
	}
	return nil
}

func mergeTargetFromSession(sess *Session) MergeProviderTarget {
	return MergeProviderTarget{
		Provider: sess.PRHandoff.Result.Provider, ID: sess.PRHandoff.Result.ID, URL: sess.PRHandoff.Result.URL,
		BaseRef: sess.ReviewBase.RequestedRef, BaseCommit: sess.ReviewFacts.BaseCommit,
		HeadCommit: sess.ReviewFacts.HeadCommit,
	}
}

func buildMergeHandoffRequest(sess *Session, key string, prTarget MergeProviderTarget) MergeHandoffRequest {
	request := MergeHandoffRequest{
		SchemaVersion: MergeHandoffSchemaVersion, Kind: "merge", Operation: MergeHandoffPreflight,
		IdempotencyKey: key, SessionID: sess.ID, Repository: sess.RepoName,
		PullRequest: prTarget, Review: buildPRHandoffRequest(sess, key).Review,
		ReviewedAt: sess.ReviewDisposition.ReportedAt,
	}
	if sess.CheckReport.current(sess.ReviewFacts) {
		request.Checks = &PRHandoffCheckEvidence{
			Source: sess.CheckReport.Source, Status: sess.CheckReport.Status, ReportedAt: sess.CheckReport.ReportedAt,
		}
	}
	return request
}

func (m *Manager) FinishMergeHandoff(id, key string, result MergeHandoffProviderResult, runErr error) (Info, error) {
	m.mu.Lock()
	live, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return Info{}, fmt.Errorf("session not found: %s", id)
	}
	if live.MergeHandoff.IdempotencyKey != key || live.MergeHandoff.Status != MergeHandoffRunning {
		m.mu.Unlock()
		return Info{}, fmt.Errorf("merge handoff %q is not running", key)
	}
	request := buildMergeHandoffRequest(live, key, live.MergeHandoff.PRTarget)
	request.Operation = MergeHandoffExecute
	request.Preflight = &live.MergeHandoff.Preflight
	if runErr != nil {
		live.MergeHandoff.Status = MergeHandoffUnknown
		live.MergeHandoff.Error = boundedHandoffError(runErr.Error())
	} else if err := ValidateMergeHandoffProviderResult(result, request); err != nil {
		live.MergeHandoff.Status = MergeHandoffUnknown
		live.MergeHandoff.Error = boundedHandoffError(err.Error())
	} else {
		live.MergeHandoff.Status = result.Status
		live.MergeHandoff.Result = result
		live.MergeHandoff.Error = ""
	}
	live.MergeHandoff.UpdatedAt = time.Now()
	saved := m.snapshotAndUnlock(live)
	err := m.store.Save(saved)
	return saved.ToInfo(), err
}
