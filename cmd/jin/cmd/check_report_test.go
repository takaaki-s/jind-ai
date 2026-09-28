package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestCheckReportCmdRecordsMetadataAndRejectsIncompleteFlags(t *testing.T) {
	flags := map[string]string{
		"idempotency-key": "run-1", "name": "unit", "reporter": "local", "fingerprint": strings.Repeat("a", 64),
		"started-at": "2026-09-28T00:00:00Z", "finished-at": "2026-09-28T00:01:00Z", "summary": "all passed",
	}
	for name, value := range flags {
		flag := checkReportCmd.Flags().Lookup(name)
		oldValue, oldChanged := flag.Value.String(), flag.Changed
		t.Cleanup(func() { _ = flag.Value.Set(oldValue); flag.Changed = oldChanged })
		if err := checkReportCmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	submission, recorded, err := checkReportSubmission(checkReportCmd, session.CheckStatusPassed)
	if err != nil || !recorded || submission.IdempotencyKey != "run-1" || submission.Name != "unit" {
		t.Fatalf("submission=%+v recorded=%v err=%v", submission, recorded, err)
	}
	listed := session.Info{ID: "abcd1234-0000", Description: "task"}
	d := startRecordingDaemon(t, []session.Info{listed}, func() daemon.Response {
		body, _ := json.Marshal(session.CheckReportRecordResult{Receipt: session.CheckReportReceipt{CheckReportMetadata: submission.CheckReportMetadata}})
		return daemon.Response{Success: true, Data: body}
	})
	if err := checkReportCmd.RunE(checkReportCmd, []string{"task", "passed"}); err != nil {
		t.Fatal(err)
	}
	actions := d.seen()
	if len(actions) != 2 || actions[1] != "check-report-record" {
		t.Fatalf("actions=%v", actions)
	}
	if err := checkReportCmd.Flags().Set("fingerprint", ""); err != nil {
		t.Fatal(err)
	}
	if err := checkReportCmd.RunE(checkReportCmd, []string{"task", "passed"}); err == nil {
		t.Fatal("accepted missing fingerprint")
	}
	if len(d.seen()) != 2 {
		t.Fatal("invalid flags reached IPC")
	}
}

func TestCheckReportCmd_RegisteredAndSendsReport(t *testing.T) {
	var registered bool
	for _, sub := range sessionCmd.Commands() {
		if sub.Name() == "check-report" {
			registered = true
			break
		}
	}
	if !registered || checkReportCmd.Args == nil {
		t.Fatal("session check-report command is not fully registered")
	}

	listed := session.Info{ID: "abcd1234-0000", Description: "check task"}
	updated := listed
	updated.CheckReport = session.CheckReportInfo{
		Source: session.CheckSourceReported, Status: session.CheckStatusFailed,
		WorkspaceFingerprint: "fingerprint", ReportedAt: time.Unix(20, 0),
	}
	d := startRecordingDaemon(t, []session.Info{listed}, okWith(updated))
	if err := checkReportCmd.RunE(checkReportCmd, []string{"check task", "failed"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	actions := d.seen()
	if len(actions) != 2 || actions[0] != "list" || actions[1] != "check-report" {
		t.Fatalf("actions = %v", actions)
	}
}

func TestCheckReportCmd_RejectsInvalidStatusBeforeIPC(t *testing.T) {
	if err := checkReportCmd.RunE(checkReportCmd, []string{"task", "unknown"}); err == nil {
		t.Fatal("RunE accepted an invalid status")
	}
}
