package cmd

import (
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestReviewCmd_RegisteredAndSendsRefresh(t *testing.T) {
	var registered bool
	for _, sub := range sessionCmd.Commands() {
		if sub.Name() == "review" {
			registered = true
			break
		}
	}
	if !registered || reviewCmd.Args == nil {
		t.Fatal("session review command is not fully registered")
	}

	listed := session.Info{ID: "abcd1234-0000", Description: "review task"}
	updated := listed
	updated.ReviewFacts = session.ReviewFacts{
		Status: session.ReviewFactsAvailable, ChangedFiles: 2,
	}
	d := startRecordingDaemon(t, []session.Info{listed}, okWith(updated))
	if err := reviewCmd.RunE(reviewCmd, []string{"review task"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	actions := d.seen()
	if len(actions) != 2 || actions[0] != "list" || actions[1] != "review-refresh" {
		t.Fatalf("actions = %v", actions)
	}
}
