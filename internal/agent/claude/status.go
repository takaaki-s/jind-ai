package claude

import (
	"github.com/takaaki-s/jindaiko/internal/agent"
	"github.com/takaaki-s/jindaiko/internal/session"
)

// HookStatusSource translates Claude Code hook payloads into StatusUpdates.
//
// Manager builds StatusSignal{Kind:"hook", Payload:{event, notification_type,
// stop_reason, cwd}} from the on-wire HookRequest and hands it here; the
// signal's Payload keys mirror the fields Claude Code's stdin JSON carries.
//
// A returned bool=false means "this event is meaningful but does not warrant
// a status change" — Manager still runs the agent-agnostic side effects
// (CWD tracking, AgentSessionStarted bookkeeping, description upgrade).
type HookStatusSource struct{}

// NewHookStatusSource constructs the interpreter. Stateless, but held as a
// pointer so future adapters can add memoisation without changing callers.
func NewHookStatusSource() *HookStatusSource { return &HookStatusSource{} }

// Interpret implements session.StatusSource.
//
// The ClearError flag on each StatusUpdate follows the pre-refactor
// invariant: hooks that mean "the agent recovered / took a new turn" clear
// the previous StopFailure message, while hooks that only report presence
// (SessionEnd / Notification) leave whatever the field held.
func (h *HookStatusSource) Interpret(sig agent.StatusSignal) (agent.StatusUpdate, bool) {
	if sig.Kind != "hook" {
		return agent.StatusUpdate{}, false
	}
	event := sig.Payload["event"]
	switch event {
	case "UserPromptSubmit", "PreToolUse", "PostToolUse":
		return agent.StatusUpdate{Status: session.StatusThinking, ClearError: true, Notify: agent.NotifyNone}, true
	case "Stop":
		return agent.StatusUpdate{Status: session.StatusIdle, ClearError: true, Notify: agent.NotifyTaskComplete}, true
	case "StopFailure":
		return agent.StatusUpdate{
			Status:       session.StatusIdle,
			ErrorMessage: sig.Payload["stop_reason"],
			Notify:       agent.NotifyError,
		}, true
	case "SessionEnd":
		// Historically SessionEnd did not touch ErrorMessage — a session
		// that ended with a StopFailure message should still surface it
		// after the process is gone.
		return agent.StatusUpdate{Status: session.StatusStopped, Notify: agent.NotifyNone}, true
	case "Notification":
		switch sig.Payload["notification_type"] {
		case "permission_prompt", "elicitation_dialog":
			return agent.StatusUpdate{Status: session.StatusPermission, Notify: agent.NotifyPermission}, true
		case "idle_prompt":
			return agent.StatusUpdate{Status: session.StatusIdle, Notify: agent.NotifyNone}, true
		}
	}
	// SessionStart / CwdChanged / unknown events — no status change, but
	// Manager still uses them (SessionStart marks AgentSessionStarted, CwdChanged
	// triggers git branch reprobing). Returning false lets Manager fall through
	// to those agent-agnostic side effects without a spurious status write.
	return agent.StatusUpdate{}, false
}
