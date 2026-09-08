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
	if readOnlyActions["check-report"] {
		t.Fatal("check-report persists evidence and must report timeouts as outcome unknown")
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
			WorkspaceFingerprint: "fingerprint", ReportedAt: time.Unix(20, 0),
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
