package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestHandleCheckReport_ValidatesAndDispatches(t *testing.T) {
	s := newTestServer(t)
	bad := s.handleCheckReport(json.RawMessage("{}"))
	if bad.Success || !strings.Contains(bad.Error, "id is required") {
		t.Fatalf("empty-ID response = %+v", bad)
	}

	data, _ := json.Marshal(CheckReportRequest{ID: "missing", Status: session.CheckStatusFailed})
	resp := s.handleRequest(&Request{Action: "check-report", Data: data})
	if strings.Contains(resp.Error, "unknown action") {
		t.Fatalf("check-report is not dispatched: %s", resp.Error)
	}
}

func TestCheckReportIsNotReadOnly(t *testing.T) {
	if readOnlyActions["check-report"] || readOnlyActions["check-report-record"] {
		t.Fatal("check-report persists evidence and must report timeouts as outcome unknown")
	}
}

func TestClientRecordChecksPreservesSubmissionAndReceipt(t *testing.T) {
	submission := session.CheckReportSubmission{Status: session.CheckStatusPassed, WorkspaceFingerprint: strings.Repeat("a", 64),
		CheckReportMetadata: session.CheckReportMetadata{IdempotencyKey: "run-1", Name: "unit", Reporter: "runner", Summary: "ok",
			StartedAt: time.Unix(10, 0).UTC(), FinishedAt: time.Unix(20, 0).UTC()},
	}
	want := session.CheckReportRecordResult{Reused: true, Receipt: session.CheckReportReceipt{
		CheckReport:         session.CheckReport{Source: session.CheckSourceReported, Status: submission.Status, WorkspaceFingerprint: submission.WorkspaceFingerprint, ReportedAt: time.Unix(30, 0).UTC()},
		CheckReportMetadata: submission.CheckReportMetadata,
	}}
	body, _ := json.Marshal(want)
	sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: body})
	got, err := NewClient(sock).RecordChecks("session", submission)
	if err != nil {
		t.Fatal(err)
	}
	var req CheckReportRecordRequest
	if err := json.Unmarshal(received.Data, &req); err != nil {
		t.Fatal(err)
	}
	if received.Action != "check-report-record" || req.ID != "session" || req.CheckReportSubmission != submission || !got.Reused || got.Receipt != want.Receipt {
		t.Fatalf("request=%+v result=%+v", req, got)
	}
	s := newTestServer(t)
	invalid := s.handleRequest(&Request{Action: "check-report-record", Data: json.RawMessage(`{"id":"session"}`)})
	if invalid.Success || strings.Contains(invalid.Error, "unknown action") {
		t.Fatalf("invalid dispatch=%+v", invalid)
	}
}

func TestClientReportChecks_SendsActionAndDecodesInfo(t *testing.T) {
	want := session.Info{
		ID: "abcd1234",
		Attention: session.AttentionInfo{
			State: session.AttentionChecksFailed, Generation: 2, Unseen: true,
		},
		CheckReport: session.CheckReportInfo{
			Source: session.CheckSourceReported, Status: session.CheckStatusFailed,
			WorkspaceFingerprint: "fingerprint", ReportedAt: time.Unix(20, 0).UTC(),
		},
	}
	data, _ := json.Marshal(want)
	sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: data})

	got, err := NewClient(sock).ReportChecks(want.ID, session.CheckStatusFailed)
	if err != nil {
		t.Fatalf("ReportChecks: %v", err)
	}
	if received.Action != "check-report" {
		t.Fatalf("Action = %q", received.Action)
	}
	var req CheckReportRequest
	if err := json.Unmarshal(received.Data, &req); err != nil {
		t.Fatal(err)
	}
	if req.ID != want.ID || req.Status != session.CheckStatusFailed {
		t.Fatalf("request = %+v", req)
	}
	if got.CheckReport != want.CheckReport || got.Attention != want.Attention {
		t.Fatalf("Info = %+v, want %+v", got, want)
	}
}
