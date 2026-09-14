package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/takaaki-s/jind-ai/internal/provider"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/task"
)

type TaskCommentRequest struct {
	TaskID         string `json:"task_id"`
	Body           string `json:"body"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	DryRun         bool   `json:"dry_run,omitempty"`
	Confirm        bool   `json:"confirm,omitempty"`
}

type TaskCommentPlan struct {
	TaskID            string               `json:"task_id"`
	Target            task.MutationTarget  `json:"target"`
	Actor             string               `json:"actor"`
	Request           task.MutationRequest `json:"request"`
	ObservedSyncToken string               `json:"observed_sync_token"`
	IdempotencyKey    string               `json:"idempotency_key"`
	Existing          *task.MutationResult `json:"existing,omitempty"`
}

type TaskCommentResponse struct {
	Plan     TaskCommentPlan `json:"plan"`
	Task     task.Info       `json:"task"`
	Mutation task.Mutation   `json:"mutation,omitzero"`
}

func (s *Server) handleTaskComment(data json.RawMessage) Response {
	var req TaskCommentRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.TaskID == "" {
		return Response{Success: false, Error: "task_id is required"}
	}
	if req.DryRun == req.Confirm {
		return Response{Success: false, Error: "choose exactly one of dry_run or confirm"}
	}
	if len(req.Body) == 0 || len(req.Body) > task.MaxMutationBodyBytes {
		return Response{Success: false, Error: fmt.Sprintf("comment body must be 1-%d bytes", task.MaxMutationBodyBytes)}
	}
	if !session.PromptVerifiable(req.Body) {
		return Response{Success: false, Error: "comment body has no verifiable content"}
	}
	if s.issueReader == nil || s.issueCommentReader == nil || s.issueCommentWriter == nil {
		return Response{Success: false, Error: "GitHub Issue comment provider is unavailable"}
	}

	info, ok := s.taskManager.Get(req.TaskID)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("task not found: %s", req.TaskID)}
	}
	ref, target, err := issueMutationTarget(info.Source)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	request := task.MutationRequest{SHA256: task.MutationBodyDigest(req.Body), Bytes: len(req.Body)}
	expectedKey := task.DefaultMutationKey(info.ID, "issue_comment", target, request)
	if req.Confirm && req.IdempotencyKey == "" {
		return Response{Success: false, Error: "confirm requires the idempotency key printed by dry-run"}
	}
	if req.IdempotencyKey != "" && req.IdempotencyKey != expectedKey {
		return Response{Success: false, Error: fmt.Sprintf("idempotency key does not match this Task, target, and comment; use %s", expectedKey)}
	}

	issue, err := s.issueReader.Read(context.Background(), ref)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if issue.Ref != ref {
		return Response{Success: false, Error: "GitHub Issue reader returned a mismatched identity"}
	}
	inspection, err := s.issueCommentReader.InspectIssueComment(context.Background(), ref, expectedKey, request.SHA256)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	plan := TaskCommentPlan{
		TaskID: info.ID, Target: target, Actor: inspection.Actor, Request: request,
		ObservedSyncToken: issue.SyncToken, IdempotencyKey: expectedKey,
	}
	if inspection.Existing != nil {
		result := providerCommentResult(*inspection.Existing)
		plan.Existing = &result
	}
	if req.DryRun {
		return taskCommentSuccess(plan, info, task.Mutation{})
	}

	candidate := task.Mutation{
		IdempotencyKey: expectedKey, Kind: "issue_comment", Target: target,
		Actor: inspection.Actor, Request: request, Status: task.MutationRunning,
	}
	current, found := mutationWithKey(info, expectedKey)
	if inspection.Existing != nil {
		if !found {
			return Response{Success: false, Error: "provider already contains this idempotency marker but the Task has no matching audit; refusing to adopt or duplicate it"}
		}
		updated, mutation, err := s.taskManager.FinishMutation(info.ID, expectedKey, task.MutationSucceeded, providerCommentResult(*inspection.Existing), "")
		if err != nil {
			return Response{Success: false, Error: err.Error()}
		}
		return taskCommentSuccess(plan, updated, mutation)
	}
	if found && current.Status == task.MutationSucceeded {
		return taskCommentSuccess(plan, info, current)
	}

	reservation, err := s.taskManager.ReserveMutation(info.ID, candidate)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if !reservation.Created {
		// Running means another caller owns the single POST. Unknown means a
		// previous POST may have landed but reconciliation found no proof. In
		// either case another POST could duplicate the external side effect.
		return taskCommentSuccess(plan, reservation.Task, reservation.Mutation)
	}
	created, createErr := s.issueCommentWriter.CreateIssueComment(
		context.Background(), ref, inspection.Actor, expectedKey, request.SHA256, req.Body,
	)
	if createErr != nil {
		updated, mutation, finishErr := s.taskManager.FinishMutation(info.ID, expectedKey, task.MutationUnknown, task.MutationResult{}, "provider response was not sufficient to prove whether the comment was created")
		if finishErr != nil {
			return Response{Success: false, Error: finishErr.Error()}
		}
		return taskCommentSuccess(plan, updated, mutation)
	}
	updated, mutation, err := s.taskManager.FinishMutation(info.ID, expectedKey, task.MutationSucceeded, providerCommentResult(created), "")
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return taskCommentSuccess(plan, updated, mutation)
}

func issueMutationTarget(source task.Source) (provider.IssueRef, task.MutationTarget, error) {
	if source.Kind != "issue" || source.Provider != provider.GitHubProvider || source.Repository == "" || source.ExternalID == "" {
		return provider.IssueRef{}, task.MutationTarget{}, fmt.Errorf("task source is not an ingested GitHub Issue")
	}
	ref, err := provider.ParseGitHubIssueReference(source.URL)
	if err != nil || ref.Provider != source.Provider || ref.Repository != source.Repository ||
		strconv.Itoa(ref.Number) != source.ExternalID || ref.URL != source.URL {
		return provider.IssueRef{}, task.MutationTarget{}, fmt.Errorf("task has an invalid GitHub Issue source identity")
	}
	return ref, task.MutationTarget{
		Provider: source.Provider, Repository: source.Repository,
		ExternalID: source.ExternalID, URL: source.URL,
	}, nil
}

func providerCommentResult(value provider.IssueComment) task.MutationResult {
	return task.MutationResult{Provider: value.Provider, ID: value.ID, URL: value.URL, Actor: value.Actor}
}

func mutationWithKey(info task.Info, key string) (task.Mutation, bool) {
	for _, mutation := range info.Mutations {
		if mutation.IdempotencyKey == key {
			return mutation, true
		}
	}
	return task.Mutation{}, false
}

func taskCommentSuccess(plan TaskCommentPlan, info task.Info, mutation task.Mutation) Response {
	payload, _ := json.Marshal(TaskCommentResponse{Plan: plan, Task: info, Mutation: mutation})
	return Response{Success: true, Data: payload}
}
