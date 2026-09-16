package opencode

import (
	"testing"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/session"
)

func hookSignal(event string) agent.StatusSignal {
	return agent.StatusSignal{Kind: "hook", Payload: map[string]string{"event": event}}
}

func TestInterpret_StatusMapping(t *testing.T) {
	tests := []struct {
		event           string
		wantStatus      session.Status
		wantNotify      agent.NotifyKind
		wantClear       bool
		wantNeedsAnswer session.NeedsAnswerSignal
	}{
		{eventUserPromptSubmit, session.StatusThinking, agent.NotifyNone, true, session.NeedsAnswerSignalResolved},
		{eventPermission, session.StatusPermission, agent.NotifyPermission, false, session.NeedsAnswerSignalRequired},
		{eventStop, session.StatusIdle, agent.NotifyTaskComplete, true, session.NeedsAnswerSignalResolved},
		{eventStopFailure, session.StatusIdle, agent.NotifyError, false, session.NeedsAnswerSignalResolved},
	}

	s := NewEventStatusSource()
	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			upd, ok := s.Interpret(hookSignal(tt.event))
			if !ok {
				t.Fatalf("Interpret(%s) ok = false, want true", tt.event)
			}
			if upd.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", upd.Status, tt.wantStatus)
			}
			if upd.Notify != tt.wantNotify {
				t.Errorf("Notify = %q, want %q", upd.Notify, tt.wantNotify)
			}
			if upd.ClearError != tt.wantClear {
				t.Errorf("ClearError = %v, want %v", upd.ClearError, tt.wantClear)
			}
			if upd.NeedsAnswer != tt.wantNeedsAnswer {
				t.Errorf("NeedsAnswer = %v, want %v", upd.NeedsAnswer, tt.wantNeedsAnswer)
			}
		})
	}
}

// StopFailure is the one mapping that sets a message; the tri-state on
// StatusUpdate means it must not also set ClearError.
func TestInterpret_StopFailureCarriesMessage(t *testing.T) {
	upd, ok := NewEventStatusSource().Interpret(hookSignal(eventStopFailure))
	if !ok {
		t.Fatal("Interpret(StopFailure) ok = false, want true")
	}
	if upd.ErrorMessage == "" {
		t.Error("ErrorMessage is empty, want a message")
	}
	if upd.ClearError {
		t.Error("ClearError = true, want false — it would cancel ErrorMessage")
	}
}

// SessionStart must return false so Manager runs only its agent-agnostic
// bookkeeping: that path is what re-keys AgentSessionID to the real ses_ id.
func TestInterpret_SessionStart_NoStatusChange(t *testing.T) {
	upd, ok := NewEventStatusSource().Interpret(hookSignal(eventSessionStart))
	if ok {
		t.Errorf("Interpret(SessionStart) ok = true, want false (got %+v)", upd)
	}
}

func TestInterpret_UnknownEvent(t *testing.T) {
	if _, ok := NewEventStatusSource().Interpret(hookSignal("session.compacted")); ok {
		t.Error("unknown event returned ok = true, want false")
	}
}

// Non-hook signals — "recover" during daemon restart — are not this
// adapter's business and must be declined rather than misread as an event.
func TestInterpret_NonHookSignal(t *testing.T) {
	sig := agent.StatusSignal{
		Kind:    "recover",
		Payload: map[string]string{"event": eventStop},
	}
	if _, ok := NewEventStatusSource().Interpret(sig); ok {
		t.Error("recover signal returned ok = true, want false")
	}
}

func TestAgent_Kind(t *testing.T) {
	if got := New().Kind(); got != "opencode" {
		t.Errorf("Kind() = %q, want %q", got, "opencode")
	}
}

// Description is nil by design (no Layer C enhancer yet); Manager treats a
// nil enhancer as "this adapter cannot upgrade descriptions".
func TestAgent_DescriptionIsNil(t *testing.T) {
	if d := New().Description(); d != nil {
		t.Errorf("Description() = %v, want nil", d)
	}
}
