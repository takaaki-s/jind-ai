package session

import (
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// CheckReceiptLimit bounds the per-session journal. Keys are never evicted:
// forgetting one would turn a delayed retry into a new report.
const CheckReceiptLimit = 128

// CheckReportMetadata is caller-supplied audit context, not authenticated
// identity. Times describe the external check run rather than receipt time.
type CheckReportMetadata struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Name           string    `json:"name"`
	Reporter       string    `json:"reporter"`
	Summary        string    `json:"summary,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
}

type CheckReportSubmission struct {
	CheckReportMetadata
	Status               CheckStatus `json:"status"`
	WorkspaceFingerprint string      `json:"workspace_fingerprint"`
}

func (s CheckReportSubmission) Validate() error {
	if s.Status != CheckStatusPassed && s.Status != CheckStatusFailed {
		return fmt.Errorf("check status must be passed or failed")
	}
	for _, field := range []struct {
		name, value string
		limit       int
	}{
		{"idempotency_key", s.IdempotencyKey, 128}, {"name", s.Name, 128}, {"reporter", s.Reporter, 128},
	} {
		if strings.TrimSpace(field.value) == "" || !boundedCheckText(field.value, field.limit) {
			return fmt.Errorf("%s must be non-empty UTF-8 text without controls, at most %d bytes", field.name, field.limit)
		}
	}
	if !boundedCheckText(s.Summary, 2048) {
		return fmt.Errorf("summary must be UTF-8 text without controls, at most 2048 bytes")
	}
	if len(s.WorkspaceFingerprint) != 64 {
		return fmt.Errorf("workspace fingerprint must be a lowercase SHA-256 digest")
	}
	if _, err := hex.DecodeString(s.WorkspaceFingerprint); err != nil || s.WorkspaceFingerprint != strings.ToLower(s.WorkspaceFingerprint) {
		return fmt.Errorf("workspace fingerprint must be a lowercase SHA-256 digest")
	}
	if s.StartedAt.IsZero() || s.FinishedAt.IsZero() || s.FinishedAt.Before(s.StartedAt) {
		return fmt.Errorf("started_at and finished_at are required and must be ordered")
	}
	return nil
}

func boundedCheckText(value string, limit int) bool {
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

type CheckReportReceipt struct {
	CheckReport
	CheckReportMetadata
}

func (r CheckReportReceipt) IsZero() bool { return r == (CheckReportReceipt{}) }

type CheckReportRecordResult struct {
	Receipt CheckReportReceipt `json:"receipt"`
	Reused  bool               `json:"reused"`
	Stale   bool               `json:"stale"`
	Session Info               `json:"session"`
}

func (r CheckReportReceipt) matches(s CheckReportSubmission) bool {
	return r.Status == s.Status && r.WorkspaceFingerprint == s.WorkspaceFingerprint &&
		r.IdempotencyKey == s.IdempotencyKey && r.Name == s.Name && r.Reporter == s.Reporter &&
		r.Summary == s.Summary && r.StartedAt.Equal(s.StartedAt) && r.FinishedAt.Equal(s.FinishedAt)
}

func (s *Session) latestCheckReceipt() CheckReportReceipt {
	for _, r := range s.CheckReceipts {
		if r.CheckReport == s.CheckReport {
			return r
		}
	}
	return CheckReportReceipt{}
}

func mergeCheckReceipts(a, b []CheckReportReceipt) ([]CheckReportReceipt, error) {
	if len(a) == 0 && len(b) == 0 {
		return nil, nil
	}
	merged := make(map[string]CheckReportReceipt, len(a)+len(b))
	for _, receipts := range [][]CheckReportReceipt{a, b} {
		for _, r := range receipts {
			if old, ok := merged[r.IdempotencyKey]; ok && old != r {
				return nil, fmt.Errorf("conflicting persisted check receipt")
			}
			merged[r.IdempotencyKey] = r
		}
	}
	if len(merged) > CheckReceiptLimit {
		return nil, fmt.Errorf("check receipt journal is full")
	}
	out := make([]CheckReportReceipt, 0, len(merged))
	for _, r := range merged {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IdempotencyKey < out[j].IdempotencyKey })
	return out, nil
}

// RecordChecks accepts evidence of an external run only if the caller's
// fingerprint still matches. A retry returns its original receipt without
// replacing a newer aggregate result or rebinding the old result to new code.
func (m *Manager) RecordChecks(id string, submission CheckReportSubmission) (CheckReportRecordResult, error) {
	if err := submission.Validate(); err != nil {
		return CheckReportRecordResult{}, err
	}
	submission.StartedAt = submission.StartedAt.UTC()
	submission.FinishedAt = submission.FinishedAt.UTC()
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return CheckReportRecordResult{}, fmt.Errorf("session not found")
	}
	if result, found, err := existingCheckReceipt(sess, submission); found || err != nil {
		saved := m.snapshotAndUnlock(sess)
		if err != nil {
			return result, err
		}
		return result, m.store.Save(saved)
	}
	generation := sess.Attention.Generation
	m.mu.Unlock()
	if generation == 0 {
		return CheckReportRecordResult{}, fmt.Errorf("session has no completed turn to report checks for")
	}
	if _, err := m.RefreshReview(id); err != nil {
		return CheckReportRecordResult{}, err
	}
	m.mu.Lock()
	live, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return CheckReportRecordResult{}, fmt.Errorf("session not found")
	}
	if result, found, err := existingCheckReceipt(live, submission); found || err != nil {
		saved := m.snapshotAndUnlock(live)
		if err != nil {
			return result, err
		}
		return result, m.store.Save(saved)
	}
	if live.Attention.Generation != generation || live.ReviewFacts.Status != ReviewFactsAvailable ||
		live.ReviewFacts.AttentionGeneration != generation || live.ReviewFacts.WorkspaceFingerprint != submission.WorkspaceFingerprint {
		m.mu.Unlock()
		return CheckReportRecordResult{}, fmt.Errorf("workspace evidence changed or is unavailable; rerun checks against refreshed evidence")
	}
	if len(live.CheckReceipts) >= CheckReceiptLimit {
		m.mu.Unlock()
		return CheckReportRecordResult{}, fmt.Errorf("check receipt journal is full; start a new execution")
	}
	at := time.Now().UTC()
	if !at.After(live.CheckReport.ReportedAt) {
		at = live.CheckReport.ReportedAt.Add(time.Nanosecond)
	}
	receipt := CheckReportReceipt{
		CheckReport:         CheckReport{Source: CheckSourceReported, Status: submission.Status, WorkspaceFingerprint: submission.WorkspaceFingerprint, ReportedAt: at},
		CheckReportMetadata: submission.CheckReportMetadata,
	}
	live.CheckReceipts = append(slices.Clone(live.CheckReceipts), receipt)
	live.CheckReport = receipt.CheckReport
	live.Attention = reconcileCheckAttention(live.Attention, live.ReviewFacts, live.CheckReport)
	saved := m.snapshotAndUnlock(live)
	result := CheckReportRecordResult{Receipt: receipt, Session: saved.ToInfo()}
	return result, m.store.Save(saved)
}

func existingCheckReceipt(sess *Session, s CheckReportSubmission) (CheckReportRecordResult, bool, error) {
	for _, r := range sess.CheckReceipts {
		if r.IdempotencyKey != s.IdempotencyKey {
			continue
		}
		if !r.matches(s) {
			return CheckReportRecordResult{}, true, fmt.Errorf("idempotency key conflicts with an existing check report")
		}
		return CheckReportRecordResult{Receipt: r, Reused: true, Stale: r.stale(sess.ReviewFacts), Session: sess.ToInfo()}, true, nil
	}
	return CheckReportRecordResult{}, false, nil
}
