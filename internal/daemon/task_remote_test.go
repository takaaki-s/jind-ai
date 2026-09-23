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
	operations []remote.Operation
	starts     []remote.StartRequest
	startErr   error
	start      remote.StartResponse
	inspect    remote.InspectResponse
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
