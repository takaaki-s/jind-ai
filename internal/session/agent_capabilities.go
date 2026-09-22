package session

import "encoding/json"

// AgentCapabilitiesSchemaVersion is the capability vocabulary understood by
// this build. Capability declarations describe adapter support, not the
// current health of an agent process or one particular hook installation.
// Session projections may narrow adapter support when jin did not establish
// the prerequisite (notably externally adopted panes).
const AgentCapabilitiesSchemaVersion = 1

// CapabilityState is an adapter's declaration for one capability. Its zero
// value is deliberately unknown: an old adapter, an old peer whose JSON has no
// capabilities field, and an unrecognised future value all fail closed.
type CapabilityState string

const (
	CapabilityUnknown     CapabilityState = ""
	CapabilitySupported   CapabilityState = "supported"
	CapabilityUnsupported CapabilityState = "unsupported"
)

// MarshalJSON spells the zero value as "unknown" on the wire. New servers
// therefore distinguish unknown from unsupported explicitly even though old
// payloads that omit the field still decode to the same safe zero value.
func (s CapabilityState) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.wireValue())
}

// UnmarshalJSON accepts the current vocabulary and treats everything else as
// unknown. Capability negotiation must remain readable when a newer peer adds
// a state this build does not understand.
func (s *CapabilityState) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	switch CapabilityState(value) {
	case CapabilitySupported, CapabilityUnsupported:
		*s = CapabilityState(value)
	default:
		*s = CapabilityUnknown
	}
	return nil
}

func (s CapabilityState) wireValue() string {
	switch s {
	case CapabilitySupported, CapabilityUnsupported:
		return string(s)
	default:
		return "unknown"
	}
}

func (s CapabilityState) String() string { return s.wireValue() }

// AgentCapability names one stable entry in AgentCapabilities. Consumers use
// State rather than selecting struct fields directly so unknown schema
// versions and future capability names have one fail-closed interpretation.
type AgentCapability string

const (
	CapabilityLiveness            AgentCapability = "liveness"
	CapabilitySend                AgentCapability = "send"
	CapabilityRespond             AgentCapability = "respond"
	CapabilityResume              AgentCapability = "resume"
	CapabilityHooks               AgentCapability = "hooks"
	CapabilityTranscript          AgentCapability = "transcript"
	CapabilityReliableNeedsAnswer AgentCapability = "reliable_needs_answer"
)

// AgentCapabilities is the versioned support contract projected with every
// session Info. SchemaVersion zero means no declaration was available.
type AgentCapabilities struct {
	SchemaVersion       int             `json:"schema_version"`
	Liveness            CapabilityState `json:"liveness"`
	Send                CapabilityState `json:"send"`
	Respond             CapabilityState `json:"respond"`
	Resume              CapabilityState `json:"resume"`
	Hooks               CapabilityState `json:"hooks"`
	Transcript          CapabilityState `json:"transcript"`
	ReliableNeedsAnswer CapabilityState `json:"reliable_needs_answer"`
}

// State returns the declared state when this build understands the schema and
// capability. Every other case is unknown, including invalid direct values.
func (c AgentCapabilities) State(capability AgentCapability) CapabilityState {
	if c.SchemaVersion != AgentCapabilitiesSchemaVersion {
		return CapabilityUnknown
	}

	var state CapabilityState
	switch capability {
	case CapabilityLiveness:
		state = c.Liveness
	case CapabilitySend:
		state = c.Send
	case CapabilityRespond:
		state = c.Respond
	case CapabilityResume:
		state = c.Resume
	case CapabilityHooks:
		state = c.Hooks
	case CapabilityTranscript:
		state = c.Transcript
	case CapabilityReliableNeedsAnswer:
		state = c.ReliableNeedsAnswer
	default:
		return CapabilityUnknown
	}

	if state != CapabilitySupported && state != CapabilityUnsupported {
		return CapabilityUnknown
	}
	return state
}

// Supports is the safe consumer predicate. Unknown is never promoted to true.
func (c AgentCapabilities) Supports(capability AgentCapability) bool {
	return c.State(capability) == CapabilitySupported
}

// CapabilityProvider is an optional extension to Agent so adapters compiled
// against the older interface remain usable. Their declaration is unknown.
type CapabilityProvider interface {
	Capabilities() AgentCapabilities
}

// CapabilitiesOf negotiates the capability set exposed by an adapter. It
// normalises every field through State so malformed or future declarations
// cannot accidentally be treated as authoritative by a consumer.
func CapabilitiesOf(agent Agent) AgentCapabilities {
	provider, ok := agent.(CapabilityProvider)
	if !ok || provider == nil {
		return AgentCapabilities{}
	}
	declared := provider.Capabilities()
	if declared.SchemaVersion != AgentCapabilitiesSchemaVersion {
		return AgentCapabilities{}
	}
	return AgentCapabilities{
		SchemaVersion:       AgentCapabilitiesSchemaVersion,
		Liveness:            declared.State(CapabilityLiveness),
		Send:                declared.State(CapabilitySend),
		Respond:             declared.State(CapabilityRespond),
		Resume:              declared.State(CapabilityResume),
		Hooks:               declared.State(CapabilityHooks),
		Transcript:          declared.State(CapabilityTranscript),
		ReliableNeedsAnswer: declared.State(CapabilityReliableNeedsAnswer),
	}
}
