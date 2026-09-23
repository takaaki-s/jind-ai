package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/remote"
	"github.com/takaaki-s/jind-ai/internal/task"
)

type scriptedRemoteCaller struct {
	operations   []remote.Operation
	starts       []remote.StartRequest
	startErr     error
	start        remote.StartResponse
	inspect      remote.InspectResponse
	operation    remote.ExecutionOperationResponse
	operationErr error
}

func (c *scriptedRemoteCaller) Call(_ context.Context, _ remote.Target, _ string, operation remote.Operation, payload any, result any) (remote.HandshakeResponse, error) {
	c.operations = append(c.operations, operation)
	handshake := remote.HandshakeResponse{Server: remote.ServerIdentity{InstanceID: "srv-1", BootID: "boot-1", JinVersion: "test"}}
	switch operation {
	case remote.OperationRepositoryPreflight:
		request := payload.(remote.PreflightRequest)
		*(result.(*remote.PreflightResponse)) = remote.PreflightResponse{
			RepositoryID:       request.RepositoryID,
			RepositoryIdentity: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			DefaultBranch:      "main", AvailableAgentKinds: []string{"claude"},
		}
	case remote.OperationExecutionStart:
		request := payload.(remote.StartRequest)
		c.starts = append(c.starts, request)
		if c.startErr != nil {
			err := c.startErr
			c.startErr = nil
			return remote.HandshakeResponse{}, err
		}
		*(result.(*remote.StartResponse)) = c.start
	case remote.OperationExecutionInspect:
		*(result.(*remote.InspectResponse)) = c.inspect
	case remote.OperationExecutionCancel, remote.OperationExecutionCleanup:
		if c.operationErr != nil {
			err := c.operationErr
			c.operationErr = nil
			return remote.HandshakeResponse{}, err
		}
		*(result.(*remote.ExecutionOperationResponse)) = c.operation
	default:
		return remote.HandshakeResponse{}, errors.New("unexpected operation")
	}
	return handshake, nil
}

func TestRemoteTaskNewRetriesUnknownStartWithoutChangingControllerIdentity(t *testing.T) {
	server := newRemoteTestServer(t, `remote:
  targets:
    build:
      ssh_host: build-host
      repositories:
        jind-ai: product
`)
	observed := time.Now().UTC().Truncate(time.Second)
	caller := &scriptedRemoteCaller{
		startErr: &remote.CallError{Kind: remote.ErrorTransport, Code: "transport", Message: "connection lost", Retryable: true},
		start: remote.StartResponse{
			RemoteTaskID: "task-target", RemoteExecutionID: "exec-target",
			Summary: task.RemoteSummary{Sequence: 1, ObservedAt: observed, Execution: task.RemoteExecutionSummary{Phase: task.ExecutionSubmitted}},
		},
		inspect: remote.InspectResponse{
			RemoteTaskID: "task-target", RemoteExecutionID: "exec-target",
			Summary: task.RemoteSummary{Sequence: 2, ObservedAt: observed.Add(time.Second), Execution: task.RemoteExecutionSummary{Phase: task.ExecutionSubmitted}},
		},
	}
	server.remoteCaller = caller
	request := TaskNewRequest{
		IdempotencyKey: "request-1", Prompt: "do it remotely", Target: "build", Repository: "jind-ai", AgentKind: "claude",
	}
	data, _ := json.Marshal(request)
	firstResponse := server.handleTaskNew(data)
	if !firstResponse.Success {
		t.Fatalf("first request: %s", firstResponse.Error)
	}
	var first TaskNewResponse
	if err := json.Unmarshal(firstResponse.Data, &first); err != nil {
		t.Fatal(err)
	}
	if first.Execution.Remote == nil || first.Execution.Remote.SyncState != task.RemoteSyncUnreachable ||
		first.Execution.Remote.RemoteExecutionID != "" {
		t.Fatalf("first execution = %+v", first.Execution)
	}

	secondResponse := server.handleTaskNew(data)
	if !secondResponse.Success {
		t.Fatalf("retry: %s", secondResponse.Error)
	}
	var second TaskNewResponse
	if err := json.Unmarshal(secondResponse.Data, &second); err != nil {
		t.Fatal(err)
	}
	if second.Task.ID != first.Task.ID || second.Execution.ID != first.Execution.ID ||
		second.Execution.Remote.SyncState != task.RemoteSyncBound || second.Execution.Remote.RemoteExecutionID != "exec-target" {
		t.Fatalf("retry changed binding: first=%+v second=%+v", first.Execution, second.Execution)
	}
	if len(caller.starts) != 2 || caller.starts[0].ControllerExecutionID != caller.starts[1].ControllerExecutionID ||
		caller.starts[0].IdempotencyKey != caller.starts[1].IdempotencyKey {
		t.Fatalf("start retries = %+v", caller.starts)
	}

	syncData, _ := json.Marshal(TaskSyncRequest{TaskID: first.Task.ID})
	syncResponse := server.handleTaskSync(syncData)
	if !syncResponse.Success {
		t.Fatalf("sync: %s", syncResponse.Error)
	}
	var synced task.Info
	if err := json.Unmarshal(syncResponse.Data, &synced); err != nil {
		t.Fatal(err)
	}
	if synced.Executions[0].Remote.Summary.Sequence != 2 {
		t.Fatalf("synced = %+v", synced.Executions[0].Remote)
	}
}

