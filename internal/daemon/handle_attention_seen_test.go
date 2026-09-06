package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/agent/agenttest"
	"github.com/takaaki-s/jind-ai/internal/session"
)

// completedSession reserves a session and runs a turn through it, so its
// receipt is the one the production path leaves rather than a field a test
// assigned. Assigning it directly would also have to be done through the
// aliased pointer Manager.Get returns, which is the one thing the daemon's own
// handlers avoid (see handleGet).
func completedSession(t *testing.T, s *Server) session.Info {
	t.Helper()
	agent.Register(&agenttest.StubAgent{
		KindStr: "hooked",
		InterpretFn: func(sig session.StatusSignal) (session.StatusUpdate, bool) {
			switch sig.Payload["event"] {
			case "UserPromptSubmit":
				return session.StatusUpdate{Status: session.StatusThinking}, true
			case "Stop":
				return session.StatusUpdate{Status: session.StatusIdle, Notify: session.NotifyTaskComplete}, true
			}
			return session.StatusUpdate{}, false
		},
	})

	info := reserveSession(t, s, "hooked")
	s.manager.HandleHookEvent("", info.ID, "UserPromptSubmit", "", "", "")
	s.manager.HandleHookEvent("", info.ID, "Stop", "", "", "")

	got, ok := s.manager.GetInfo(info.ID)
	if !ok {
		t.Fatal("GetInfo returned ok=false")
	}
	if !got.Attention.Unseen {
		t.Fatalf("setup: Attention = %+v, want an unseen completion", got.Attention)
	}
	return got
}

func seenFor(t *testing.T, s *Server, id string) Response {
	t.Helper()
	data, err := json.Marshal(IDRequest{ID: id})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return s.handleAttentionSeen(data)
}

func TestHandleAttentionSeen_RejectsMalformedJSON(t *testing.T) {
	s := newAsyncTestServer(t)

	resp := s.handleAttentionSeen(json.RawMessage("{ not json"))
	if resp.Success {
		t.Fatal("handleAttentionSeen succeeded on malformed JSON")
	}
}

func TestHandleAttentionSeen_RejectsEmptyID(t *testing.T) {
	s := newAsyncTestServer(t)

	resp := seenFor(t, s, "")
	if resp.Success {
		t.Fatal("handleAttentionSeen succeeded on an empty id")
	}
	if !strings.Contains(resp.Error, "id is required") {
		t.Errorf("error = %q, want it to say the id is required", resp.Error)
	}
}

func TestHandleAttentionSeen_ReportsNotFound(t *testing.T) {
	s := newAsyncTestServer(t)

	resp := seenFor(t, s, "no-such-session")
	if resp.Success {
		t.Fatal("handleAttentionSeen succeeded on a missing session")
	}
	if !strings.Contains(resp.Error, "not found") {
		t.Errorf("error = %q, want it to say the session was not found", resp.Error)
	}
}

// The response carries the postcondition, so a caller learns the result
// without a follow-up get.
func TestHandleAttentionSeen_ReturnsThePostcondition(t *testing.T) {
	s := newAsyncTestServer(t)
	info := completedSession(t, s)

	resp := seenFor(t, s, info.ID)
	if !resp.Success {
		t.Fatalf("Success=false: %s", resp.Error)
	}
	var got session.Info
	if err := json.Unmarshal(resp.Data, &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	want := session.AttentionInfo{State: session.AttentionDone, Generation: 1, SeenGeneration: 1}
	if got.Attention != want {
		t.Errorf("Attention = %+v, want %+v", got.Attention, want)
	}
	if got.Status != session.StatusIdle {
		t.Errorf("Status = %q, want %q — acknowledging moves no process status", got.Status, session.StatusIdle)
	}
}

// Retrying is the documented answer to a timeout on this action, so a second
// call must return the same thing rather than fail.
func TestHandleAttentionSeen_IsIdempotent(t *testing.T) {
	s := newAsyncTestServer(t)
	info := completedSession(t, s)

	first := seenFor(t, s, info.ID)
	if !first.Success {
		t.Fatalf("first call Success=false: %s", first.Error)
	}
	second := seenFor(t, s, info.ID)
	if !second.Success {
		t.Fatalf("second call Success=false: %s", second.Error)
	}
	if string(first.Data) != string(second.Data) {
		t.Errorf("second call returned %s, want the same as the first %s", second.Data, first.Data)
	}
}

// A timeout on this action leaves the outcome unknown. Listing it read-only
// would give the client the wrong wording — the reassuring one.
func TestAttentionSeenIsNotReadOnly(t *testing.T) {
	if readOnlyActions["attention-seen"] {
		t.Error("attention-seen is listed read-only, but it writes a session file")
	}
}

// Without this the handler is fully tested and completely unreachable: every
// test above calls handleAttentionSeen directly, and TestActionsAreDispatched
// only walks readOnlyActions, which this action is deliberately absent from.
func TestAttentionSeenIsDispatched(t *testing.T) {
	s := newTestServer(t)

	data, _ := json.Marshal(IDRequest{ID: "no-such-session"})
	resp := s.handleRequest(&Request{Action: "attention-seen", Data: data})
	if strings.Contains(resp.Error, "unknown action") {
		t.Fatalf("attention-seen is not wired into handleRequest: %q", resp.Error)
	}
}
