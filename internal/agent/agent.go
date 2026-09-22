// Package agent hosts the Agent interface and the process-global registry
// that maps a kind string (e.g. "claude") to a concrete adapter.
//
// Adapters are registered via a blank-import package (internal/agent/register)
// so this file never has to depend on any specific adapter package — the
// dependency graph is:
//
//	internal/session  ──►  (owns Agent interface and supporting types)
//	                       ▲
//	                       │  type-alias re-export
//	                       │
//	internal/agent    ◄──  internal/agent/register  ──►  internal/agent/<kind>/
//
// The Manager only ever talks to a session.AgentResolver, so it stays free
// of any import into this package.
package agent

import "github.com/takaaki-s/jind-ai/internal/session"

// The type aliases below re-export the interface + supporting types the
// session package owns. Adapter implementations use these local names so
// their signatures read as "agent.SpawnPlan" etc., without pulling every
// call site to also import internal/session.

// Agent is the interface satisfied by every adapter.
type Agent = session.Agent

// CapabilityProvider is the optional, versioned support declaration an
// adapter may expose. Adapters without it safely negotiate to unknown.
type CapabilityProvider = session.CapabilityProvider

// ExecutableDetector is the optional process-name predicate used only for
// adopted-pane classification. Capabilities remain a separate contract.
type ExecutableDetector = session.AgentExecutableDetector

// AgentCapabilities is the adapter capability set projected in session Info.
type AgentCapabilities = session.AgentCapabilities

// AgentCapability names one entry in AgentCapabilities.
type AgentCapability = session.AgentCapability

// CapabilityState distinguishes confirmed support, confirmed lack of support,
// and an unknown declaration.
type CapabilityState = session.CapabilityState

const AgentCapabilitiesSchemaVersion = session.AgentCapabilitiesSchemaVersion

const (
	CapabilityUnknown     = session.CapabilityUnknown
	CapabilitySupported   = session.CapabilitySupported
	CapabilityUnsupported = session.CapabilityUnsupported

	CapabilityLiveness            = session.CapabilityLiveness
	CapabilitySend                = session.CapabilitySend
	CapabilityRespond             = session.CapabilityRespond
	CapabilityResume              = session.CapabilityResume
	CapabilityHooks               = session.CapabilityHooks
	CapabilityTranscript          = session.CapabilityTranscript
	CapabilityReliableNeedsAnswer = session.CapabilityReliableNeedsAnswer
)

// SpawnOptions is the input to Agent.SpawnCommand.
type SpawnOptions = session.SpawnOptions

// SpawnPlan is the output of Agent.SpawnCommand.
type SpawnPlan = session.SpawnPlan

// StatusSignal is a raw event handed to Agent.StatusSource().Interpret.
type StatusSignal = session.StatusSignal

// StatusUpdate is the adapter's verdict on a StatusSignal.
type StatusUpdate = session.StatusUpdate

// SetupContext carries the paths Agent.Setup needs.
type SetupContext = session.SetupContext

// StatusSource interprets raw StatusSignals.
type StatusSource = session.StatusSource

// DescriptionSource is the Layer C description enhancer surface.
type DescriptionSource = session.DescriptionEnhancer

// TranscriptSource reads an agent's own conversation log; the return type of
// Agent.Transcript.
type TranscriptSource = session.TranscriptSource

// PollableTranscriptSource is the opt-in a reader makes when it is cheap enough
// to call on a timer. Re-exported because adapters are written against this
// package, and opting in is invisible when forgotten — the preview simply never
// appears — so a reader that means to opt in wants
// `var _ agent.PollableTranscriptSource = (*Reader)(nil)` to say so at compile
// time.
type PollableTranscriptSource = session.PollableTranscriptSource

// BlockKind categorises the blocking prompt an agent's TUI is showing; the
// return type of Agent.DetectBlock.
type BlockKind = session.BlockKind

// BlockAnswer is the answer handed to Agent.AnswerBlockKeys.
type BlockAnswer = session.BlockAnswer

// KeyStep is one step of the sequence Agent.AnswerBlockKeys returns.
type KeyStep = session.KeyStep

// Blocking-prompt kind constants — re-exported for adapter convenience.
const (
	BlockNone           = session.BlockNone
	BlockPermission     = session.BlockPermission
	BlockQuestion       = session.BlockQuestion
	BlockQuestionMulti  = session.BlockQuestionMulti
	BlockQuestionSubmit = session.BlockQuestionSubmit
)

// NotifyKind categorises the notification signal an adapter attaches to a
// StatusUpdate; downstream plugins receive it via JIN_NOTIFY_KIND.
type NotifyKind = session.NotifyKind

// Notification kind constants — re-exported for adapter convenience.
const (
	NotifyNone         = session.NotifyNone
	NotifyTaskComplete = session.NotifyTaskComplete
	NotifyError        = session.NotifyError
	NotifyPermission   = session.NotifyPermission
)
