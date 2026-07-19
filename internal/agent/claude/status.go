package claude

import (
	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/transcript"
)

// turnStater is the slice of transcript.Reader the recover branch needs. The
// real Reader keeps its base dir private, so this seam lets tests inject a stub
// that returns a canned TurnState without touching the filesystem.
type turnStater interface {
	TurnState(workDir, sessionID string) transcript.TurnState
}

// HookStatusSource translates Claude Code hook payloads into StatusUpdates.
//
// Manager builds StatusSignal{Kind:"hook", Payload:{event, notification_type,
// stop_reason, cwd}} from the on-wire HookRequest and hands it here; the
// signal's Payload keys mirror the fields Claude Code's stdin JSON carries.
//
// A returned bool=false means "this event is meaningful but does not warrant
// a status change" — Manager still runs the agent-agnostic side effects
// (CWD tracking, AgentSessionStarted bookkeeping, description upgrade).
type HookStatusSource struct {
	turns turnStater
}

// NewHookStatusSource constructs the interpreter wired to the real transcript
// reader, which the recover branch consults to re-derive stale status.
func NewHookStatusSource() *HookStatusSource {
	return &HookStatusSource{turns: NewTranscriptReader()}
}

// Interpret implements session.StatusSource.
//
// The ClearError flag on each StatusUpdate follows the pre-refactor
// invariant: hooks that mean "the agent recovered / took a new turn" clear
// the previous StopFailure message, while hooks that only report presence
// (SessionEnd / Notification) leave whatever the field held.
func (h *HookStatusSource) Interpret(sig agent.StatusSignal) (agent.StatusUpdate, bool) {
	if sig.Kind == "recover" {
		return h.interpretRecover(sig)
	}
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

// interpretRecover re-derives a session's live status from its transcript when
// Manager recovers pane-alive sessions after a daemon restart. The persisted
// (hook-driven) status may be stale — e.g. a Stop hook missed while the daemon
// was down — so the transcript's last turn is the more trustworthy signal.
//
// Notify is always NotifyNone and ErrorMessage/ClearError are left untouched:
// these transitions correct stale state, not live events, so they must not
// fire notifications or mutate the error field. A false verdict keeps whatever
// status Manager already decided.
func (h *HookStatusSource) interpretRecover(sig agent.StatusSignal) (agent.StatusUpdate, bool) {
	sessionID := sig.Payload["agent_session_id"]
	if sessionID == "" {
		return agent.StatusUpdate{}, false
	}
	switch h.turns.TurnState(sig.Payload["workdir"], sessionID) {
	case transcript.TurnStateComplete:
		return agent.StatusUpdate{Status: session.StatusIdle, Notify: agent.NotifyNone}, true
	case transcript.TurnStatePendingTool:
		// tool execution vs. permission wait are indistinguishable from the
		// transcript alone; the persisted value breaks the tie.
		if sig.Payload["persisted_status"] == string(session.StatusPermission) {
			return agent.StatusUpdate{Status: session.StatusPermission, Notify: agent.NotifyNone}, true
		}
		return agent.StatusUpdate{Status: session.StatusThinking, Notify: agent.NotifyNone}, true
	case transcript.TurnStateUserPending:
		return agent.StatusUpdate{Status: session.StatusThinking, Notify: agent.NotifyNone}, true
	default: // TurnStateUnknown — no transcript / unreadable; keep Manager's decision.
		return agent.StatusUpdate{}, false
	}
}
