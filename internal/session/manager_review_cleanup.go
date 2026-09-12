package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PrepareReviewCleanup builds an exact local-only plan. It never writes a
// journal or mutates the session/worktree/branch. If a prior journal exists,
// the persisted plan is returned so an already-deleted session remains
// inspectable by its full ID.
func (m *Manager) PrepareReviewCleanup(id, key string) (ReviewCleanupPlan, ReviewCleanupJournal, error) {
	m.cleanupMu.Lock()
	defer m.cleanupMu.Unlock()
	return m.prepareReviewCleanup(id, key)
}

func (m *Manager) prepareReviewCleanup(id, key string) (ReviewCleanupPlan, ReviewCleanupJournal, error) {
	if !cleanupSessionIDPattern.MatchString(id) {
		return ReviewCleanupPlan{}, ReviewCleanupJournal{}, fmt.Errorf("invalid cleanup session id %q", id)
	}
	if key != "" {
		if err := validatePRHandoffKey(key); err != nil {
			return ReviewCleanupPlan{}, ReviewCleanupJournal{}, err
		}
	}
	journal, loadErr := m.cleanupStore.Load(id)
	if loadErr == nil {
		if key != "" && journal.Plan.IdempotencyKey != key {
			return ReviewCleanupPlan{}, journal, fmt.Errorf("cleanup already uses idempotency key %q", journal.Plan.IdempotencyKey)
		}
		return journal.Plan, journal, nil
	}
	if !errors.Is(loadErr, os.ErrNotExist) {
		return ReviewCleanupPlan{}, ReviewCleanupJournal{}, loadErr
	}

	m.mu.RLock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.RUnlock()
		return ReviewCleanupPlan{}, ReviewCleanupJournal{}, fmt.Errorf("session not found: %s", id)
	}
	snapshot := *sess
	gitClient := m.gitClient
	tmuxClient := m.tmuxClient
	m.mu.RUnlock()

	merge := snapshot.MergeHandoff.toInfo(snapshot.ReviewFacts, snapshot.PRHandoff)
	if key == "" {
		key = defaultReviewCleanupKey(snapshot.ID, snapshot.ReviewFacts.WorkspaceFingerprint, snapshot.MergeHandoff.IdempotencyKey)
	}
	plan := ReviewCleanupPlan{
		SchemaVersion: ReviewCleanupSchemaVersion, IdempotencyKey: key,
		SessionID: snapshot.ID, SessionDescription: snapshot.Description,
		TmuxWindowName: snapshot.TmuxWindowName, WorktreePath: snapshot.ReviewBase.WorktreePath,
		HeadCommit: snapshot.ReviewFacts.HeadCommit, MergeTargetCommit: merge.Result.TargetCommit,
		PlannedAt: time.Now(),
	}

	if snapshot.Status == StatusDeleting {
		plan.Blockers = append(plan.Blockers, "another session deletion is already in progress")
	}
	if err := validateReviewCleanupMergeReceipt(&snapshot); err != nil {
		plan.Blockers = append(plan.Blockers, "a current verified merge result is required: "+err.Error())
	}
	if snapshot.ReviewBase.WorktreePath == "" || snapshot.ReviewBase.UnavailableReason != "" {
		plan.Blockers = append(plan.Blockers, "an exact jind-ai-managed worktree path is required")
	}
	if snapshot.ReviewFacts.Status != ReviewFactsAvailable || snapshot.ReviewFacts.WorkspaceFingerprint == "" {
		plan.Blockers = append(plan.Blockers, "current review evidence is unavailable")
	}
	if merge.Result.HeadCommit == "" || merge.Result.HeadCommit != snapshot.ReviewFacts.HeadCommit {
		plan.Blockers = append(plan.Blockers, "the local review head does not match the verified merge head")
	}
	if activeCleanupPane(tmuxClient, snapshot.TmuxWindowName, snapshot.TmuxPaneID) {
		plan.Blockers = append(plan.Blockers, "the session pane is active; stop it before cleanup")
	}

	// Only inspect a target after the immutable evidence establishes a bounded
	// path and full provider-observed head. Failures become visible blockers in
	// dry-run instead of turning the preview itself into a destructive-looking
	// command error.
	if snapshot.ReviewBase.WorktreePath != "" && mergeCommitPattern.MatchString(merge.Result.HeadCommit) {
		ctx, cancel := context.WithTimeout(context.Background(), reviewProbeTimeout)
		defer cancel()
		select {
		case m.reviewSem <- struct{}{}:
			defer func() { <-m.reviewSem }()
		case <-ctx.Done():
			plan.Blockers = append(plan.Blockers, "worktree ownership inspection timed out")
			plan.Ready = false
			return plan, ReviewCleanupJournal{}, nil
		}
		observed, err := gitClient.InspectCleanupWorktree(ctx, snapshot.ReviewBase.WorktreePath, merge.Result.HeadCommit)
		if err != nil {
			plan.Blockers = append(plan.Blockers, "worktree ownership could not be verified: "+err.Error())
		} else {
			plan.RepositoryPath = observed.RepositoryPath
			plan.Branch = observed.Branch
			plan.HeadCommit = observed.HeadCommit
			plan.UnpushedCommits = observed.UnpushedCommits
			if observed.HeadCommit != snapshot.ReviewFacts.HeadCommit || observed.Branch != snapshot.ReviewFacts.Branch {
				plan.Blockers = append(plan.Blockers, "worktree branch or head changed after verified merge")
			}
			if observed.UnpushedCommits != 0 {
				plan.Blockers = append(plan.Blockers, fmt.Sprintf("worktree has %d commit(s) after the verified merge head", observed.UnpushedCommits))
			}
			clean, cleanErr := gitClient.ReviewWorktreeClean(ctx, observed.WorktreePath)
			if cleanErr != nil {
				plan.Blockers = append(plan.Blockers, "worktree cleanliness could not be verified: "+cleanErr.Error())
			} else if !clean {
				plan.Blockers = append(plan.Blockers, "worktree has staged, unstaged, untracked, or submodule changes")
			}
			fresh, freshErr := gitClient.InspectReview(ctx, observed.WorktreePath, snapshot.ReviewFacts.BaseCommit)
			if freshErr != nil {
				plan.Blockers = append(plan.Blockers, "review evidence could not be revalidated: "+freshErr.Error())
			} else if fresh.WorkspaceFingerprint != snapshot.ReviewFacts.WorkspaceFingerprint {
				plan.Blockers = append(plan.Blockers, "workspace changed after the verified merge")
			}
		}
	}
	plan.Ready = len(plan.Blockers) == 0
	return plan, ReviewCleanupJournal{}, nil
}

