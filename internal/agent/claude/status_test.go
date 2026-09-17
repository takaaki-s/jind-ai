package claude

import (
	"testing"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/transcript"
)

func TestInterpret_HookEventMap(t *testing.T) {
	cases := []struct {
		name            string
		event           string
		notificationTyp string
		stopReason      string
		wantOK          bool
		wantStatus      session.Status
		wantNotify      agent.NotifyKind
		wantErrMsg      string
		// wantLiveness pins which events may open a turn. Manager refuses to
		// let a liveness verdict take a session out of idle, so a row that
		// gains or loses the flag changes what a late hook does to a finished
		// session — see Manager.HandleHookEvent.
		wantLiveness bool
		// wantClearError pins the invariant this file's doc comment states:
		// the hooks that mean the agent recovered or took a new turn clear the
		// previous StopFailure message, and the ones that only report presence
		// leave it. Splitting the thinking arm in two duplicated that decision,
		// so the column exists to keep the halves from drifting apart.
		wantClearError  bool
		wantNeedsAnswer session.NeedsAnswerSignal
	}{
		{name: "UserPromptSubmit → thinking", event: "UserPromptSubmit", wantOK: true, wantStatus: session.StatusThinking, wantNotify: agent.NotifyNone, wantClearError: true, wantNeedsAnswer: session.NeedsAnswerSignalResolved},
		{name: "PreToolUse → thinking, liveness", event: "PreToolUse", wantOK: true, wantStatus: session.StatusThinking, wantNotify: agent.NotifyNone, wantLiveness: true, wantClearError: true},
		{name: "PostToolUse → thinking, liveness", event: "PostToolUse", wantOK: true, wantStatus: session.StatusThinking, wantNotify: agent.NotifyNone, wantLiveness: true, wantClearError: true},
		{name: "Stop → idle + task-complete", event: "Stop", wantOK: true, wantStatus: session.StatusIdle, wantNotify: agent.NotifyTaskComplete, wantClearError: true, wantNeedsAnswer: session.NeedsAnswerSignalResolved},
		{name: "StopFailure carries reason", event: "StopFailure", stopReason: "rate_limit", wantOK: true, wantStatus: session.StatusIdle, wantNotify: agent.NotifyError, wantErrMsg: "rate_limit", wantNeedsAnswer: session.NeedsAnswerSignalResolved},
		{name: "SessionEnd → stopped", event: "SessionEnd", wantOK: true, wantStatus: session.StatusStopped, wantNotify: agent.NotifyNone, wantNeedsAnswer: session.NeedsAnswerSignalResolved},
		{name: "Notification permission_prompt → permission", event: "Notification", notificationTyp: "permission_prompt", wantOK: true, wantStatus: session.StatusPermission, wantNotify: agent.NotifyPermission, wantNeedsAnswer: session.NeedsAnswerSignalRequired},
		{name: "Notification elicitation_dialog → permission", event: "Notification", notificationTyp: "elicitation_dialog", wantOK: true, wantStatus: session.StatusPermission, wantNotify: agent.NotifyPermission, wantNeedsAnswer: session.NeedsAnswerSignalRequired},
		{name: "Notification idle_prompt → idle", event: "Notification", notificationTyp: "idle_prompt", wantOK: true, wantStatus: session.StatusIdle, wantNotify: agent.NotifyNone, wantNeedsAnswer: session.NeedsAnswerSignalResolved},
		{name: "Notification unknown type → false", event: "Notification", notificationTyp: "something-else", wantOK: false},
		{name: "SessionStart → no status change", event: "SessionStart", wantOK: false},
		{name: "CwdChanged → no status change", event: "CwdChanged", wantOK: false},
		{name: "unknown event → false", event: "MysteryEvent", wantOK: false},
	}
	src := NewHookStatusSource()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upd, ok := src.Interpret(agent.StatusSignal{
				Kind: "hook",
				Payload: map[string]string{
					"event":             tc.event,
					"notification_type": tc.notificationTyp,
					"stop_reason":       tc.stopReason,
				},
			})
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (upd=%+v)", ok, tc.wantOK, upd)
			}
			if !ok {
				return
			}
			if upd.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", upd.Status, tc.wantStatus)
			}
			if upd.Notify != tc.wantNotify {
				t.Errorf("Notify = %q, want %q", upd.Notify, tc.wantNotify)
			}
			if upd.ErrorMessage != tc.wantErrMsg {
				t.Errorf("ErrorMessage = %q, want %q", upd.ErrorMessage, tc.wantErrMsg)
			}
			if upd.Liveness != tc.wantLiveness {
				t.Errorf("Liveness = %v, want %v — the flag decides whether this event "+
					"may take a session out of idle", upd.Liveness, tc.wantLiveness)
			}
			if upd.ClearError != tc.wantClearError {
				t.Errorf("ClearError = %v, want %v", upd.ClearError, tc.wantClearError)
			}
			if upd.NeedsAnswer != tc.wantNeedsAnswer {
				t.Errorf("NeedsAnswer = %v, want %v", upd.NeedsAnswer, tc.wantNeedsAnswer)
			}
		})
	}
}

