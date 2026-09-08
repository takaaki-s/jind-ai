package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestHandleReviewDisposition_ValidatesAndDispatches(t *testing.T) {
	s := newTestServer(t)
	bad := s.handleReviewDisposition(json.RawMessage("{}"))
	if bad.Success || !strings.Contains(bad.Error, "id is required") {
		t.Fatalf("empty-ID response = %+v", bad)
	}
	data, _ := json.Marshal(ReviewDispositionRequest{ID: "missing", Decision: session.ReviewDecisionReviewed})
	resp := s.handleRequest(&Request{Action: "review-disposition", Data: data})
	if strings.Contains(resp.Error, "unknown action") {
		t.Fatalf("review-disposition is not dispatched: %s", resp.Error)
	}
}

func TestReviewDispositionIsNotReadOnly(t *testing.T) {
	if readOnlyActions["review-disposition"] {
		t.Fatal("review-disposition persists evidence and must report timeouts as outcome unknown")
	}
}

func TestClientReportReviewDisposition_SendsActionAndDecodesInfo(t *testing.T) {
	want := session.Info{ID: "abcd1234", ReviewDisposition: session.ReviewDispositionInfo{
		Source: session.ReviewDispositionSourceReported, Decision: session.ReviewDecisionReviewed,
		WorkspaceFingerprint: "fingerprint", ReportedAt: time.Unix(20, 0).UTC(),
	}}
	data, _ := json.Marshal(want)
	sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: data})
	got, err := NewClient(sock).ReportReviewDisposition(want.ID, session.ReviewDecisionReviewed)
	if err != nil {
		t.Fatalf("ReportReviewDisposition: %v", err)
	}
	if received.Action != "review-disposition" {
		t.Fatalf("Action = %q", received.Action)
	}
	var req ReviewDispositionRequest
	if err := json.Unmarshal(received.Data, &req); err != nil {
		t.Fatal(err)
	}
	if req.ID != want.ID || req.Decision != session.ReviewDecisionReviewed {
		t.Fatalf("request = %+v", req)
	}
	if got.ReviewDisposition != want.ReviewDisposition {
		t.Fatalf("Info = %+v, want %+v", got, want)
	}
}
