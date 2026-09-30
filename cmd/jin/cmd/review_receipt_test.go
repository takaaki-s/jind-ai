package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestReviewDispositionCmdRecordsMetadataAndRejectsIncompleteFlags(t *testing.T) {
	flags := map[string]string{
		"idempotency-key": "run-1", "actor": "local", "fingerprint": strings.Repeat("a", 64),
		"note": "inspected diff",
	}
	for name, value := range flags {
		flag := reviewDispositionCmd.Flags().Lookup(name)
		oldValue, oldChanged := flag.Value.String(), flag.Changed
		t.Cleanup(func() { _ = flag.Value.Set(oldValue); flag.Changed = oldChanged })
		if err := reviewDispositionCmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	submission, recorded, err := reviewDispositionSubmission(reviewDispositionCmd, session.ReviewDecisionReviewed)
	if err != nil || !recorded || submission.IdempotencyKey != "run-1" || submission.Actor != "local" {
		t.Fatalf("submission=%+v recorded=%v err=%v", submission, recorded, err)
	}
	listed := session.Info{ID: "abcd1234-0000", Description: "task"}
	d := startRecordingDaemon(t, []session.Info{listed}, func() daemon.Response {
		body, _ := json.Marshal(session.ReviewDispositionRecordResult{Receipt: session.ReviewDispositionReceipt{ReviewDispositionMetadata: submission.ReviewDispositionMetadata}})
		return daemon.Response{Success: true, Data: body}
	})
	if err := reviewDispositionCmd.RunE(reviewDispositionCmd, []string{"task", "reviewed"}); err != nil {
		t.Fatal(err)
	}
	actions := d.seen()
	if len(actions) != 2 || actions[1] != "review-disposition-record" {
		t.Fatalf("actions=%v", actions)
	}
	if err := reviewDispositionCmd.Flags().Set("fingerprint", ""); err != nil {
		t.Fatal(err)
	}
	if err := reviewDispositionCmd.RunE(reviewDispositionCmd, []string{"task", "reviewed"}); err == nil {
		t.Fatal("accepted missing fingerprint")
	}
	if len(d.seen()) != 2 {
		t.Fatal("invalid flags reached IPC")
	}
}
