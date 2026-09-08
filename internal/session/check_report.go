package session

import "time"

// CheckStatus is the aggregate result reported by an external check runner.
// jind-ai stores the claim but never discovers or executes repository checks.
type CheckStatus string

const (
	CheckStatusPassed CheckStatus = "passed"
	CheckStatusFailed CheckStatus = "failed"
)

const CheckSourceReported = "reported"

// CheckReport is the latest aggregate check result supplied explicitly by a
// caller. WorkspaceFingerprint binds the claim to the exact local checkout
// observed when it was accepted.
type CheckReport struct {
	Source               string      `json:"source"`
	Status               CheckStatus `json:"status"`
	WorkspaceFingerprint string      `json:"workspace_fingerprint"`
	ReportedAt           time.Time   `json:"reported_at"`
}

func (r CheckReport) IsZero() bool { return r == (CheckReport{}) }

func (r CheckReport) validStatus() bool {
	return r.Status == CheckStatusPassed || r.Status == CheckStatusFailed
}

// stale is derived from cached local review evidence. It performs no I/O, so
// list/info/TUI polling remains cheap. Completion and explicit review/report
// operations are the only paths that refresh the workspace fingerprint.
func (r CheckReport) stale(facts ReviewFacts) bool {
	return !r.IsZero() && (r.Source != CheckSourceReported || !r.validStatus() ||
		facts.Status != ReviewFactsAvailable ||
		facts.WorkspaceFingerprint == "" ||
		r.WorkspaceFingerprint != facts.WorkspaceFingerprint)
}

func (r CheckReport) current(facts ReviewFacts) bool {
	return !r.IsZero() && r.validStatus() && !r.stale(facts)
}

// CheckReportInfo is the wire projection. Stale is derived rather than
// persisted, preventing a list poll from claiming currency after newer review
// evidence has arrived.
type CheckReportInfo struct {
	Source               string      `json:"source"`
	Status               CheckStatus `json:"status"`
	WorkspaceFingerprint string      `json:"workspace_fingerprint"`
	ReportedAt           time.Time   `json:"reported_at"`
	Stale                bool        `json:"stale"`
}

func (r CheckReportInfo) IsZero() bool { return r == (CheckReportInfo{}) }

func (r CheckReport) toInfo(facts ReviewFacts) CheckReportInfo {
	if r.IsZero() {
		return CheckReportInfo{}
	}
	return CheckReportInfo{
		Source:               r.Source,
		Status:               r.Status,
		WorkspaceFingerprint: r.WorkspaceFingerprint,
		ReportedAt:           r.ReportedAt,
		Stale:                r.stale(facts),
	}
}

// mergeCheckReport keeps the latest explicit report when stale Session
// snapshots race at Store.Save.
func mergeCheckReport(a, b CheckReport) CheckReport {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if a.ReportedAt.After(b.ReportedAt) {
		return a
	}
	return b
}

// reconcileCheckAttention makes a current failed report the highest local
// handoff state. Once that report is replaced, or newer workspace evidence
// makes it stale, it stops blocking review readiness.
func reconcileCheckAttention(attention Attention, facts ReviewFacts, report CheckReport) Attention {
	if attention.Generation == 0 {
		return attention
	}
	if report.current(facts) && report.Status == CheckStatusFailed {
		attention.State = AttentionChecksFailed
		return attention
	}
	if attention.State == AttentionChecksFailed {
		if facts.Status == ReviewFactsAvailable &&
			facts.AttentionGeneration == attention.Generation && facts.ChangedFiles > 0 {
			attention.State = AttentionReadyForReview
		} else {
			attention.State = AttentionDone
		}
	}
	return attention
}
