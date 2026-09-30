package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestClientRecordReviewDispositionPreservesSubmissionAndReceipt(t *testing.T) {
	submission := session.ReviewDispositionSubmission{Decision: session.ReviewDecisionReviewed, WorkspaceFingerprint: strings.Repeat("a", 64),
		ReviewDispositionMetadata: session.ReviewDispositionMetadata{IdempotencyKey: "run-1", Actor: "reviewer", Note: "inspected"},
	}
	want := session.ReviewDispositionRecordResult{Reused: true, Receipt: session.ReviewDispositionReceipt{
		ReviewDisposition:         session.ReviewDisposition{Source: session.ReviewDispositionSourceReported, Decision: submission.Decision, WorkspaceFingerprint: submission.WorkspaceFingerprint, ReportedAt: time.Unix(30, 0).UTC()},
		ReviewDispositionMetadata: submission.ReviewDispositionMetadata,
	}}
	body, _ := json.Marshal(want)
	sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: body})
	got, err := NewClient(sock).RecordReviewDisposition("session", submission)
	if err != nil {
		t.Fatal(err)
	}
	var req ReviewDispositionRecordRequest
	if err := json.Unmarshal(received.Data, &req); err != nil {
		t.Fatal(err)
	}
	if received.Action != "review-disposition-record" || req.ID != "session" || req.ReviewDispositionSubmission != submission || !got.Reused || got.Receipt != want.Receipt {
		t.Fatalf("request=%+v result=%+v", req, got)
	}
	s := newTestServer(t)
	invalid := s.handleRequest(&Request{Action: "review-disposition-record", Data: json.RawMessage(`{"id":"session"}`)})
	if invalid.Success || strings.Contains(invalid.Error, "unknown action") {
		t.Fatalf("invalid dispatch=%+v", invalid)
	}
}
