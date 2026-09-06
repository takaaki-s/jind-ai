package daemon

import (
	"encoding/json"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

// The client's half of the contract: the action's spelling, the payload shape,
// and that the returned Info is decoded rather than re-fetched.
func TestClientMarkSeen_SendsTheActionAndDecodesTheInfo(t *testing.T) {
	want := session.Info{
		ID:     "abcd1234",
		Status: session.StatusIdle,
		Attention: session.AttentionInfo{
			State:          session.AttentionDone,
			Generation:     3,
			SeenGeneration: 3,
		},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: data})

	got, err := NewClient(sock).MarkSeen("abcd1234")
	if err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	if received.Action != "attention-seen" {
		t.Errorf("Action = %q, want %q", received.Action, "attention-seen")
	}
	var sent IDRequest
	if err := json.Unmarshal(received.Data, &sent); err != nil {
		t.Fatalf("unmarshal sent data: %v", err)
	}
	if sent.ID != "abcd1234" {
		t.Errorf("sent ID = %q, want %q", sent.ID, "abcd1234")
	}
	if got.Attention != want.Attention {
		t.Errorf("Attention = %+v, want %+v", got.Attention, want.Attention)
	}
}

func TestClientMarkSeen_SurfacesTheDaemonError(t *testing.T) {
	sock, _ := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: false, Error: "session not found: x"})

	if _, err := NewClient(sock).MarkSeen("x"); err == nil {
		t.Fatal("MarkSeen returned nil error for an unsuccessful response")
	}
}