// stubTurnStater returns a canned TurnState and records whether it was called,
// so tests can assert the recover branch skips the transcript read when the
// agent_session_id is empty.
type stubTurnStater struct {
	state  transcript.TurnState
	called bool
}

func (s *stubTurnStater) TurnState(workDir, sessionID string) transcript.TurnState {
	s.called = true
	return s.state
}

func TestInterpret_RecoverTurnStateMap(t *testing.T) {
	cases := []struct {
		name       string
		state      transcript.TurnState
		persisted  session.Status
		wantOK     bool
		wantStatus session.Status
	}{
		{name: "Complete → idle", state: transcript.TurnStateComplete, persisted: session.StatusThinking, wantOK: true, wantStatus: session.StatusIdle},
		{name: "PendingTool + persisted permission → permission", state: transcript.TurnStatePendingTool, persisted: session.StatusPermission, wantOK: true, wantStatus: session.StatusPermission},
		{name: "PendingTool + persisted thinking → thinking", state: transcript.TurnStatePendingTool, persisted: session.StatusThinking, wantOK: true, wantStatus: session.StatusThinking},
		{name: "PendingTool + persisted running → thinking", state: transcript.TurnStatePendingTool, persisted: session.StatusRunning, wantOK: true, wantStatus: session.StatusThinking},
		{name: "UserPending → thinking", state: transcript.TurnStateUserPending, persisted: session.StatusRunning, wantOK: true, wantStatus: session.StatusThinking},
		{name: "Unknown → false", state: transcript.TurnStateUnknown, persisted: session.StatusRunning, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubTurnStater{state: tc.state}
			src := &HookStatusSource{turns: stub}
			upd, ok := src.Interpret(agent.StatusSignal{
				Kind: "recover",
				Payload: map[string]string{
					"agent_session_id": "sess-1",
					"workdir":          "/work",
					"persisted_status": string(tc.persisted),
				},
			})
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (upd=%+v)", ok, tc.wantOK, upd)
			}
			if !ok {
				return
			}
			if upd.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", upd.Status, tc.wantStatus)
			}
			if upd.Notify != agent.NotifyNone {
				t.Errorf("Notify = %q, want NotifyNone (recover transitions are stale-derived)", upd.Notify)
			}
			if upd.ErrorMessage != "" || upd.ClearError {
				t.Errorf("recover must not touch error fields: ErrorMessage=%q ClearError=%v", upd.ErrorMessage, upd.ClearError)
			}
			// The recover branch re-derives where a session already stands
			// rather than reporting that something just happened, and Manager
			// applies it verbatim on a path that never consults Liveness. A
			// verdict that set the flag would be asking the hook path's
			// question in a place that does not ask it — and would quietly
			// change what happens if that ever stops being true.
			if upd.Liveness {
				t.Error("recover verdicts must leave Liveness unset")
			}
		})
	}
}

func TestInterpret_RecoverEmptySessionIDSkipsTranscript(t *testing.T) {
	stub := &stubTurnStater{state: transcript.TurnStateComplete}
	src := &HookStatusSource{turns: stub}
	upd, ok := src.Interpret(agent.StatusSignal{
		Kind: "recover",
		Payload: map[string]string{
			"agent_session_id": "",
			"persisted_status": string(session.StatusThinking),
		},
	})
	if ok {
		t.Errorf("expected ok=false for empty agent_session_id, got upd=%+v", upd)
	}
	if stub.called {
		t.Error("TurnState must not be consulted when agent_session_id is empty")
	}
}

func TestInterpret_NonHookKindIsIgnored(t *testing.T) {
	src := NewHookStatusSource()
	upd, ok := src.Interpret(agent.StatusSignal{
		Kind:    "pane-output",
		Payload: map[string]string{"event": "Stop"},
	})
	if ok {
		t.Errorf("expected ok=false for non-hook kind, got upd=%+v", upd)
	}
}
