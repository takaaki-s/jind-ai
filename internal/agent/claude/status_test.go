package claude

import (
	"testing"

	"github.com/takaaki-s/jindaiko/internal/agent"
	"github.com/takaaki-s/jindaiko/internal/session"
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
	}{
		{name: "UserPromptSubmit → thinking", event: "UserPromptSubmit", wantOK: true, wantStatus: session.StatusThinking, wantNotify: agent.NotifyNone},
		{name: "PreToolUse → thinking", event: "PreToolUse", wantOK: true, wantStatus: session.StatusThinking, wantNotify: agent.NotifyNone},
		{name: "PostToolUse → thinking", event: "PostToolUse", wantOK: true, wantStatus: session.StatusThinking, wantNotify: agent.NotifyNone},
		{name: "Stop → idle + task-complete", event: "Stop", wantOK: true, wantStatus: session.StatusIdle, wantNotify: agent.NotifyTaskComplete},
		{name: "StopFailure carries reason", event: "StopFailure", stopReason: "rate_limit", wantOK: true, wantStatus: session.StatusIdle, wantNotify: agent.NotifyError, wantErrMsg: "rate_limit"},
		{name: "SessionEnd → stopped", event: "SessionEnd", wantOK: true, wantStatus: session.StatusStopped, wantNotify: agent.NotifyNone},
		{name: "Notification permission_prompt → permission", event: "Notification", notificationTyp: "permission_prompt", wantOK: true, wantStatus: session.StatusPermission, wantNotify: agent.NotifyPermission},
		{name: "Notification elicitation_dialog → permission", event: "Notification", notificationTyp: "elicitation_dialog", wantOK: true, wantStatus: session.StatusPermission, wantNotify: agent.NotifyPermission},
		{name: "Notification idle_prompt → idle", event: "Notification", notificationTyp: "idle_prompt", wantOK: true, wantStatus: session.StatusIdle, wantNotify: agent.NotifyNone},
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
		})
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
