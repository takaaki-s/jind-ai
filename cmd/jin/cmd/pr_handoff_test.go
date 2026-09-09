package cmd

import (
	"encoding/json"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestPRHandoffCmd_RegisteredAndRunsDryRun(t *testing.T) {
	var registered bool
	for _, sub := range sessionCmd.Commands() {
		if sub.Name() == "pr-handoff" {
			registered = true
			break
		}
	}
	if !registered {
		t.Fatal("session pr-handoff command is not registered")
	}
	if err := prHandoffCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatal(err)
	}
	if err := prHandoffCmd.Flags().Set("confirm", "false"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = prHandoffCmd.Flags().Set("dry-run", "false")
		_ = prHandoffCmd.Flags().Set("confirm", "false")
	})
	listed := session.Info{ID: "abcd1234", Description: "handoff task"}
	response := daemon.PRHandoffResponse{
		Payload: session.PRHandoffRequest{SchemaVersion: 1, Kind: "pull-request", IdempotencyKey: "generated",
			Review: session.PRHandoffReviewEvidence{Branch: "feat/x", BaseCommit: "aaaaaaaa", HeadCommit: "bbbbbbbb", CommitCount: 1, ChangedFiles: 2}},
		Target: session.PRHandoffTarget{Plugin: "provider", Action: "create"},
	}
	responseData, _ := json.Marshal(response)
	d := startRecordingDaemon(t, []session.Info{listed}, func() daemon.Response {
		return daemon.Response{Success: true, Data: responseData}
	})
	if err := prHandoffCmd.RunE(prHandoffCmd, []string{"handoff task", "provider", "create"}); err != nil {
		t.Fatal(err)
	}
	actions := d.seen()
	if len(actions) != 2 || actions[0] != "list" || actions[1] != "pr-handoff" {
		t.Fatalf("actions = %v", actions)
	}
}

func TestPRHandoffCmd_RequiresExplicitModeBeforeIPC(t *testing.T) {
	_ = prHandoffCmd.Flags().Set("dry-run", "false")
	_ = prHandoffCmd.Flags().Set("confirm", "false")
	if err := prHandoffCmd.RunE(prHandoffCmd, []string{"task", "provider"}); err == nil {
		t.Fatal("command accepted neither --dry-run nor --confirm")
	}
}