func activeCleanupPane(tc interface {
	HasSession(string) bool
	IsPaneDead(string) bool
}, windowName, paneID string) bool {
	if windowName == "" {
		return false
	}
	if tc == nil {
		return true
	}
	if !tc.HasSession(windowName) {
		return false
	}
	return paneID == "" || !tc.IsPaneDead(paneID)
}

func validateReviewCleanupMergeReceipt(sess *Session) error {
	handoff := sess.MergeHandoff
	if handoff.Status != MergeHandoffSucceeded || handoff.stale(sess.ReviewFacts, sess.PRHandoff) {
		return fmt.Errorf("merge handoff is missing, unsuccessful, or stale")
	}
	if err := validateMergeHandoffEvidence(sess, sess.Attention.Generation); err != nil {
		return err
	}
	if !samePullRequestTarget(handoff.PRTarget, mergeTargetFromSession(sess)) {
		return fmt.Errorf("merge target no longer matches the successful PR handoff")
	}
	request := buildMergeHandoffRequest(sess, handoff.IdempotencyKey, handoff.PRTarget)
	request.Operation = MergeHandoffExecute
	request.Preflight = &handoff.Preflight
	if err := ValidateMergeHandoffProviderResult(handoff.Result, request); err != nil {
		return err
	}
	return nil
}

