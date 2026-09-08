package cmd

import (
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestReviewDispositionCmd_RegisteredAndSendsDecision(t *testing.T) {
	var registered bool
	for _, sub := range sessionCmd.Commands() {
		if sub.Name() == "review-disposition" {
			registered = true
			break
		}
	}
	if !registered || reviewDispositionCmd.Args == nil {
		t.Fatal("session review-disposition command is not fully registered")
	}

	listed := session.Info{ID: "abcd1234-0000", Description: "review task"}
	updated := listed
	updated.ReviewDisposition = session.ReviewDispositionInfo{
		Source: session.ReviewDispositionSourceReported, Decision: session.ReviewDecisionReviewed,
		WorkspaceFingerprint: "fingerprint", ReportedAt: time.Unix(20, 0),
	}
	d := startRecordingDaemon(t, []session.Info{listed}, okWith(updated))
	if err := reviewDispositionCmd.RunE(reviewDispositionCmd, []string{"review task", "reviewed"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	actions := d.seen()
	if len(actions) != 2 || actions[0] != "list" || actions[1] != "review-disposition" {
		t.Fatalf("actions = %v", actions)
	}
}

func TestReviewDispositionCmd_RejectsInvalidDecisionBeforeIPC(t *testing.T) {
	if err := reviewDispositionCmd.RunE(reviewDispositionCmd, []string{"task", "approved"}); err == nil {
		t.Fatal("RunE accepted an invalid review decision")
	}
}
