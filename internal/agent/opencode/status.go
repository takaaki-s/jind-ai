package opencode

import (
	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/session"
)

// Canonical event names. opencode's own bus vocabulary (session.created,
// session.status, session.idle, permission.asked, ...) does not line up with
// the names HandleHookEvent keys its agent-agnostic side effects on, so the
// bundled plugin translates before calling `jin hook`. The mapping table lives
// in plugin/jin.ts.
const (
	eventSessionStart     = "SessionStart"
	eventUserPromptSubmit = "UserPromptSubmit"
	eventPermission       = "PermissionRequest"
	eventStop             = "Stop"
	eventStopFailure      = "StopFailure"
)

// errorMessage is what StopFailure surfaces on the session. opencode's
// session.error payload carries a structured error whose shape varies by
// provider, and the plugin deliberately does not try to flatten it into a
// message, so the adapter reports a fixed string and leaves the detail to
// the agent's own pane output.
const errorMessage = "opencode reported an error"

// EventStatusSource translates the canonical event names the bundled plugin
// emits into StatusUpdates.
type EventStatusSource struct{}

// NewEventStatusSource constructs the interpreter.
func NewEventStatusSource() *EventStatusSource { return &EventStatusSource{} }

// Interpret implements session.StatusSource.
//
//	SessionStart       (zero, false)          side effects only (AgentSessionStarted, id re-key)
//	UserPromptSubmit   thinking + ClearError  turn started, or permission just answered
//	PermissionRequest  permission             agent is blocked on the user
//	Stop               idle + ClearError      turn finished
//	StopFailure        idle + ErrorMessage    turn ended in an error
func (s *EventStatusSource) Interpret(sig agent.StatusSignal) (agent.StatusUpdate, bool) {
	if sig.Kind != "hook" {
		return agent.StatusUpdate{}, false
	}
	switch sig.Payload["event"] {
	case eventUserPromptSubmit:
		return agent.StatusUpdate{
			Status:      session.StatusThinking,
			ClearError:  true,
			Notify:      agent.NotifyNone,
			NeedsAnswer: session.NeedsAnswerSignalResolved,
		}, true
	case eventPermission:
		return agent.StatusUpdate{
			Status:      session.StatusPermission,
			Notify:      agent.NotifyPermission,
			NeedsAnswer: session.NeedsAnswerSignalRequired,
		}, true
	case eventStop:
		return agent.StatusUpdate{
			Status:      session.StatusIdle,
			ClearError:  true,
			Notify:      agent.NotifyTaskComplete,
			NeedsAnswer: session.NeedsAnswerSignalResolved,
		}, true
	case eventStopFailure:
		return agent.StatusUpdate{
			Status:       session.StatusIdle,
			ErrorMessage: errorMessage,
			Notify:       agent.NotifyError,
			NeedsAnswer:  session.NeedsAnswerSignalResolved,
		}, true
	}
	// SessionStart and anything we don't recognise (opencode emits a large
	// bus vocabulary the plugin filters, but new event names can always
	// appear) leave Status untouched.
	return agent.StatusUpdate{}, false
}