// ExecuteReviewCleanup persists the exact plan, then advances four local-only
// steps. A failed journal is returned as data (not an opaque error) so callers
// can report which assets remain and retry the same key. Completed steps are
// never repeated; missing assets are accepted only from an already-persisted
// exact plan.
func (m *Manager) ExecuteReviewCleanup(id, key string) (ReviewCleanupJournal, error) {
	if key == "" {
		return ReviewCleanupJournal{}, fmt.Errorf("explicit idempotency key is required for cleanup confirmation")
	}
	if err := validatePRHandoffKey(key); err != nil {
		return ReviewCleanupJournal{}, err
	}
	m.cleanupMu.Lock()
	defer m.cleanupMu.Unlock()

	journal, err := m.cleanupStore.Load(id)
	if errors.Is(err, os.ErrNotExist) {
		plan, _, prepErr := m.prepareReviewCleanup(id, key)
		if prepErr != nil {
			return ReviewCleanupJournal{}, prepErr
		}
		if plan.IdempotencyKey != key {
			return ReviewCleanupJournal{}, fmt.Errorf("cleanup confirmation key does not match dry-run key %q", plan.IdempotencyKey)
		}
		if !plan.Ready {
			return ReviewCleanupJournal{}, fmt.Errorf("cleanup is blocked: %s", strings.Join(plan.Blockers, "; "))
		}
		journal = newReviewCleanupJournal(plan, time.Now())
		if err := m.cleanupStore.Save(journal); err != nil {
			return ReviewCleanupJournal{}, err
		}
	} else if err != nil {
		return ReviewCleanupJournal{}, err
	} else {
		if journal.Plan.IdempotencyKey != key {
			return journal, fmt.Errorf("cleanup already uses idempotency key %q", journal.Plan.IdempotencyKey)
		}
		if journal.Status == ReviewCleanupSucceeded {
			return journal, nil
		}
		journal.Status = ReviewCleanupRunning
		journal.Error = ""
		for i := range journal.Steps {
			if journal.Steps[i].Status == ReviewCleanupStepFailed {
				journal.Steps[i].Status = ReviewCleanupStepPending
				journal.Steps[i].Error = ""
			}
		}
		journal.UpdatedAt = time.Now()
		if err := m.cleanupStore.Save(journal); err != nil {
			return journal, err
		}
	}
	if claimErr := m.claimReviewCleanupSession(journal.Plan); claimErr != nil {
		for i := range journal.Steps {
			if journal.Steps[i].Status == ReviewCleanupStepSucceeded {
				continue
			}
			journal.Steps[i].Status = ReviewCleanupStepFailed
			journal.Steps[i].Error = claimErr.Error()
			journal.Steps[i].UpdatedAt = time.Now()
			break
		}
		journal.Status = ReviewCleanupFailed
		journal.Error = claimErr.Error()
		journal.UpdatedAt = time.Now()
		if err := m.cleanupStore.Save(journal); err != nil {
			return journal, err
		}
		return journal, nil
	}

	for i := range journal.Steps {
		if journal.Steps[i].Status == ReviewCleanupStepSucceeded {
			continue
		}
		stepErr := m.runReviewCleanupStep(journal.Plan, journal.Steps[i].Name)
		journal.Steps[i].UpdatedAt = time.Now()
		journal.UpdatedAt = journal.Steps[i].UpdatedAt
		if stepErr != nil {
			journal.Steps[i].Status = ReviewCleanupStepFailed
			journal.Steps[i].Error = stepErr.Error()
			journal.Status = ReviewCleanupFailed
			journal.Error = fmt.Sprintf("%s: %v", journal.Steps[i].Name, stepErr)
			if err := m.cleanupStore.Save(journal); err != nil {
				return journal, err
			}
			return journal, nil
		}
		journal.Steps[i].Status = ReviewCleanupStepSucceeded
		journal.Steps[i].Error = ""
		if err := m.cleanupStore.Save(journal); err != nil {
			return journal, err
		}
	}
	journal.Status = ReviewCleanupSucceeded
	journal.Error = ""
	journal.UpdatedAt = time.Now()
	if err := m.cleanupStore.Save(journal); err != nil {
		return journal, err
	}
	return journal, nil
}

