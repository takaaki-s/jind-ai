package session

// NeedsAnswerState is the evidence-backed answer-wait axis. It is deliberately
// independent of Status: an adapter may know that a human answer is required
// before its process-status hook catches up, and an unsupported adapter must
// stay unknown even when its raw event happens to be named PermissionRequest.
type NeedsAnswerState string

const (
	NeedsAnswerUnknown   NeedsAnswerState = "unknown"
	NeedsAnswerNotNeeded NeedsAnswerState = "not-needed"
	NeedsAnswerRequired  NeedsAnswerState = "needs-answer"
)

// NeedsAnswer is the persisted evidence cursor. All fields are monotonic so a
// stale Store.Save snapshot can be merged without resurrecting or erasing a
// wait:
//
//   - Generation advances when a new wait begins.
//   - ResolvedGeneration catches up when that wait is explicitly cleared.
//   - SeenGeneration catches up when the operator acknowledges the signal.
//   - Known becomes true after a reliable adapter establishes a baseline.
//
// A wait may be seen and still unresolved. Keeping those cursors separate is
// what prevents opening or attaching to a session from pretending it was
// answered.
type NeedsAnswer struct {
	Known              bool   `json:"known,omitempty"`
	Generation         uint64 `json:"generation,omitempty"`
	ResolvedGeneration uint64 `json:"resolved_generation,omitempty"`
	SeenGeneration     uint64 `json:"seen_generation,omitempty"`
}

// NeedsAnswerInfo is the explicit wire projection. State is always present so
// old/unsupported/unknown evidence cannot be mistaken for not-needed.
type NeedsAnswerInfo struct {
	State              NeedsAnswerState `json:"state"`
	Generation         uint64           `json:"generation"`
	ResolvedGeneration uint64           `json:"resolved_generation"`
	SeenGeneration     uint64           `json:"seen_generation"`
	Unseen             bool             `json:"unseen"`
}

// EvidenceState normalizes a missing or future wire value to unknown. This is
// what keeps an older peer's zero-value Info from displaying as not-needed.
func (n NeedsAnswerInfo) EvidenceState() NeedsAnswerState {
	switch n.State {
	case NeedsAnswerNotNeeded, NeedsAnswerRequired:
		return n.State
	default:
		return NeedsAnswerUnknown
	}
}

// Unresolved reports whether the wire projection carries affirmative evidence
// of a current wait. Unlike Unseen it remains true after acknowledgement.
func (n NeedsAnswerInfo) Unresolved() bool { return n.EvidenceState() == NeedsAnswerRequired }

func (n NeedsAnswer) state() NeedsAnswerState {
	if !n.Known {
		return NeedsAnswerUnknown
	}
	if n.Generation > n.ResolvedGeneration {
		return NeedsAnswerRequired
	}
	return NeedsAnswerNotNeeded
}

// Unresolved reports a reliable affirmative wait that has not been cleared.
func (n NeedsAnswer) Unresolved() bool { return n.state() == NeedsAnswerRequired }

// Unseen reports a current wait that the operator has not acknowledged.
func (n NeedsAnswer) Unseen() bool {
	return n.Unresolved() && n.Generation > n.SeenGeneration
}

func (n NeedsAnswer) required() NeedsAnswer {
	n.Known = true
	if !n.Unresolved() {
		n.Generation++
	}
	return n
}

func (n NeedsAnswer) resolved() NeedsAnswer {
	n.Known = true
	n.ResolvedGeneration = n.Generation
	return n
}

func (n NeedsAnswer) acknowledged() NeedsAnswer {
	n.SeenGeneration = n.Generation
	return n
}

func (n NeedsAnswer) toInfo(capability CapabilityState) NeedsAnswerInfo {
	state := NeedsAnswerUnknown
	unseen := false
	if capability == CapabilitySupported {
		state = n.state()
		unseen = n.Unseen()
	}
	return NeedsAnswerInfo{
		State:              state,
		Generation:         n.Generation,
		ResolvedGeneration: n.ResolvedGeneration,
		SeenGeneration:     n.SeenGeneration,
		Unseen:             unseen,
	}
}

func mergeNeedsAnswer(a, b NeedsAnswer) NeedsAnswer {
	return NeedsAnswer{
		Known:              a.Known || b.Known,
		Generation:         max(a.Generation, b.Generation),
		ResolvedGeneration: max(a.ResolvedGeneration, b.ResolvedGeneration),
		SeenGeneration:     max(a.SeenGeneration, b.SeenGeneration),
	}
}
