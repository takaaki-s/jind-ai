package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestClientPRHandoff_SendsExplicitModeAndDecodesResult(t *testing.T) {
	want := PRHandoffResponse{
		Payload: session.PRHandoffRequest{SchemaVersion: 1, Kind: "pull-request", IdempotencyKey: "stable-key"},
		Target:  session.PRHandoffTarget{Plugin: "provider", Action: "create"},
		Handoff: session.PRHandoffInfo{PRHandoff: session.PRHandoff{Status: session.PRHandoffSucceeded}},
	}
	data, _ := json.Marshal(want)
	sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: data})
	req := PRHandoffRequest{ID: "abcd", Plugin: "provider", Action: "create", Confirm: true, IdempotencyKey: "stable-key"}
	got, err := NewClient(sock).PRHandoff(req)
	if err != nil {
		t.Fatal(err)
	}
	if received.Action != "pr-handoff" {
		t.Fatalf("action = %q", received.Action)
	}
	var sent PRHandoffRequest
	if err := json.Unmarshal(received.Data, &sent); err != nil {
		t.Fatal(err)
	}
	if sent != req || got.Target != want.Target || got.Handoff.Status != session.PRHandoffSucceeded {
		t.Fatalf("sent=%+v got=%+v", sent, got)
	}
}

func TestHandlePRHandoff_RequiresExplicitModeAndIsDispatched(t *testing.T) {
	s := newTestServer(t)
	data, _ := json.Marshal(PRHandoffRequest{ID: "missing", Plugin: "provider"})
	resp := s.handleRequest(&Request{Action: "pr-handoff", Data: data})
	if resp.Success || !strings.Contains(resp.Error, "exactly one") {
		t.Fatalf("response = %+v", resp)
	}
}

func TestPRHandoffIsNotReadOnly(t *testing.T) {
	if readOnlyActions["pr-handoff"] {
		t.Fatal("pr-handoff may invoke an external mutation and must keep unknown-outcome timeout wording")
	}
}
