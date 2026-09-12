package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const MergeHandoffSchemaVersion = 1

type MergeHandoffOperation string

const (
	MergeHandoffPreflight MergeHandoffOperation = "preflight"
	MergeHandoffExecute   MergeHandoffOperation = "merge"
)

type MergeHandoffStatus string

const (
	MergeHandoffRunning   MergeHandoffStatus = "running"
	MergeHandoffSucceeded MergeHandoffStatus = "succeeded"
	MergeHandoffFailed    MergeHandoffStatus = "failed"
	MergeHandoffUnknown   MergeHandoffStatus = "unknown"
)

type MergeHandoffTarget struct {
	Plugin string `json:"plugin"`
	Action string `json:"action"`
}

type MergeRequiredChecks string

const (
	MergeChecksPassed  MergeRequiredChecks = "passed"
	MergeChecksFailed  MergeRequiredChecks = "failed"
	MergeChecksPending MergeRequiredChecks = "pending"
	MergeChecksUnknown MergeRequiredChecks = "unknown"
)

type MergeProviderTarget struct {
	Provider   string `json:"provider"`
	ID         string `json:"id,omitempty"`
	URL        string `json:"url,omitempty"`
	BaseRef    string `json:"base_ref"`
	BaseCommit string `json:"base_commit"`
	HeadCommit string `json:"head_commit"`
}

type MergeHandoffPreflightResult struct {
	Status         string              `json:"status"`
	Target         MergeProviderTarget `json:"target"`
	Mergeable      bool                `json:"mergeable"`
	RequiredChecks MergeRequiredChecks `json:"required_checks"`
	Message        string              `json:"message,omitempty"`
}

func (r MergeHandoffPreflightResult) ready() bool {
	return r.Status == "ready" && r.Mergeable && r.RequiredChecks == MergeChecksPassed
}

type MergeHandoffProviderResult struct {
	Status       MergeHandoffStatus `json:"status"`
	Provider     string             `json:"provider"`
	ID           string             `json:"id,omitempty"`
	URL          string             `json:"url,omitempty"`
	HeadCommit   string             `json:"head_commit"`
	TargetCommit string             `json:"target_commit,omitempty"`
	Method       string             `json:"method,omitempty"`
	Message      string             `json:"message,omitempty"`
}

type MergeHandoff struct {
	IdempotencyKey       string                      `json:"idempotency_key"`
	Target               MergeHandoffTarget          `json:"target"`
	WorkspaceFingerprint string                      `json:"workspace_fingerprint"`
	PRTarget             MergeProviderTarget         `json:"pr_target"`
	Preflight            MergeHandoffPreflightResult `json:"preflight"`
	Status               MergeHandoffStatus          `json:"status"`
	Result               MergeHandoffProviderResult  `json:"result,omitzero"`
	Error                string                      `json:"error,omitempty"`
	StartedAt            time.Time                   `json:"started_at"`
	UpdatedAt            time.Time                   `json:"updated_at"`
}

func (h MergeHandoff) IsZero() bool { return h == (MergeHandoff{}) }

type MergeHandoffInfo struct {
	MergeHandoff
	Stale bool `json:"stale"`
}

func (h MergeHandoffInfo) IsZero() bool { return h.MergeHandoff.IsZero() }

func (h MergeHandoff) stale(facts ReviewFacts, pr PRHandoff) bool {
	return !h.IsZero() && (facts.Status != ReviewFactsAvailable ||
		facts.WorkspaceFingerprint == "" || h.WorkspaceFingerprint != facts.WorkspaceFingerprint ||
		pr.Status != PRHandoffSucceeded || pr.stale(facts))
}

func (h MergeHandoff) toInfo(facts ReviewFacts, pr PRHandoff) MergeHandoffInfo {
	if h.IsZero() {
		return MergeHandoffInfo{}
	}
	return MergeHandoffInfo{MergeHandoff: h, Stale: h.stale(facts, pr)}
}

func mergeMergeHandoff(a, b MergeHandoff) MergeHandoff {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if a.UpdatedAt.After(b.UpdatedAt) {
		return a
	}
	if b.UpdatedAt.After(a.UpdatedAt) {
		return b
	}
	if mergeHandoffStatusRank(a.Status) > mergeHandoffStatusRank(b.Status) {
		return a
	}
	return b
}

func mergeHandoffStatusRank(status MergeHandoffStatus) int {
	if status == MergeHandoffRunning {
		return 0
	}
	return 1
}

type MergeHandoffRequest struct {
	SchemaVersion  int                          `json:"schema_version"`
	Kind           string                       `json:"kind"`
	Operation      MergeHandoffOperation        `json:"operation"`
	IdempotencyKey string                       `json:"idempotency_key"`
	SessionID      string                       `json:"session_id"`
	Repository     string                       `json:"repository,omitempty"`
	PullRequest    MergeProviderTarget          `json:"pull_request"`
	Review         PRHandoffReviewEvidence      `json:"review"`
	Checks         *PRHandoffCheckEvidence      `json:"reported_checks,omitempty"`
	ReviewedAt     time.Time                    `json:"reviewed_at"`
	Preflight      *MergeHandoffPreflightResult `json:"preflight,omitempty"`
}

var mergeCommitPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

func defaultMergeHandoffKey(sessionID, fingerprint string, pr MergeProviderTarget, target MergeHandoffTarget) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + fingerprint + "\x00" + pr.Provider + "\x00" + pr.ID + "\x00" + pr.URL + "\x00" + target.Plugin + "\x00" + target.Action))
	return "mrg_" + hex.EncodeToString(sum[:16])
}

func ValidateMergeHandoffPreflight(result MergeHandoffPreflightResult, request MergeHandoffRequest) error {
	if result.Status != "ready" && result.Status != "blocked" {
		return fmt.Errorf("merge preflight status must be ready or blocked")
	}
	if err := validateMergeProviderTarget(result.Target); err != nil {
		return err
	}
	if result.RequiredChecks != MergeChecksPassed && result.RequiredChecks != MergeChecksFailed &&
		result.RequiredChecks != MergeChecksPending && result.RequiredChecks != MergeChecksUnknown {
		return fmt.Errorf("merge preflight required_checks must be passed, failed, pending, or unknown")
	}
	if err := validateBoundedProviderString("message", result.Message, 512); err != nil {
		return err
	}
	if !samePullRequestTarget(result.Target, request.PullRequest) {
		return fmt.Errorf("merge provider target does not match the successful PR handoff")
	}
	if result.Target.HeadCommit != request.Review.HeadCommit {
		return fmt.Errorf("merge provider head %s does not match reviewed head %s", result.Target.HeadCommit, request.Review.HeadCommit)
	}
	if result.Status == "ready" && (!result.Mergeable || result.RequiredChecks != MergeChecksPassed) {
		return fmt.Errorf("ready merge preflight must be mergeable with required checks passed")
	}
	return nil
}

func ValidateMergeHandoffProviderResult(result MergeHandoffProviderResult, request MergeHandoffRequest) error {
	if result.Status != MergeHandoffSucceeded && result.Status != MergeHandoffFailed {
		return fmt.Errorf("merge provider result status must be succeeded or failed")
	}
	for label, value := range map[string]string{
		"provider": result.Provider, "id": result.ID, "method": result.Method, "message": result.Message,
	} {
		limit := 256
		if label == "provider" || label == "method" {
			limit = 64
		} else if label == "message" {
			limit = 512
		}
		if err := validateBoundedProviderString(label, value, limit); err != nil {
			return err
		}
	}
	if err := validateProviderURL(result.URL); err != nil {
		return err
	}
	if result.Provider != request.PullRequest.Provider || result.ID != request.PullRequest.ID || result.URL != request.PullRequest.URL {
		return fmt.Errorf("merge provider result does not match the successful PR handoff target")
	}
	if result.HeadCommit != request.Review.HeadCommit {
		return fmt.Errorf("merge provider result head does not match reviewed head")
	}
	if result.Status == MergeHandoffSucceeded && !mergeCommitPattern.MatchString(result.TargetCommit) {
		return fmt.Errorf("successful merge result requires a full lowercase target commit ID")
	}
	if result.TargetCommit != "" && !mergeCommitPattern.MatchString(result.TargetCommit) {
		return fmt.Errorf("merge target commit must be a full lowercase commit ID")
	}
	return nil
}

func validateMergeProviderTarget(target MergeProviderTarget) error {
	if err := validateBoundedProviderString("provider", target.Provider, 64); err != nil || target.Provider == "" {
		if err != nil {
			return err
		}
		return fmt.Errorf("merge provider is required")
	}
	if err := validateBoundedProviderString("id", target.ID, 256); err != nil {
		return err
	}
	if err := validateProviderURL(target.URL); err != nil {
		return err
	}
	if target.ID == "" && target.URL == "" {
		return fmt.Errorf("merge provider target requires id or url")
	}
	if err := validateBoundedProviderString("base_ref", target.BaseRef, 256); err != nil || target.BaseRef == "" {
		if err != nil {
			return err
		}
		return fmt.Errorf("merge provider base_ref is required")
	}
	if !mergeCommitPattern.MatchString(target.BaseCommit) || !mergeCommitPattern.MatchString(target.HeadCommit) {
		return fmt.Errorf("merge provider base/head must be full lowercase commit IDs")
	}
	return nil
}

func samePullRequestTarget(actual, expected MergeProviderTarget) bool {
	if actual.Provider != expected.Provider || actual.ID != expected.ID || actual.URL != expected.URL {
		return false
	}
	return true
}

func validateBoundedProviderString(label, value string, limit int) error {
	if len(value) > limit || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("provider result %s is invalid or exceeds %d bytes", label, limit)
	}
	return nil
}
