package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestClientMergeHandoff_SendsExplicitModeAndDecodesResult(t *testing.T) {
	want := MergeHandoffResponse{
		Payload: session.MergeHandoffRequest{SchemaVersion: 1, Kind: "merge", IdempotencyKey: "stable-key"},
		Target:  session.MergeHandoffTarget{Plugin: "provider", Action: "merge"},
		Preflight: session.MergeHandoffPreflightResult{Status: "ready", Mergeable: true,
			RequiredChecks: session.MergeChecksPassed},
		Handoff: session.MergeHandoffInfo{MergeHandoff: session.MergeHandoff{Status: session.MergeHandoffSucceeded}},
	}
	data, _ := json.Marshal(want)
	sock, received := fakeServer(t, Response{ProtocolVersion: ProtocolVersion, Success: true, Data: data})
	req := MergeHandoffRequest{ID: "abcd", Plugin: "provider", Action: "merge", Confirm: true, IdempotencyKey: "stable-key"}
	got, err := NewClient(sock).MergeHandoff(req)
	if err != nil {
		t.Fatal(err)
	}
	if received.Action != "merge-handoff" {
		t.Fatalf("action = %q", received.Action)
	}
	var sent MergeHandoffRequest
	if err := json.Unmarshal(received.Data, &sent); err != nil {
		t.Fatal(err)
	}
	if sent != req || got.Target != want.Target || got.Handoff.Status != session.MergeHandoffSucceeded {
		t.Fatalf("sent=%+v got=%+v", sent, got)
	}
}

func TestHandleMergeHandoff_RequiresExplicitModeAndConfirmKey(t *testing.T) {
	s := newTestServer(t)
	data, _ := json.Marshal(MergeHandoffRequest{ID: "missing", Plugin: "provider"})
	resp := s.handleRequest(&Request{Action: "merge-handoff", Data: data})
	if resp.Success || !strings.Contains(resp.Error, "exactly one") {
		t.Fatalf("mode response = %+v", resp)
	}
	data, _ = json.Marshal(MergeHandoffRequest{ID: "missing", Plugin: "provider", Confirm: true})
	resp = s.handleRequest(&Request{Action: "merge-handoff", Data: data})
	if resp.Success || !strings.Contains(resp.Error, "idempotency_key") {
		t.Fatalf("key response = %+v", resp)
	}
}

func TestMergeHandoffIsNotReadOnly(t *testing.T) {
	if readOnlyActions["merge-handoff"] {
		t.Fatal("merge-handoff may mutate an external provider and must use unknown-outcome timeout wording")
	}
}

func TestReusableMergeHandoff_SkipsProviderForExactRunningOrSuccess(t *testing.T) {
	target := session.MergeHandoffTarget{Plugin: "provider", Action: "merge"}
	payload := session.MergeHandoffRequest{
		IdempotencyKey: "stable-key",
		PullRequest: session.MergeProviderTarget{
			Provider: "github", ID: "42", URL: "https://example.test/pulls/42",
		},
		Review: session.PRHandoffReviewEvidence{WorkspaceFingerprint: "fingerprint"},
	}
	current := session.MergeHandoffInfo{MergeHandoff: session.MergeHandoff{
		IdempotencyKey: "stable-key", Target: target, WorkspaceFingerprint: "fingerprint",
		PRTarget: payload.PullRequest, Status: session.MergeHandoffSucceeded,
	}}
	if !reusableMergeHandoff(current, target, "stable-key", payload) {
		t.Fatal("exact successful retry should reuse persisted result")
	}
	current.Status = session.MergeHandoffRunning
	if !reusableMergeHandoff(current, target, "stable-key", payload) {
		t.Fatal("exact running retry should reuse persisted state")
	}
	current.Status = session.MergeHandoffUnknown
	if reusableMergeHandoff(current, target, "stable-key", payload) {
		t.Fatal("unknown retry must invoke provider reconciliation")
	}
	current.Status = session.MergeHandoffSucceeded
	current.Stale = true
	if reusableMergeHandoff(current, target, "stable-key", payload) {
		t.Fatal("stale success must not be reused")
	}
}
