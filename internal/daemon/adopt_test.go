package daemon

import (
	"encoding/json"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/tmux"
)

func TestClientAdoptCarriesExactTwoPhaseRequest(t *testing.T) {
	payload, _ := json.Marshal(AdoptResponse{Preview: session.AdoptionPreview{ConfirmationKey: "adopt-v1-key"}})
	sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: payload})
	c := NewClient(sock)
	server := tmux.ServerRef{Kind: tmux.ServerPath, Value: "/tmp/tmux/default"}
	if _, err := c.Adopt(AdoptOptions{
		Server: server, Target: "%19", AgentKind: "claude", Description: "existing",
		Fleet: "work", ConfirmationKey: "adopt-v1-key", Confirm: true,
	}); err != nil {
		t.Fatal(err)
	}
	if received.Action != "adopt" {
		t.Fatalf("action = %q", received.Action)
	}
	var req AdoptRequest
	if err := json.Unmarshal(received.Data, &req); err != nil {
		t.Fatal(err)
	}
	if req.Server != server || req.Target != "%19" || req.AgentKind != "claude" || !req.Confirm || req.DryRun || req.ConfirmationKey != "adopt-v1-key" {
		t.Fatalf("request = %+v", req)
	}
}

func TestHandleAdoptRejectsMissingPhaseBeforeManagerAccess(t *testing.T) {
	s := &Server{}
	for _, req := range []AdoptRequest{{}, {DryRun: true, Confirm: true}} {
		data, _ := json.Marshal(req)
		if resp := s.handleAdopt(data); resp.Success {
			t.Fatalf("handleAdopt(%+v) succeeded", req)
		}
	}
}
