package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

func newMutationTestManager(t *testing.T) (*Manager, string, Info) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tasks")
	m, err := NewManager(dir, &fakeSessions{infos: map[string]session.Info{}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := m.Create(CreateOptions{Title: "Issue", Source: Source{
		Kind: "issue", Ref: "https://github.com/owner/repo/issues/7",
		Provider: "github", Repository: "owner/repo", ExternalID: "7",
		URL: "https://github.com/owner/repo/issues/7", SyncToken: "2026-09-13T00:00:00Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m, dir, created
}

func testMutation(info Info, body string) Mutation {
	target := MutationTarget{Provider: "github", Repository: "owner/repo", ExternalID: "7", URL: "https://github.com/owner/repo/issues/7"}
	request := MutationRequest{SHA256: MutationBodyDigest(body), Bytes: len(body)}
	return Mutation{
		IdempotencyKey: DefaultMutationKey(info.ID, "issue_comment", target, request),
		Kind:           "issue_comment", Target: target, Actor: "octocat", Request: request,
		Status: MutationRunning,
	}
}

func TestManagerMutationPersistsBeforeOutcomeAndIsIdempotent(t *testing.T) {
	m, _, info := newMutationTestManager(t)
	candidate := testMutation(info, "ship it")
	first, err := m.ReserveMutation(info.ID, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Mutation.ID == "" || first.Mutation.Sequence != 1 || first.Mutation.Status != MutationRunning {
		t.Fatalf("reservation = %+v", first)
	}
	again, err := m.ReserveMutation(info.ID, candidate)
	if err != nil || again.Created || again.Mutation.ID != first.Mutation.ID {
		t.Fatalf("retry = %+v, %v", again, err)
	}

	changed := candidate
	changed.Request = MutationRequest{SHA256: MutationBodyDigest("different"), Bytes: len("different")}
	if _, err := m.ReserveMutation(info.ID, changed); err == nil || !strings.Contains(err.Error(), "different provider mutation") {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestManagerMutationUnknownRestartAndReconcileSuccess(t *testing.T) {
	m, dir, info := newMutationTestManager(t)
	candidate := testMutation(info, "ship it")
	reserved, err := m.ReserveMutation(info.ID, candidate)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManager(dir, &fakeSessions{infos: map[string]session.Info{}})
	if err != nil {
		t.Fatal(err)
	}
	afterRestart, _ := restarted.Get(info.ID)
	mutation := afterRestart.Mutations[0]
	if mutation.Status != MutationUnknown || !strings.Contains(mutation.Guidance, "reconcile") {
		t.Fatalf("mutation after restart = %+v", mutation)
	}
	result := MutationResult{
		Provider: "github", ID: "91", URL: "https://github.com/owner/repo/issues/7#issuecomment-91", Actor: "octocat",
	}
	updated, outcome, err := restarted.FinishMutation(info.ID, reserved.Mutation.IdempotencyKey, MutationSucceeded, result, "")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != MutationSucceeded || updated.Mutations[0].Result != result {
		t.Fatalf("outcome=%+v task=%+v", outcome, updated.Mutations)
	}
}

func TestManagerMutationUnknownStoresNoProviderErrorOrBody(t *testing.T) {
	m, dir, info := newMutationTestManager(t)
	body := "secret comment body"
	candidate := testMutation(info, body)
	if _, err := m.ReserveMutation(info.ID, candidate); err != nil {
		t.Fatal(err)
	}
	_, outcome, err := m.FinishMutation(info.ID, candidate.IdempotencyKey, MutationUnknown, MutationResult{}, "provider response did not prove an outcome")
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != MutationUnknown || outcome.Result != (MutationResult{}) {
		t.Fatalf("outcome = %+v", outcome)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), body) || strings.Contains(string(data), "secret-token") {
		t.Fatalf("sensitive mutation content persisted: %s", data)
	}
}

func TestValidateMutationRejectsUnboundedOrCredentialedAudit(t *testing.T) {
	_, _, info := newMutationTestManager(t)
	valid := testMutation(info, "ship it")
	tests := []Mutation{valid, valid, valid, valid}
	tests[0].Actor = strings.Repeat("a", MaxMutationActorLength+1)
	tests[1].Target.URL = "https://token@github.com/owner/repo/issues/7"
	tests[2].Request.Bytes = MaxMutationBodyBytes + 1
	tests[3].IdempotencyKey = "bad key"
	for _, value := range tests {
		if err := ValidateMutation(value); err == nil {
			t.Fatalf("accepted mutation: %+v", value)
		}
	}
}
