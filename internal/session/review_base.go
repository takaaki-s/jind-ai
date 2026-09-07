package session

// ReviewBaseUnavailableNotManagedWorktree is recorded when jind-ai did not
// create the session's worktree. An existing worktree may have useful git
// history, but its creation point is not evidence jind-ai observed, so it must
// not be guessed after the fact.
const ReviewBaseUnavailableNotManagedWorktree = "not_managed_worktree"

// ReviewBase is the immutable starting point for reviewing one session's
// managed worktree. RequestedRef records the fully-qualified ref jind-ai
// resolved at creation time; CommitOID is the full object ID returned by git;
// WorktreePath is the checkout jind-ai created. The path is evidence too:
// Session.WorkDir may later follow an agent into a different repository.
//
// A non-empty UnavailableReason is an explicit negative result for a newly
// created session. The zero value is reserved for legacy records whose review
// base was never observed, so loading old JSON does not synthesize evidence.
type ReviewBase struct {
	RequestedRef      string `json:"requested_ref,omitempty"`
	CommitOID         string `json:"commit_oid,omitempty"`
	WorktreePath      string `json:"worktree_path,omitempty"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

// IsZero lets encoding/json's omitzero keep legacy records free of a synthetic
// review_base member when they are saved for an unrelated reason.
func (b ReviewBase) IsZero() bool {
	return b == ReviewBase{}
}

// mergeReviewBase preserves the first persisted observation. ReviewBase has
// no state transitions: once a commit or an unavailable reason reaches disk,
// a stale Session snapshot must not replace or clear it.
func mergeReviewBase(candidate, persisted ReviewBase) ReviewBase {
	if !persisted.IsZero() {
		return persisted
	}
	return candidate
}
