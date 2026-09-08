package session

import "time"

// ReviewFactsStatus says whether the cached local review inspection completed.
// The zero value means no assessment has ever been requested.
type ReviewFactsStatus string

const (
	ReviewFactsPending     ReviewFactsStatus = "pending"
	ReviewFactsAvailable   ReviewFactsStatus = "available"
	ReviewFactsUnavailable ReviewFactsStatus = "unavailable"
)

const (
	ReviewUnavailableLegacyUnknown = "legacy_unknown"
	ReviewUnavailableProbeFailed   = "probe_failed"
	ReviewUnavailableProbeTimeout  = "probe_timeout"
	ReviewUnavailableOutputLimit   = "output_limit"
	ReviewUnavailableTooManyFiles  = "too_many_files"
	ReviewUnavailableWorktreePath  = "worktree_path_unknown"
)

// ReviewFacts is a bounded summary of the worktree delta at one completed
// attention generation. It deliberately stores no paths or patch contents.
// AttentionGeneration binds the cache to the completion it assessed, while
// ObservedAt orders repeated refreshes of that same generation.
type ReviewFacts struct {
	Status              ReviewFactsStatus `json:"status"`
	UnavailableReason   string            `json:"unavailable_reason,omitempty"`
	AttentionGeneration uint64            `json:"attention_generation"`
	BaseCommit          string            `json:"base_commit,omitempty"`
	HeadCommit          string            `json:"head_commit,omitempty"`
	Branch              string            `json:"branch,omitempty"`
	ChangedFiles        int               `json:"changed_files,omitempty"`
	Additions           int               `json:"additions,omitempty"`
	Deletions           int               `json:"deletions,omitempty"`
	BinaryFiles         int               `json:"binary_files,omitempty"`
	UntrackedFiles      int               `json:"untracked_files,omitempty"`
	CommitCount         int               `json:"commit_count,omitempty"`
	ObservedAt          time.Time         `json:"observed_at,omitzero"`
}

func (f ReviewFacts) IsZero() bool { return f == (ReviewFacts{}) }

func pendingReviewFacts(generation uint64, at time.Time) ReviewFacts {
	return ReviewFacts{
		Status:              ReviewFactsPending,
		AttentionGeneration: generation,
		ObservedAt:          at,
	}
}

// mergeReviewFacts keeps the assessment for the newest completion, then the
// latest observation of that completion. This prevents an older asynchronous
// probe or a stale Session snapshot from rolling the cache back on disk.
func mergeReviewFacts(a, b ReviewFacts) ReviewFacts {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if a.AttentionGeneration != b.AttentionGeneration {
		if a.AttentionGeneration > b.AttentionGeneration {
			return a
		}
		return b
	}
	if a.ObservedAt.After(b.ObservedAt) {
		return a
	}
	return b
}
