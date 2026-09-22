package session

import "github.com/takaaki-s/jind-ai/internal/tmux"

type TmuxOwnership string

const TmuxOwnershipAdopted TmuxOwnership = "adopted"

// TmuxBinding records enough immutable identity to distinguish a moved or
// reused foreign pane from the process the user explicitly previewed.
type TmuxBinding struct {
	Ownership   TmuxOwnership  `json:"ownership,omitempty"`
	Server      tmux.ServerRef `json:"server,omitzero"`
	SessionID   string         `json:"session_id,omitempty"`
	SessionName string         `json:"session_name,omitempty"`
	WindowID    string         `json:"window_id,omitempty"`
	WindowName  string         `json:"window_name,omitempty"`
	WindowIndex int            `json:"window_index,omitempty"`
	PaneID      string         `json:"pane_id,omitempty"`
	PaneIndex   int            `json:"pane_index,omitempty"`
	PanePID     int            `json:"pane_pid,omitempty"`
	PaneStarted string         `json:"pane_started,omitempty"`
	IdentityKey string         `json:"identity_key,omitempty"`
}

func (s *Session) IsAdopted() bool {
	return s != nil && s.TmuxBinding.Ownership == TmuxOwnershipAdopted
}

func (b TmuxBinding) matches(p tmux.PaneInfo) bool {
	return b.Ownership == TmuxOwnershipAdopted &&
		b.SessionID == p.SessionID && b.WindowID == p.WindowID && b.PaneID == p.PaneID &&
		b.PanePID == p.PanePID && b.PaneStarted == p.PaneStarted
}

func adoptedCapabilities(_ AgentCapabilities) AgentCapabilities {
	return AgentCapabilities{
		SchemaVersion:       AgentCapabilitiesSchemaVersion,
		Liveness:            CapabilitySupported,
		Send:                CapabilityUnsupported,
		Respond:             CapabilityUnsupported,
		Resume:              CapabilityUnsupported,
		Hooks:               CapabilityUnsupported,
		Transcript:          CapabilityUnsupported,
		ReliableNeedsAnswer: CapabilityUnsupported,
	}
}

func capabilitiesForSession(resolver AgentResolver, sess *Session) AgentCapabilities {
	base := capabilitiesForKind(resolver, sess.AgentKind)
	if sess.IsAdopted() {
		return adoptedCapabilities(base)
	}
	return base
}