func TestRemoteTaskCancelKeepsKeyAcrossUnknownOutcome(t *testing.T) {
	server := newRemoteTestServer(t, `remote:
  targets:
    build:
      ssh_host: build-host
      repositories:
        jind-ai: product
`)
	observed := time.Now().UTC().Truncate(time.Second)
	summary := task.RemoteSummary{Sequence: 1, ObservedAt: observed, Execution: task.RemoteExecutionSummary{Phase: task.ExecutionSubmitted}}
	caller := &scriptedRemoteCaller{
		start:        remote.StartResponse{RemoteTaskID: "task-target", RemoteExecutionID: "exec-target", Summary: summary},
		operationErr: &remote.CallError{Kind: remote.ErrorTransport, Code: "transport", Message: "connection lost", Retryable: true},
		operation: remote.ExecutionOperationResponse{
			RemoteTaskID: "task-target", RemoteExecutionID: "exec-target",
			Receipt: task.RemoteOperationReceipt{IdempotencyKey: "cancel-key", Status: task.RemoteOperationSucceeded},
			Summary: task.RemoteSummary{Sequence: 2, ObservedAt: observed.Add(time.Second), Execution: task.RemoteExecutionSummary{Phase: task.ExecutionSubmitted}},
		},
	}
	server.remoteCaller = caller
	data, _ := json.Marshal(TaskNewRequest{
		IdempotencyKey: "start-key", Prompt: "do it remotely", Target: "build", Repository: "jind-ai", AgentKind: "claude",
	})
	createdResponse := server.handleTaskNew(data)
	var created TaskNewResponse
	if !createdResponse.Success || json.Unmarshal(createdResponse.Data, &created) != nil {
		t.Fatalf("create = %+v", createdResponse)
	}
	requestData, _ := json.Marshal(TaskRemoteOperationRequest{TaskID: created.Task.ID, Confirm: true, IdempotencyKey: "cancel-key"})
	firstResponse := server.handleTaskCancel(requestData)
	var first task.Info
	if !firstResponse.Success || json.Unmarshal(firstResponse.Data, &first) != nil {
		t.Fatalf("first cancel = %+v", firstResponse)
	}
	if got := first.Executions[0].Remote.Cancel; got == nil || got.Status != task.RemoteOperationUnknown || got.IdempotencyKey != "cancel-key" {
		t.Fatalf("unknown receipt = %+v", got)
	}
	secondResponse := server.handleTaskCancel(requestData)
	var second task.Info
	if !secondResponse.Success || json.Unmarshal(secondResponse.Data, &second) != nil {
		t.Fatalf("retry cancel = %+v", secondResponse)
	}
	if got := second.Executions[0].Remote.Cancel; got == nil || got.Status != task.RemoteOperationSucceeded || got.IdempotencyKey != "cancel-key" {
		t.Fatalf("reconciled receipt = %+v", got)
	}
	if len(caller.operations) < 4 || caller.operations[len(caller.operations)-2] != remote.OperationExecutionCancel ||
		caller.operations[len(caller.operations)-1] != remote.OperationExecutionCancel {
		t.Fatalf("operations = %+v", caller.operations)
	}
}

