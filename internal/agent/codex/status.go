package codex

import (
	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/session"
)

// HookStatusSource translates Codex hook payloads into StatusUpdates.
//
// Manager builds StatusSignal{Kind:"hook", Payload:{event, notification_type,
// stop_reason, cwd}} from the on-wire HookRequest. Codex's hook_event_name
// values line up with Claude Code's set for the events jind-ai cares about
// (SessionStart / UserPromptSubmit / Stop), so no cross-vocabulary translation
// happens here.
//
// A returned bool=false means "meaningful but no status change" — Manager still
// runs the agent-agnostic side effects.
type HookStatusSource struct{}

// NewHookStatusSource constructs the interpreter. Stateless, but held as a
// pointer so a future adapter can layer in memoisation or per-session state
// without changing callers.
func NewHookStatusSource() *HookStatusSource { return &HookStatusSource{} }

// Interpret implements session.StatusSource.
//
//	SessionStart       (zero, false)          side effects only (AgentSessionStarted, re-key, Layer C)
//	UserPromptSubmit   thinking + ClearError  canonical progression signal
//	PreToolUse         thinking + ClearError  liveness signal during long turns
//	PostToolUse        thinking + ClearError  liveness signal during long turns
//	PermissionRequest  thinking               an approval path opened
//	Stop               idle + ClearError + task-complete
//
// PermissionRequest is thinking despite its name, and no notification goes
// out. Why, and what that gives up, are under "Codex adapter" in
// docs/gotchas.md.
//
// Codex has no SessionEnd / StopFailure event surface today; StatusStopped is
// driven by the pane-death path in captureOutputTmux.
func (h *HookStatusSource) Interpret(sig agent.StatusSignal) (agent.StatusUpdate, bool) {
	if sig.Kind == "recover" {
		return h.interpretRecover(sig)
	}
	if sig.Kind != "hook" {
		return agent.StatusUpdate{}, false
	}
	switch sig.Payload["event"] {
	case "UserPromptSubmit", "PreToolUse", "PostToolUse":
		return agent.StatusUpdate{
			Status:     session.StatusThinking,
			ClearError: true,
			Notify:     agent.NotifyNone,
		}, true
	case "PermissionRequest":
		// No ClearError: an approval path opening is not a report that a
		// failed turn recovered.
		return agent.StatusUpdate{
			Status: session.StatusThinking,
			Notify: agent.NotifyNone,
		}, true
	case "Stop":
		return agent.StatusUpdate{
			Status:     session.StatusIdle,
			ClearError: true,
			Notify:     agent.NotifyTaskComplete,
		}, true
	}
	// SessionStart / unknown events — no status change, but Manager still
	// uses them (SessionStart marks AgentSessionStarted, re-keys
	// AgentSessionID with the real Codex UUID, and triggers Layer C).
	return agent.StatusUpdate{}, false
}

// interpretRecover normalises a persisted StatusPermission to StatusThinking on
// daemon-restart recovery. Interpret cannot produce that value, so on a codex
// session it is always stale, and applyRecovery's fallback would restore it
// verbatim.
//
// The rollout is not consulted: Codex has no counterpart to
// transcript.TurnState. That leaves this verdict a restatement of the persisted
// value rather than a fresh read, which is why applyRecovery guards it.
func (h *HookStatusSource) interpretRecover(sig agent.StatusSignal) (agent.StatusUpdate, bool) {
	if sig.Payload["persisted_status"] != string(session.StatusPermission) {
		return agent.StatusUpdate{}, false
	}
	return agent.StatusUpdate{Status: session.StatusThinking, Notify: agent.NotifyNone}, true
}