// claimReviewCleanupSession prevents a stopped session from being revived
// between a partial cleanup and its retry. It also repeats the active-pane
// guard even when the journal's session_stop step already succeeded before a
// daemon restart.
func (m *Manager) claimReviewCleanupSession(plan ReviewCleanupPlan) error {
	m.mu.RLock()
	sess, exists := m.sessions[plan.SessionID]
	if !exists {
		m.mu.RUnlock()
		return nil
	}
	window, paneID := sess.TmuxWindowName, sess.TmuxPaneID
	tc := m.tmuxClient
	m.mu.RUnlock()
	if activeCleanupPane(tc, window, paneID) {
		return fmt.Errorf("session pane is active; stop it before retrying")
	}

	m.mu.Lock()
	live, exists := m.sessions[plan.SessionID]
	if !exists {
		m.mu.Unlock()
		return nil
	}
	if live.TmuxWindowName != window || live.TmuxPaneID != paneID {
		m.mu.Unlock()
		return fmt.Errorf("session process identity changed during cleanup; retry")
	}
	if live.Status == StatusDeleting && live.ReviewCleanupKey != plan.IdempotencyKey {
		m.mu.Unlock()
		return fmt.Errorf("another session deletion owns this session")
	}
	if live.ReviewBase.WorktreePath != plan.WorktreePath || live.ReviewFacts.HeadCommit != plan.HeadCommit ||
		live.MergeHandoff.Result.HeadCommit != plan.HeadCommit || live.MergeHandoff.Result.TargetCommit != plan.MergeTargetCommit ||
		validateReviewCleanupMergeReceipt(live) != nil {
		m.mu.Unlock()
		return fmt.Errorf("session merge evidence changed after cleanup confirmation")
	}
	live.Status = StatusDeleting
	live.ReviewCleanupKey = plan.IdempotencyKey
	saved := *live
	m.mu.Unlock()
	if err := m.store.Save(saved); err != nil {
		return err
	}
	if activeCleanupPane(tc, window, paneID) {
		return fmt.Errorf("session pane became active during cleanup; stop it before retrying")
	}
	return nil
}

func (m *Manager) runReviewCleanupStep(plan ReviewCleanupPlan, step ReviewCleanupStepName) error {
	switch step {
	case ReviewCleanupStopSession:
		m.mu.RLock()
		sess, exists := m.sessions[plan.SessionID]
		window := plan.TmuxWindowName
		if exists {
			window = sess.TmuxWindowName
		}
		tc := m.tmuxClient
		m.mu.RUnlock()
		if window == "" || tc == nil || !tc.HasSession(window) {
			return nil
		}
		return tc.KillSession(window)

	case ReviewCleanupRemoveWorktree:
		if _, err := os.Stat(plan.WorktreePath); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), reviewProbeTimeout)
		defer cancel()
		observed, err := m.gitClient.InspectCleanupWorktree(ctx, plan.WorktreePath, plan.HeadCommit)
		if err != nil {
			return err
		}
		if filepath.Clean(observed.WorktreePath) != filepath.Clean(plan.WorktreePath) ||
			filepath.Clean(observed.RepositoryPath) != filepath.Clean(plan.RepositoryPath) ||
			observed.Branch != plan.Branch || observed.HeadCommit != plan.HeadCommit {
			return fmt.Errorf("worktree ownership changed after cleanup confirmation")
		}
		if observed.UnpushedCommits != 0 {
			return fmt.Errorf("worktree has %d commit(s) after the verified merge head", observed.UnpushedCommits)
		}
		clean, err := m.gitClient.ReviewWorktreeClean(ctx, plan.WorktreePath)
		if err != nil {
			return err
		}
		if !clean {
			return ErrWorktreeDirty
		}
		return m.gitClient.RemoveWorktree(plan.WorktreePath, false)

	case ReviewCleanupDeleteBranch:
		head, exists, err := m.gitClient.LocalBranchHead(plan.RepositoryPath, plan.Branch)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if head != plan.HeadCommit {
			return fmt.Errorf("local branch %q moved from %s to %s", plan.Branch, plan.HeadCommit, head)
		}
		return m.gitClient.DeleteExactBranch(plan.RepositoryPath, plan.Branch, plan.HeadCommit)

	case ReviewCleanupDeleteSession:
		m.mu.Lock()
		defer m.mu.Unlock()
		if err := m.store.Delete(plan.SessionID); err != nil {
			return err
		}
		delete(m.sessions, plan.SessionID)
		return nil
	default:
		return fmt.Errorf("unknown cleanup step %q", step)
	}
}

// ReviewCleanupJournal returns a persisted journal by exact session ID.
func (m *Manager) ReviewCleanupJournal(id string) (ReviewCleanupJournal, error) {
	return m.cleanupStore.Load(id)
}