func TestRemoteBackendStartIsIdempotentBeforeSessionSideEffects(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := newRemoteTestServer(t, fmt.Sprintf(`worktree:
  default_branch: main
remote:
  serve:
    enabled: true
    repositories:
      product: %q
`, repo))
	driver := newFakeTaskExecutionDriver()
	manager, err := task.NewManager(filepath.Join(dir, "tasks"), driver)
	if err != nil {
		t.Fatal(err)
	}
	server.taskManager = manager
	server.taskDriver = driver
	server.taskAgentValidator = func(string) error { return nil }
	preflight, wireErr := server.remoteRepositoryPreflight(remote.PreflightRequest{RepositoryID: "product"})
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	prompt := "implement once"
	request := remote.StartRequest{
		ControllerExecutionID: "exec-controller", IdempotencyKey: "request-controller", RepositoryID: "product",
		ExpectedRepositoryIdentity: preflight.RepositoryIdentity, Title: "Remote implementation",
		Source: task.Source{Kind: "prompt", Ref: "sha256:" + task.PromptDigest(prompt)}, AgentKind: "claude",
		Prompt: remote.PromptEnvelope{SHA256: task.PromptDigest(prompt), Bytes: len(prompt), Body: prompt},
	}
	first, wireErr := server.remoteExecutionStart("ctl-controller", request)
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	waitForTaskRunPhase(t, manager, first.RemoteTaskID, task.ExecutionSubmitted)
	second, wireErr := server.remoteExecutionStart("ctl-controller", request)
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	if second.RemoteTaskID != first.RemoteTaskID || second.RemoteExecutionID != first.RemoteExecutionID {
		t.Fatalf("duplicate identities: first=%+v second=%+v", first, second)
	}
	if driver.reserveCalls != 1 || driver.provisionCalls != 1 || driver.submitCalls != 1 {
		t.Fatalf("duplicate side effects: reserve=%d provision=%d submit=%d", driver.reserveCalls, driver.provisionCalls, driver.submitCalls)
	}
	inspected, wireErr := server.remoteExecutionInspect("ctl-controller", remote.InspectRequest{
		ControllerExecutionID: request.ControllerExecutionID, RemoteExecutionID: first.RemoteExecutionID,
	})
	if wireErr != nil || inspected.Summary.Sequence < second.Summary.Sequence {
		t.Fatalf("inspect = %+v, %v", inspected, wireErr)
	}
}

func TestRemoteBackendCancelAndCleanupAreIdempotentAndTargetOwned(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := newRemoteTestServer(t, fmt.Sprintf(`worktree:
  default_branch: main
remote:
  serve:
    enabled: true
    repositories:
      product: %q
`, repo))
	driver := newFakeTaskExecutionDriver()
	manager, err := task.NewManager(filepath.Join(dir, "tasks"), driver)
	if err != nil {
		t.Fatal(err)
	}
	server.taskManager, server.taskDriver = manager, driver
	server.taskAgentValidator = func(string) error { return nil }
	preflight, wireErr := server.remoteRepositoryPreflight(remote.PreflightRequest{RepositoryID: "product"})
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	prompt := "implement and stop"
	started, wireErr := server.remoteExecutionStart("ctl-controller", remote.StartRequest{
		ControllerExecutionID: "exec-controller", IdempotencyKey: "start-key", RepositoryID: "product",
		ExpectedRepositoryIdentity: preflight.RepositoryIdentity, Title: "Remote lifecycle",
		Source: task.Source{Kind: "prompt", Ref: "sha256:" + task.PromptDigest(prompt)}, AgentKind: "claude",
		Prompt: remote.PromptEnvelope{SHA256: task.PromptDigest(prompt), Bytes: len(prompt), Body: prompt},
	})
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	waitForTaskRunPhase(t, manager, started.RemoteTaskID, task.ExecutionSubmitted)
	base := remote.ExecutionOperationRequest{
		ControllerExecutionID: "exec-controller", RemoteExecutionID: started.RemoteExecutionID,
		IdempotencyKey: "cancel-key",
	}
	first, wireErr := server.remoteExecutionOperation("ctl-controller", base, task.RemoteOperationCancel)
	if wireErr != nil || first.Receipt.Status != task.RemoteOperationSucceeded || driver.killCalls != 1 {
		t.Fatalf("cancel = %+v, %v; calls=%d", first, wireErr, driver.killCalls)
	}
	second, wireErr := server.remoteExecutionOperation("ctl-controller", base, task.RemoteOperationCancel)
	if wireErr != nil || second.Receipt.Status != task.RemoteOperationSucceeded || driver.killCalls != 1 {
		t.Fatalf("cancel retry = %+v, %v; calls=%d", second, wireErr, driver.killCalls)
	}
	conflict := base
	conflict.IdempotencyKey = "different-key"
	if _, wireErr = server.remoteExecutionOperation("ctl-controller", conflict, task.RemoteOperationCancel); wireErr == nil || wireErr.Code != "conflict" {
		t.Fatalf("different cancel key = %+v", wireErr)
	}
	cleanup := base
	cleanup.IdempotencyKey = "cleanup-key"
	cleaned, wireErr := server.remoteExecutionOperation("ctl-controller", cleanup, task.RemoteOperationCleanup)
	if wireErr != nil || cleaned.Receipt.Status != task.RemoteOperationSucceeded ||
		cleaned.Receipt.Removed != (task.RemoteRemovedResources{Session: true, Worktree: true, Branch: true}) || driver.cleanupCalls != 1 {
		t.Fatalf("cleanup = %+v, %v; calls=%d", cleaned, wireErr, driver.cleanupCalls)
	}
	if _, wireErr = server.remoteExecutionOperation("ctl-controller", cleanup, task.RemoteOperationCleanup); wireErr != nil || driver.cleanupCalls != 1 {
		t.Fatalf("cleanup retry = %v; calls=%d", wireErr, driver.cleanupCalls)
	}
}
