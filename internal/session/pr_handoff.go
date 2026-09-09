package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const PRHandoffSchemaVersion = 1

type PRHandoffStatus string

const (
	PRHandoffRunning   PRHandoffStatus = "running"
	PRHandoffSucceeded PRHandoffStatus = "succeeded"
	PRHandoffFailed    PRHandoffStatus = "failed"
	PRHandoffUnknown   PRHandoffStatus = "unknown"
)

type PRHandoffTarget struct {
	Plugin string `json:"plugin"`
	Action string `json:"action"`
}

// PRHandoffProviderResult is the only provider-controlled value persisted in
// a session. Every string is bounded and URLs with credentials/query strings
// are rejected before persistence.
type PRHandoffProviderResult struct {
	Status   PRHandoffStatus `json:"status"`
	Provider string          `json:"provider,omitempty"`
	ID       string          `json:"id,omitempty"`
	URL      string          `json:"url,omitempty"`
	Message  string          `json:"message,omitempty"`
}

type PRHandoff struct {
	IdempotencyKey       string                  `json:"idempotency_key"`
	Target               PRHandoffTarget         `json:"target"`
	WorkspaceFingerprint string                  `json:"workspace_fingerprint"`
	Status               PRHandoffStatus         `json:"status"`
	Result               PRHandoffProviderResult `json:"result,omitzero"`
	Error                string                  `json:"error,omitempty"`
	StartedAt            time.Time               `json:"started_at"`
	UpdatedAt            time.Time               `json:"updated_at"`
}

func (h PRHandoff) IsZero() bool { return h == (PRHandoff{}) }

type PRHandoffInfo struct {
	PRHandoff
	Stale bool `json:"stale"`
}

func (h PRHandoffInfo) IsZero() bool { return h.PRHandoff.IsZero() }

func (h PRHandoff) stale(facts ReviewFacts) bool {
	return !h.IsZero() && (facts.Status != ReviewFactsAvailable ||
		facts.WorkspaceFingerprint == "" || h.WorkspaceFingerprint != facts.WorkspaceFingerprint)
}

func (h PRHandoff) toInfo(facts ReviewFacts) PRHandoffInfo {
	if h.IsZero() {
		return PRHandoffInfo{}
	}
	return PRHandoffInfo{PRHandoff: h, Stale: h.stale(facts)}
}

func mergePRHandoff(a, b PRHandoff) PRHandoff {
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
	// A stale running snapshot must never roll a terminal outcome back when
	// timestamps tie after JSON/time normalization.
	if handoffStatusRank(a.Status) > handoffStatusRank(b.Status) {
		return a
	}
	return b
}

func handoffStatusRank(status PRHandoffStatus) int {
	if status == PRHandoffRunning {
		return 0
	}
	return 1
}

type PRHandoffReviewEvidence struct {
	BaseCommit           string `json:"base_commit"`
	HeadCommit           string `json:"head_commit"`
	Branch               string `json:"branch"`
	WorkspaceFingerprint string `json:"workspace_fingerprint"`
	ChangedFiles         int    `json:"changed_files"`
	Additions            int    `json:"additions"`
	Deletions            int    `json:"deletions"`
	BinaryFiles          int    `json:"binary_files,omitempty"`
	CommitCount          int    `json:"commit_count"`
}

type PRHandoffCheckEvidence struct {
	Source     string      `json:"source"`
	Status     CheckStatus `json:"status"`
	ReportedAt time.Time   `json:"reported_at"`
}

// PRHandoffRequest is deliberately smaller than session.Info: it contains no
// worktree path, prompt, transcript, patch, environment, or credential.
type PRHandoffRequest struct {
	SchemaVersion  int                     `json:"schema_version"`
	Kind           string                  `json:"kind"`
	IdempotencyKey string                  `json:"idempotency_key"`
	SessionID      string                  `json:"session_id"`
	Repository     string                  `json:"repository,omitempty"`
	Review         PRHandoffReviewEvidence `json:"review"`
	Checks         *PRHandoffCheckEvidence `json:"checks,omitempty"`
	ReviewedAt     time.Time               `json:"reviewed_at"`
}

var handoffKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func validatePRHandoffKey(key string) error {
	if !handoffKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid idempotency key %q (use 1-128 letters, digits, '.', '_', ':', or '-')", key)
	}
	return nil
}

func defaultPRHandoffKey(sessionID, fingerprint string, target PRHandoffTarget) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + fingerprint + "\x00" + target.Plugin + "\x00" + target.Action))
	return "prh_" + hex.EncodeToString(sum[:16])
}

func ValidatePRHandoffProviderResult(result PRHandoffProviderResult) error {
	if result.Status != PRHandoffSucceeded && result.Status != PRHandoffFailed {
		return fmt.Errorf("provider result status must be succeeded or failed")
	}
	for label, value := range map[string]string{
		"provider": result.Provider,
		"id":       result.ID,
		"message":  result.Message,
	} {
		limit := 256
		if label == "provider" {
			limit = 64
		} else if label == "message" {
			limit = 512
		}
		if len(value) > limit || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("provider result %s is invalid or exceeds %d bytes", label, limit)
		}
	}
	if len(result.URL) > 2048 {
		return fmt.Errorf("provider result url exceeds 2048 bytes")
	}
	if result.URL != "" {
		u, err := url.Parse(result.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("provider result url must be an http(s) URL without credentials, query, or fragment")
		}
	}
	if result.Status == PRHandoffSucceeded && result.ID == "" && result.URL == "" {
		return fmt.Errorf("successful provider result requires id or url")
	}
	return nil
}
