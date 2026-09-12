package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestClientReviewCleanupSendsModeAndDecodesJournal(t *testing.T) {
	want := ReviewCleanupResponse{
		Plan:    session.ReviewCleanupPlan{SessionID: "session-id", IdempotencyKey: "cln_key", Ready: true},
		Journal: session.ReviewCleanupJournal{Status: session.ReviewCleanupSucceeded},
	}
	data, _ := json.Marshal(want)
	sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: data})
	req := ReviewCleanupRequest{ID: "session-id", Confirm: true, IdempotencyKey: "cln_key"}
	got, err := NewClient(sock).ReviewCleanup(req)
	if err != nil {
		t.Fatal(err)
	}
	if received.Action != "review-cleanup" || got.Plan.IdempotencyKey != want.Plan.IdempotencyKey || got.Journal.Status != session.ReviewCleanupSucceeded {
		t.Fatalf("received=%+v got=%+v", received, got)
	}
	var sent ReviewCleanupRequest
	if err := json.Unmarshal(received.Data, &sent); err != nil || sent != req {
		t.Fatalf("sent=%+v err=%v", sent, err)
	}
}

func TestHandleReviewCleanupRequiresExplicitModeAndConfirmationKey(t *testing.T) {
	s := &Server{}
	data, _ := json.Marshal(ReviewCleanupRequest{ID: "session-id"})
	resp := s.handleRequest(&Request{Action: "review-cleanup", Data: data})
	if resp.Success || !strings.Contains(resp.Error, "exactly one") {
		t.Fatalf("mode response = %+v", resp)
	}
	data, _ = json.Marshal(ReviewCleanupRequest{ID: "session-id", Confirm: true})
	resp = s.handleRequest(&Request{Action: "review-cleanup", Data: data})
	if resp.Success || !strings.Contains(resp.Error, "idempotency_key") {
		t.Fatalf("key response = %+v", resp)
	}
}

func TestReviewCleanupIsNotReadOnly(t *testing.T) {
	if readOnlyActions["review-cleanup"] {
		t.Fatal("review-cleanup confirm mutates local assets and must retain unknown-outcome timeout wording")
	}
}
