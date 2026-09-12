package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestMergeHandoffCmd_RegisteredAndRunsDryRun(t *testing.T) {
	var registered bool
	for _, sub := range sessionCmd.Commands() {
		if sub.Name() == "merge-handoff" {
			registered = true
			break
		}
	}
	if !registered {
		t.Fatal("session merge-handoff command is not registered")
	}
	_ = mergeHandoffCmd.Flags().Set("dry-run", "true")
	_ = mergeHandoffCmd.Flags().Set("confirm", "false")
	_ = mergeHandoffCmd.Flags().Set("idempotency-key", "")
	t.Cleanup(func() {
		_ = mergeHandoffCmd.Flags().Set("dry-run", "false")
		_ = mergeHandoffCmd.Flags().Set("confirm", "false")
		_ = mergeHandoffCmd.Flags().Set("idempotency-key", "")
	})
	listed := session.Info{ID: "abcd1234", Description: "merge task"}
	response := daemon.MergeHandoffResponse{
		Payload: session.MergeHandoffRequest{IdempotencyKey: "mrg_generated"},
		Target:  session.MergeHandoffTarget{Plugin: "provider", Action: "merge"},
		Preflight: session.MergeHandoffPreflightResult{
			Status: "ready", Mergeable: true, RequiredChecks: session.MergeChecksPassed,
			Target: session.MergeProviderTarget{
				Provider: "fake", ID: "42", BaseRef: "main",
				BaseCommit: strings.Repeat("a", 40), HeadCommit: strings.Repeat("b", 40),
			},
		},
	}
	responseData, _ := json.Marshal(response)
	d := startRecordingDaemon(t, []session.Info{listed}, func() daemon.Response {
		return daemon.Response{Success: true, Data: responseData}
	})
	if err := mergeHandoffCmd.RunE(mergeHandoffCmd, []string{"merge task", "provider", "merge"}); err != nil {
		t.Fatal(err)
	}
	actions := d.seen()
	if len(actions) != 2 || actions[0] != "list" || actions[1] != "merge-handoff" {
		t.Fatalf("actions = %v", actions)
	}
}

func TestMergeHandoffCmd_RequiresModeAndExplicitConfirmKeyBeforeIPC(t *testing.T) {
	_ = mergeHandoffCmd.Flags().Set("dry-run", "false")
	_ = mergeHandoffCmd.Flags().Set("confirm", "false")
	_ = mergeHandoffCmd.Flags().Set("idempotency-key", "")
	if err := mergeHandoffCmd.RunE(mergeHandoffCmd, []string{"task", "provider"}); err == nil {
		t.Fatal("command accepted neither mode")
	}
	_ = mergeHandoffCmd.Flags().Set("confirm", "true")
	t.Cleanup(func() { _ = mergeHandoffCmd.Flags().Set("confirm", "false") })
	if err := mergeHandoffCmd.RunE(mergeHandoffCmd, []string{"task", "provider"}); err == nil || !strings.Contains(err.Error(), "idempotency-key") {
		t.Fatalf("missing key error = %v", err)
	}
}
