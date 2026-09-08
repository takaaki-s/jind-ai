package cmd

import (
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/session"
)

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
