package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/provider"
	"github.com/takaaki-s/jind-ai/internal/task"
)

type fakeIssueComments struct {
	inspection   provider.IssueCommentInspection
	inspectErr   error
	created      provider.IssueComment
	createErr    error
	inspectCalls int
	createCalls  int
	body         string
}

func (f *fakeIssueComments) InspectIssueComment(_ context.Context, _ provider.IssueRef, _, _ string) (provider.IssueCommentInspection, error) {
	f.inspectCalls++
	return f.inspection, f.inspectErr
}

func (f *fakeIssueComments) CreateIssueComment(_ context.Context, _ provider.IssueRef, _, _, _, body string) (provider.IssueComment, error) {
	f.createCalls++
	f.body = body
	return f.created, f.createErr
}

func newIssueCommentServer(t *testing.T) (*Server, *fakeIssueComments, task.Info) {
	t.Helper()
	s, _, _ := newTaskNewTestServer(t)
	ref, _ := provider.ParseGitHubIssueReference("owner/repo#7")
	s.issueReader = &fakeIssueReader{issue: provider.Issue{
		Ref: ref, Title: "Issue", State: "open", SyncToken: "2026-09-14T00:00:00Z",
	}}
	comments := &fakeIssueComments{
		inspection: provider.IssueCommentInspection{Actor: "octocat"},
		created: provider.IssueComment{
			Provider: "github", ID: "91", URL: ref.URL + "#issuecomment-91", Actor: "octocat",
		},
	}
	s.issueCommentReader = comments
	s.issueCommentWriter = comments
	info, err := s.taskManager.Create(task.CreateOptions{Title: "Issue", Source: task.Source{
		Kind: "issue", Ref: ref.URL, Provider: ref.Provider, Repository: ref.Repository,
		ExternalID: "7", URL: ref.URL, SyncToken: "2026-09-13T00:00:00Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s, comments, info
}

func taskCommentJSON(t *testing.T, req TaskCommentRequest) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestHandleTaskCommentDryRunIsReadOnlyAndConfirmAuditsSuccess(t *testing.T) {
	s, comments, info := newIssueCommentServer(t)
	body := "Implemented and verified."
	dry := s.handleTaskComment(taskCommentJSON(t, TaskCommentRequest{TaskID: info.ID, Body: body, DryRun: true}))
	if !dry.Success {
		t.Fatalf("dry-run: %s", dry.Error)
	}
	var preview TaskCommentResponse
	if err := json.Unmarshal(dry.Data, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Plan.IdempotencyKey == "" || preview.Plan.Actor != "octocat" || preview.Plan.Request.Bytes != len(body) {
		t.Fatalf("plan = %+v", preview.Plan)
	}
	if comments.createCalls != 0 || len(preview.Task.Mutations) != 0 {
		t.Fatalf("dry-run mutated state: provider=%d task=%+v", comments.createCalls, preview.Task.Mutations)
	}

	confirmed := s.handleTaskComment(taskCommentJSON(t, TaskCommentRequest{
		TaskID: info.ID, Body: body, Confirm: true, IdempotencyKey: preview.Plan.IdempotencyKey,
	}))
	if !confirmed.Success {
		t.Fatalf("confirm: %s", confirmed.Error)
	}
	var result TaskCommentResponse
	if err := json.Unmarshal(confirmed.Data, &result); err != nil {
		t.Fatal(err)
	}
	if comments.createCalls != 1 || comments.body != body || result.Mutation.Status != task.MutationSucceeded ||
		result.Mutation.Result.ID != "91" || len(result.Task.Mutations) != 1 {
		t.Fatalf("calls=%d mutation=%+v task=%+v", comments.createCalls, result.Mutation, result.Task.Mutations)
	}
	encoded, _ := json.Marshal(result.Task)
	if strings.Contains(string(encoded), body) {
		t.Fatalf("comment body persisted: %s", encoded)
	}

	again := s.handleTaskComment(taskCommentJSON(t, TaskCommentRequest{
		TaskID: info.ID, Body: body, Confirm: true, IdempotencyKey: preview.Plan.IdempotencyKey,
	}))
	if !again.Success || comments.createCalls != 1 {
		t.Fatalf("idempotent retry=%+v createCalls=%d", again, comments.createCalls)
	}
}

func TestHandleTaskCommentUnknownNeverBlindlyResubmits(t *testing.T) {
	s, comments, info := newIssueCommentServer(t)
	comments.createErr = &provider.CommentMutationError{Err: errors.New("secret-token")}
	body := "Please verify this."
	key := task.DefaultMutationKey(info.ID, "issue_comment",
		task.MutationTarget{Provider: "github", Repository: "owner/repo", ExternalID: "7", URL: "https://github.com/owner/repo/issues/7"},
		task.MutationRequest{SHA256: task.MutationBodyDigest(body), Bytes: len(body)})
	first := s.handleTaskComment(taskCommentJSON(t, TaskCommentRequest{TaskID: info.ID, Body: body, Confirm: true, IdempotencyKey: key}))
	if !first.Success {
		t.Fatalf("first: %s", first.Error)
	}
	var unknown TaskCommentResponse
	_ = json.Unmarshal(first.Data, &unknown)
	if unknown.Mutation.Status != task.MutationUnknown || strings.Contains(unknown.Mutation.Error, "secret-token") || comments.createCalls != 1 {
		t.Fatalf("unknown=%+v calls=%d", unknown.Mutation, comments.createCalls)
	}
	retry := s.handleTaskComment(taskCommentJSON(t, TaskCommentRequest{TaskID: info.ID, Body: body, Confirm: true, IdempotencyKey: key}))
	if !retry.Success || comments.createCalls != 1 {
		t.Fatalf("retry=%+v calls=%d", retry, comments.createCalls)
	}
}

func TestHandleTaskCommentReconcilesProviderProofAfterUnknown(t *testing.T) {
	s, comments, info := newIssueCommentServer(t)
	comments.createErr = errors.New("timeout")
	body := "Please verify this."
	request := task.MutationRequest{SHA256: task.MutationBodyDigest(body), Bytes: len(body)}
	target := task.MutationTarget{Provider: "github", Repository: "owner/repo", ExternalID: "7", URL: "https://github.com/owner/repo/issues/7"}
	key := task.DefaultMutationKey(info.ID, "issue_comment", target, request)
	first := s.handleTaskComment(taskCommentJSON(t, TaskCommentRequest{TaskID: info.ID, Body: body, Confirm: true, IdempotencyKey: key}))
	if !first.Success {
		t.Fatal(first.Error)
	}
	proof := comments.created
	comments.inspection.Existing = &proof
	retry := s.handleTaskComment(taskCommentJSON(t, TaskCommentRequest{TaskID: info.ID, Body: body, Confirm: true, IdempotencyKey: key}))
	if !retry.Success || comments.createCalls != 1 {
		t.Fatalf("retry=%+v calls=%d", retry, comments.createCalls)
	}
	var reconciled TaskCommentResponse
	_ = json.Unmarshal(retry.Data, &reconciled)
	if reconciled.Mutation.Status != task.MutationSucceeded || reconciled.Mutation.Result.ID != "91" {
		t.Fatalf("mutation = %+v", reconciled.Mutation)
	}
}

func TestHandleTaskCommentRejectsBeforeAudit(t *testing.T) {
	s, comments, info := newIssueCommentServer(t)
	tests := []TaskCommentRequest{
		{TaskID: info.ID, Body: "x"},
		{TaskID: info.ID, Body: "x", DryRun: true, Confirm: true},
		{TaskID: info.ID, Body: "   ", DryRun: true},
		{TaskID: info.ID, Body: "x", Confirm: true},
		{TaskID: info.ID, Body: "x", Confirm: true, IdempotencyKey: "wrong"},
	}
	for _, req := range tests {
		if resp := s.handleTaskComment(taskCommentJSON(t, req)); resp.Success {
			t.Fatalf("accepted %+v", req)
		}
	}
	current, _ := s.taskManager.Get(info.ID)
	if len(current.Mutations) != 0 || comments.createCalls != 0 {
		t.Fatalf("invalid request mutated state: %+v calls=%d", current.Mutations, comments.createCalls)
	}
}

func TestHandleTaskCommentReadFailureCreatesNoAuditAndLeaksNoSecret(t *testing.T) {
	s, comments, info := newIssueCommentServer(t)
	comments.inspectErr = &provider.ReadError{Kind: provider.ReadErrorAuth, Err: errors.New("secret-token")}
	resp := s.handleTaskComment(taskCommentJSON(t, TaskCommentRequest{TaskID: info.ID, Body: "done", DryRun: true}))
	if resp.Success || !strings.Contains(resp.Error, "authentication failed") || strings.Contains(resp.Error, "secret-token") {
		t.Fatalf("response = %+v", resp)
	}
	current, _ := s.taskManager.Get(info.ID)
	if len(current.Mutations) != 0 || comments.createCalls != 0 {
		t.Fatalf("read failure mutated state: %+v calls=%d", current.Mutations, comments.createCalls)
	}
}

func TestHandleTaskCommentRejectsNonIssueTask(t *testing.T) {
	s, comments, _ := newIssueCommentServer(t)
	manual, err := s.taskManager.Create(task.CreateOptions{Title: "Manual", Source: task.Source{Kind: "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	resp := s.handleTaskComment(taskCommentJSON(t, TaskCommentRequest{TaskID: manual.ID, Body: "done", DryRun: true}))
	if resp.Success || comments.inspectCalls != 0 || comments.createCalls != 0 {
		t.Fatalf("response=%+v inspect=%d create=%d", resp, comments.inspectCalls, comments.createCalls)
	}
}

func TestHandleTaskCommentRejectsNonCanonicalIssueURL(t *testing.T) {
	s, comments, _ := newIssueCommentServer(t)
	info, err := s.taskManager.Create(task.CreateOptions{Title: "Issue", Source: task.Source{
		Kind: "issue", Ref: "http://github.com/owner/repo/issues/8", Provider: "github",
		Repository: "owner/repo", ExternalID: "8", URL: "http://github.com/owner/repo/issues/8",
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp := s.handleTaskComment(taskCommentJSON(t, TaskCommentRequest{TaskID: info.ID, Body: "done", DryRun: true}))
	if resp.Success || comments.inspectCalls != 0 || comments.createCalls != 0 {
		t.Fatalf("response=%+v inspect=%d create=%d", resp, comments.inspectCalls, comments.createCalls)
	}
}
