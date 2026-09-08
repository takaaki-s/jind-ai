package session

import "time"

// ReviewDecision is the human disposition explicitly reported for one exact
// workspace. It is independent of completion attention: looking at a result is
// not approval, and recording a decision does not acknowledge its receipt.
type ReviewDecision string

const (
	ReviewDecisionReviewed         ReviewDecision = "reviewed"
	ReviewDecisionChangesRequested ReviewDecision = "changes-requested"
)

const ReviewDispositionSourceReported = "reported"

// ReviewDisposition binds a human decision to the workspace fingerprint that
// was refreshed immediately before the decision was accepted.
type ReviewDisposition struct {
	Source               string         `json:"source"`
	Decision             ReviewDecision `json:"decision"`
	WorkspaceFingerprint string         `json:"workspace_fingerprint"`
	ReportedAt           time.Time      `json:"reported_at"`
}

func (d ReviewDisposition) IsZero() bool { return d == (ReviewDisposition{}) }

func (d ReviewDisposition) validDecision() bool {
	return d.Decision == ReviewDecisionReviewed || d.Decision == ReviewDecisionChangesRequested
}

func (d ReviewDisposition) stale(facts ReviewFacts) bool {
	return !d.IsZero() && (d.Source != ReviewDispositionSourceReported || !d.validDecision() ||
		facts.Status != ReviewFactsAvailable || facts.WorkspaceFingerprint == "" ||
		facts.ChangedFiles == 0 ||
		d.WorkspaceFingerprint != facts.WorkspaceFingerprint)
}

// ReviewDispositionInfo is the wire projection. Stale is derived from cached
// review facts, keeping list/info/TUI reads free of filesystem and git work.
type ReviewDispositionInfo struct {
	Source               string         `json:"source"`
	Decision             ReviewDecision `json:"decision"`
	WorkspaceFingerprint string         `json:"workspace_fingerprint"`
	ReportedAt           time.Time      `json:"reported_at"`
	Stale                bool           `json:"stale"`
}

func (d ReviewDispositionInfo) IsZero() bool { return d == (ReviewDispositionInfo{}) }

func (d ReviewDisposition) toInfo(facts ReviewFacts) ReviewDispositionInfo {
	if d.IsZero() {
		return ReviewDispositionInfo{}
	}
	return ReviewDispositionInfo{
		Source:               d.Source,
		Decision:             d.Decision,
		WorkspaceFingerprint: d.WorkspaceFingerprint,
		ReportedAt:           d.ReportedAt,
		Stale:                d.stale(facts),
	}
}

// mergeReviewDisposition keeps the latest explicit decision when stale full
// Session snapshots race at Store.Save.
func mergeReviewDisposition(a, b ReviewDisposition) ReviewDisposition {
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
