package session

import (
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// ReviewReceiptLimit bounds the per-session journal. Keys are never evicted:
// forgetting one would turn a delayed retry into a new report.
const ReviewReceiptLimit = 128

// ReviewDispositionMetadata is caller-reported context, not authenticated identity.
type ReviewDispositionMetadata struct {
	IdempotencyKey string `json:"idempotency_key"`
	Actor          string `json:"actor"`
	Note           string `json:"note,omitempty"`
}

type ReviewDispositionSubmission struct {
	ReviewDispositionMetadata
	Decision             ReviewDecision `json:"decision"`
	WorkspaceFingerprint string         `json:"workspace_fingerprint"`
}

func (s ReviewDispositionSubmission) Validate() error {
	if s.Decision != ReviewDecisionReviewed && s.Decision != ReviewDecisionChangesRequested {
		return fmt.Errorf("decision must be reviewed or changes-requested")
	}
	for _, field := range []struct {
		name, value string
		limit       int
	}{
		{"idempotency_key", s.IdempotencyKey, 128}, {"actor", s.Actor, 128},
	} {
		if strings.TrimSpace(field.value) == "" || !boundedCheckText(field.value, field.limit) {
			return fmt.Errorf("%s must be non-empty UTF-8 text without controls, at most %d bytes", field.name, field.limit)
		}
	}
	if !boundedCheckText(s.Note, 2048) {
		return fmt.Errorf("note must be UTF-8 text without controls, at most 2048 bytes")
	}
	if len(s.WorkspaceFingerprint) != 64 {
		return fmt.Errorf("workspace fingerprint must be a lowercase SHA-256 digest")
	}
	if _, err := hex.DecodeString(s.WorkspaceFingerprint); err != nil || s.WorkspaceFingerprint != strings.ToLower(s.WorkspaceFingerprint) {
		return fmt.Errorf("workspace fingerprint must be a lowercase SHA-256 digest")
	}
	return nil
}

type ReviewDispositionReceipt struct {
	ReviewDisposition
	ReviewDispositionMetadata
}

func (r ReviewDispositionReceipt) IsZero() bool { return r == (ReviewDispositionReceipt{}) }

type ReviewDispositionRecordResult struct {
	Receipt ReviewDispositionReceipt `json:"receipt"`
	Reused  bool                     `json:"reused"`
	Stale   bool                     `json:"stale"`
	Session Info                     `json:"session"`
}

func (r ReviewDispositionReceipt) matches(s ReviewDispositionSubmission) bool {
	return r.Decision == s.Decision && r.WorkspaceFingerprint == s.WorkspaceFingerprint &&
		r.ReviewDispositionMetadata == s.ReviewDispositionMetadata
}

func (s *Session) latestReviewReceipt() ReviewDispositionReceipt {
	for _, r := range s.ReviewReceipts {
		if r.ReviewDisposition == s.ReviewDisposition {
			return r
		}
	}
	return ReviewDispositionReceipt{}
}

func mergeReviewReceipts(a, b []ReviewDispositionReceipt) ([]ReviewDispositionReceipt, error) {
	if len(a) == 0 && len(b) == 0 {
		return nil, nil
	}
	merged := make(map[string]ReviewDispositionReceipt, len(a)+len(b))
	for _, receipts := range [][]ReviewDispositionReceipt{a, b} {
		for _, r := range receipts {
			if old, ok := merged[r.IdempotencyKey]; ok && old != r {
				return nil, fmt.Errorf("conflicting persisted review receipt")
			}
			merged[r.IdempotencyKey] = r
		}
	}
	if len(merged) > ReviewReceiptLimit {
		return nil, fmt.Errorf("review receipt journal is full")
	}
	out := make([]ReviewDispositionReceipt, 0, len(merged))
	for _, r := range merged {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IdempotencyKey < out[j].IdempotencyKey })
	return out, nil
}

// RecordReviewDisposition accepts a decision for the inspected workspace.
// Known retries return their original receipt without replacing later decisions.
func (m *Manager) RecordReviewDisposition(id string, submission ReviewDispositionSubmission) (ReviewDispositionRecordResult, error) {
	if err := submission.Validate(); err != nil {
		return ReviewDispositionRecordResult{}, err
	}
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return ReviewDispositionRecordResult{}, fmt.Errorf("session not found")
	}
	if result, found, err := existingReviewReceipt(sess, submission); found || err != nil {
		saved := m.snapshotAndUnlock(sess)
		if err != nil {
			return result, err
		}
		return result, m.store.Save(saved)
	}
	generation := sess.Attention.Generation
	m.mu.Unlock()
	if generation == 0 {
		return ReviewDispositionRecordResult{}, fmt.Errorf("session has no completed turn to review")
	}
	if _, err := m.RefreshReview(id); err != nil {
		return ReviewDispositionRecordResult{}, err
	}
	m.mu.Lock()
	live, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return ReviewDispositionRecordResult{}, fmt.Errorf("session not found")
	}
	if result, found, err := existingReviewReceipt(live, submission); found || err != nil {
		saved := m.snapshotAndUnlock(live)
		if err != nil {
			return result, err
		}
		return result, m.store.Save(saved)
	}
	if live.Attention.Generation != generation || live.ReviewFacts.Status != ReviewFactsAvailable ||
		live.ReviewFacts.ChangedFiles == 0 || live.ReviewFacts.AttentionGeneration != generation || live.ReviewFacts.WorkspaceFingerprint != submission.WorkspaceFingerprint {
		m.mu.Unlock()
		return ReviewDispositionRecordResult{}, fmt.Errorf("workspace evidence changed, is empty or unavailable; inspect refreshed evidence before reviewing")
	}
	if len(live.ReviewReceipts) >= ReviewReceiptLimit {
		m.mu.Unlock()
		return ReviewDispositionRecordResult{}, fmt.Errorf("review receipt journal is full; start a new execution")
	}
	at := time.Now().UTC()
	if !at.After(live.ReviewDisposition.ReportedAt) {
		at = live.ReviewDisposition.ReportedAt.Add(time.Nanosecond)
	}
	receipt := ReviewDispositionReceipt{
		ReviewDisposition:         ReviewDisposition{Source: ReviewDispositionSourceReported, Decision: submission.Decision, WorkspaceFingerprint: submission.WorkspaceFingerprint, ReportedAt: at},
		ReviewDispositionMetadata: submission.ReviewDispositionMetadata,
	}
	live.ReviewReceipts = append(slices.Clone(live.ReviewReceipts), receipt)
	live.ReviewDisposition = receipt.ReviewDisposition
	saved := m.snapshotAndUnlock(live)
	result := ReviewDispositionRecordResult{Receipt: receipt, Session: saved.ToInfo()}
	return result, m.store.Save(saved)
}

func existingReviewReceipt(sess *Session, s ReviewDispositionSubmission) (ReviewDispositionRecordResult, bool, error) {
	for _, r := range sess.ReviewReceipts {
		if r.IdempotencyKey != s.IdempotencyKey {
			continue
		}
		if !r.matches(s) {
			return ReviewDispositionRecordResult{}, true, fmt.Errorf("idempotency key conflicts with an existing review disposition")
		}
		return ReviewDispositionRecordResult{Receipt: r, Reused: true, Stale: r.stale(sess.ReviewFacts), Session: sess.ToInfo()}, true, nil
	}
	return ReviewDispositionRecordResult{}, false, nil
}
