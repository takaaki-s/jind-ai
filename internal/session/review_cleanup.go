package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"
)

const ReviewCleanupSchemaVersion = 1

type ReviewCleanupStatus string

const (
	ReviewCleanupRunning   ReviewCleanupStatus = "running"
	ReviewCleanupSucceeded ReviewCleanupStatus = "succeeded"
	ReviewCleanupFailed    ReviewCleanupStatus = "failed"
)

type ReviewCleanupStepName string

const (
	ReviewCleanupStopSession    ReviewCleanupStepName = "session_stop"
	ReviewCleanupRemoveWorktree ReviewCleanupStepName = "worktree_remove"
	ReviewCleanupDeleteBranch   ReviewCleanupStepName = "local_branch_delete"
	ReviewCleanupDeleteSession  ReviewCleanupStepName = "session_delete"
)

type ReviewCleanupStepStatus string

const (
	ReviewCleanupStepPending   ReviewCleanupStepStatus = "pending"
	ReviewCleanupStepSucceeded ReviewCleanupStepStatus = "succeeded"
	ReviewCleanupStepFailed    ReviewCleanupStepStatus = "failed"
)

// ReviewCleanupPlan is the exact local deletion boundary shown by dry-run and
// persisted before confirmation performs its first destructive step.
type ReviewCleanupPlan struct {
	SchemaVersion      int       `json:"schema_version"`
	IdempotencyKey     string    `json:"idempotency_key"`
	SessionID          string    `json:"session_id"`
	SessionDescription string    `json:"session_description,omitempty"`
	TmuxWindowName     string    `json:"tmux_window_name,omitempty"`
	WorktreePath       string    `json:"worktree_path"`
	RepositoryPath     string    `json:"repository_path"`
	Branch             string    `json:"branch"`
	HeadCommit         string    `json:"head_commit"`
	MergeTargetCommit  string    `json:"merge_target_commit"`
	UnpushedCommits    int       `json:"unpushed_commits"`
	Blockers           []string  `json:"blockers,omitempty"`
	Ready              bool      `json:"ready"`
	PlannedAt          time.Time `json:"planned_at"`
}

type ReviewCleanupStep struct {
	Name      ReviewCleanupStepName   `json:"name"`
	Target    string                  `json:"target"`
	Status    ReviewCleanupStepStatus `json:"status"`
	Error     string                  `json:"error,omitempty"`
	UpdatedAt time.Time               `json:"updated_at,omitzero"`
}

// ReviewCleanupJournal survives deletion of the Session record so a caller
// can audit the result or retry a partially completed cleanup by exact ID and
// idempotency key after a daemon/client restart.
type ReviewCleanupJournal struct {
	Plan      ReviewCleanupPlan   `json:"plan"`
	Status    ReviewCleanupStatus `json:"status"`
	Steps     []ReviewCleanupStep `json:"steps"`
	Error     string              `json:"error,omitempty"`
	StartedAt time.Time           `json:"started_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

func (j ReviewCleanupJournal) IsZero() bool { return j.Status == "" }

func defaultReviewCleanupKey(sessionID, fingerprint, mergeKey string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + fingerprint + "\x00" + mergeKey))
	return "cln_" + hex.EncodeToString(sum[:16])
}

func newReviewCleanupJournal(plan ReviewCleanupPlan, now time.Time) ReviewCleanupJournal {
	return ReviewCleanupJournal{
		Plan: plan, Status: ReviewCleanupRunning, StartedAt: now, UpdatedAt: now,
		Steps: []ReviewCleanupStep{
			{Name: ReviewCleanupStopSession, Target: plan.TmuxWindowName, Status: ReviewCleanupStepPending},
			{Name: ReviewCleanupRemoveWorktree, Target: plan.WorktreePath, Status: ReviewCleanupStepPending},
			{Name: ReviewCleanupDeleteBranch, Target: plan.Branch, Status: ReviewCleanupStepPending},
			{Name: ReviewCleanupDeleteSession, Target: plan.SessionID, Status: ReviewCleanupStepPending},
		},
	}
}

func validateReviewCleanupJournal(journal ReviewCleanupJournal) error {
	plan := journal.Plan
	if plan.SchemaVersion != ReviewCleanupSchemaVersion || !cleanupSessionIDPattern.MatchString(plan.SessionID) {
		return fmt.Errorf("invalid cleanup journal identity or schema")
	}
	if err := validatePRHandoffKey(plan.IdempotencyKey); err != nil {
		return err
	}
	if !plan.Ready || len(plan.Blockers) != 0 || plan.UnpushedCommits != 0 ||
		!safeCleanupPlanPath(plan.WorktreePath) || !safeCleanupPlanPath(plan.RepositoryPath) {
		return fmt.Errorf("cleanup journal contains an unsafe or blocked plan")
	}
	if plan.Branch == "" || !mergeCommitPattern.MatchString(plan.HeadCommit) || !mergeCommitPattern.MatchString(plan.MergeTargetCommit) {
		return fmt.Errorf("cleanup journal branch or commit identity is invalid")
	}
	if journal.Status != ReviewCleanupRunning && journal.Status != ReviewCleanupSucceeded && journal.Status != ReviewCleanupFailed {
		return fmt.Errorf("cleanup journal status %q is invalid", journal.Status)
	}
	wantNames := []ReviewCleanupStepName{
		ReviewCleanupStopSession, ReviewCleanupRemoveWorktree,
		ReviewCleanupDeleteBranch, ReviewCleanupDeleteSession,
	}
	wantTargets := []string{plan.TmuxWindowName, plan.WorktreePath, plan.Branch, plan.SessionID}
	if len(journal.Steps) != len(wantNames) {
		return fmt.Errorf("cleanup journal must contain exactly four ordered steps")
	}
	for i, step := range journal.Steps {
		if step.Name != wantNames[i] || step.Target != wantTargets[i] {
			return fmt.Errorf("cleanup journal step %d identity is invalid", i)
		}
		if step.Status != ReviewCleanupStepPending && step.Status != ReviewCleanupStepSucceeded && step.Status != ReviewCleanupStepFailed {
			return fmt.Errorf("cleanup journal step %q status is invalid", step.Name)
		}
	}
	return nil
}

func safeCleanupPlanPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Dir(path) != path
}
