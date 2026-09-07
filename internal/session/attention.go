package session

// AttentionState is the completion axis. It is deliberately independent of
// Status: Status says what the agent's process is doing right now, attention
// says whether a turn that finished is still unacknowledged. Neither derives
// from the other — a session that finished and went idle keeps its receipt
// after the operator starts a new turn, and a session that is running again
// can still be carrying one from before.
type AttentionState string

const (
	// AttentionNone is the zero value, and it is the empty string so that a
	// session file written before attention existed decodes to it. That is
	// what keeps attention out of migrateSessionJSON.
	AttentionNone AttentionState = ""
	// AttentionDone records that a task-completion verdict was applied.
	AttentionDone AttentionState = "done"
	// AttentionReadyForReview records that the completed generation has a
	// non-empty, locally inspected delta from its immutable review base.
	AttentionReadyForReview AttentionState = "ready-for-review"
)

// Attention is the persisted completion receipt. Generation counts applied
// completions; SeenGeneration is the one the operator explicitly acknowledged.
// "Unseen" is the gap between them and is never stored — see AttentionInfo.
//
// Every field is monotonic for the life of a session, which is what makes
// mergeAttention a safe repair for a stale snapshot.
type Attention struct {
	State          AttentionState `json:"state,omitempty"`
	Generation     uint64         `json:"generation,omitempty"`
	SeenGeneration uint64         `json:"seen_generation,omitempty"`
}

// AttentionInfo is the wire projection. Unlike the persisted form it spells
// out all four fields once the object exists, so a script can read
// `.attention.unseen` without testing for the key first. The object itself is
// still omitted at zero (Info's `omitzero`), so a missing object means
// none/seen.
type AttentionInfo struct {
	State          AttentionState `json:"state"`
	Generation     uint64         `json:"generation"`
	SeenGeneration uint64         `json:"seen_generation"`
	Unseen         bool           `json:"unseen"`
}

// Unseen reports a completion the operator has not acknowledged.
func (a Attention) Unseen() bool {
	return (a.State == AttentionDone || a.State == AttentionReadyForReview) &&
		a.Generation > a.SeenGeneration
}

func (a Attention) completed() Attention {
	a.State = AttentionDone
	a.Generation++
	return a
}

func (a Attention) readyForReview(generation uint64) Attention {
	if a.Generation == generation && a.State == AttentionDone {
		a.State = AttentionReadyForReview
	}
	return a
}

// acknowledged raises SeenGeneration to Generation. State and Generation are
// preserved, so the next completion is unseen again.
func (a Attention) acknowledged() Attention {
	a.SeenGeneration = a.Generation
	return a
}

func (a Attention) toInfo() AttentionInfo {
	return AttentionInfo{
		State:          a.State,
		Generation:     a.Generation,
		SeenGeneration: a.SeenGeneration,
		Unseen:         a.Unseen(),
	}
}

// mergeAttention combines two observations of one session's attention,
// keeping the larger of each counter. Why Store.Save needs it is in
// docs/gotchas.md, under "Session persistence".
//
// Deliberately an unkeyed literal: a fourth field must not compile until this
// function says what happens to it, or every Save would silently zero it.
func mergeAttention(a, b Attention) Attention {
	newer, older := a, b
	if b.Generation > a.Generation {
		newer, older = b, a
	}
	state := newer.State
	if a.Generation == b.Generation {
		// Assessment is the only transition within one generation. Once either
		// snapshot observed ready-for-review, a stale done snapshot cannot undo
		// it. A later completion has a larger generation and takes the branch
		// above, resetting the state to done as intended.
		if a.State == AttentionReadyForReview || b.State == AttentionReadyForReview {
			state = AttentionReadyForReview
		} else if a.State == AttentionDone || b.State == AttentionDone {
			state = AttentionDone
		}
	} else if state == AttentionNone {
		// A malformed/newer snapshot with a generation but no state must not
		// erase the last meaningful state. Validation normally prevents this;
		// keeping it here makes persistence recovery conservative.
		state = older.State
	}
	return Attention{
		state,
		newer.Generation,
		max(a.SeenGeneration, b.SeenGeneration),
	}
}
