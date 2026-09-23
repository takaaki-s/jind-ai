package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/remote"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/task"
)

type TaskSyncRequest struct {
	TaskID string `json:"task_id"`
}

type TaskRemoteOperationRequest struct {
	TaskID         string `json:"task_id"`
	Confirm        bool   `json:"confirm"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *Server) handleRemoteTaskNew(req TaskNewRequest) Response {
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.IdempotencyKey == "" {
		return Response{Success: false, Error: "idempotency_key is required"}
	}
	if strings.TrimSpace(req.Target) == "" || strings.TrimSpace(req.Repository) == "" || strings.TrimSpace(req.Repo) != "" {
		return Response{Success: false, Error: "remote tasks require --target and --repository instead of --repo"}
	}
	if strings.TrimSpace(req.Issue) != "" {
		return Response{Success: false, Error: "remote task execution currently supports direct prompts only"}
	}
	if req.Prompt == "" || len(req.Prompt) > task.MaxPromptBytes || !session.PromptVerifiable(req.Prompt) {
		return Response{Success: false, Error: "a bounded, verifiable prompt is required"}
	}
	if err := validateRelativeWorkDir(req.RelativeWorkDir); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	targetConfig, remoteRepository, revision, err := s.configMgr.ResolveRemoteTarget(req.Target, req.Repository)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if strings.TrimSpace(req.Title) == "" {
		req.Title = filepath.Base(req.Repository) + " task"
	}
	digest := task.PromptDigest(req.Prompt)
	source := task.Source{Kind: "prompt", Ref: "sha256:" + digest}
	reservation, err := s.taskManager.ReserveRemoteRun(task.RemoteRunOptions{
		IdempotencyKey: req.IdempotencyKey, Title: req.Title, TargetID: req.Target,
		TargetRevision: revision, RepositoryLabel: req.Repository, RepositoryID: remoteRepository,
		RelativeWorkDir: req.RelativeWorkDir, AgentKind: req.AgentKind, Model: req.Model,
		Fleet: req.Fleet, NoHook: req.NoHook, RequestedBase: req.RequestedBase,
		PromptSHA256: digest, PromptBytes: len(req.Prompt), Source: source,
	})
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	execution := reservation.Execution
	if execution.Remote == nil || execution.Run == nil {
		return Response{Success: false, Error: "reserved remote execution is incomplete"}
	}
	if execution.Remote.RemoteExecutionID != "" {
		updated := s.syncRemoteExecution(reservation.Task, execution, targetConfig, remoteRepository, revision)
		return remoteTaskNewSuccess(updated, execution.ID)
	}
	target := remoteTarget(req.Target, revision, targetConfig)
	controllerID, err := s.stateMgr.EnsureRemoteControllerID()
	if err != nil {
		updated := s.setRemoteFailure(reservation.Task.ID, execution.ID, task.RemoteSyncBlocked, "remote controller identity is unavailable")
		return remoteTaskNewSuccess(updated, execution.ID)
	}
	var preflight remote.PreflightResponse
	handshake, callErr := s.remoteCaller.Call(context.Background(), target, controllerID,
		remote.OperationRepositoryPreflight, remote.PreflightRequest{RepositoryID: remoteRepository}, &preflight)
	if callErr != nil {
		updated := s.recordRemoteCallFailure(reservation.Task.ID, execution.ID, callErr)
		return remoteTaskNewSuccess(updated, execution.ID)
	}
	updated, err := s.taskManager.ApplyRemotePreflight(reservation.Task.ID, execution.ID, revision,
		handshake.Server.InstanceID, handshake.Server.BootID, handshake.Capabilities, preflight.RepositoryIdentity)
	if err != nil {
		return remoteTaskNewSuccess(updated, execution.ID)
	}
	target.ExpectedServerInstanceID = handshake.Server.InstanceID
	var started remote.StartResponse
	handshake, callErr = s.remoteCaller.Call(context.Background(), target, controllerID, remote.OperationExecutionStart,
		remote.StartRequest{
			ControllerExecutionID: execution.Remote.ControllerExecutionID, IdempotencyKey: execution.Run.IdempotencyKey,
			RepositoryID: remoteRepository, ExpectedRepositoryIdentity: preflight.RepositoryIdentity,
			Title: reservation.Task.Title, Source: reservation.Task.Source, RequestedBase: execution.Run.RequestedBase,
			RelativeWorkDir: execution.Run.RelativeWorkDir, AgentKind: execution.Run.AgentKind,
			Model: execution.Run.Model, Fleet: execution.Run.Fleet, NoHook: execution.Run.NoHook,
			Prompt: remote.PromptEnvelope{SHA256: execution.Run.Prompt.SHA256, Bytes: execution.Run.Prompt.Bytes, Body: req.Prompt},
		}, &started)
	if callErr != nil {
		updated = s.recordRemoteCallFailure(reservation.Task.ID, execution.ID, callErr)
		return remoteTaskNewSuccess(updated, execution.ID)
	}
	updated, err = s.taskManager.BindRemoteExecution(reservation.Task.ID, execution.ID,
		handshake.Server.InstanceID, handshake.Server.BootID, started.RemoteTaskID, started.RemoteExecutionID, started.Summary)
	if err != nil {
		return remoteTaskNewSuccess(updated, execution.ID)
	}
	return remoteTaskNewSuccess(updated, execution.ID)
}

func (s *Server) handleTaskSync(data json.RawMessage) Response {
	var req TaskSyncRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	info, ok := s.taskManager.Get(req.TaskID)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("task not found: %s", req.TaskID)}
	}
	execution, err := latestRemoteExecution(info)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	link := execution.Remote
	targetConfig, remoteRepository, revision, err := s.configMgr.ResolveRemoteTarget(link.TargetID, link.RepositoryLabel)
	if err != nil {
		updated := s.setRemoteFailure(info.ID, execution.ID, task.RemoteSyncBlocked, err.Error())
		return taskSyncSuccess(updated)
	}
	updated := s.syncRemoteExecution(info, execution, targetConfig, remoteRepository, revision)
	return taskSyncSuccess(updated)
}

func (s *Server) handleTaskCancel(data json.RawMessage) Response {
	return s.handleTaskRemoteOperation(data, task.RemoteOperationCancel)
}

func (s *Server) handleTaskCleanup(data json.RawMessage) Response {
	return s.handleTaskRemoteOperation(data, task.RemoteOperationCleanup)
}

func (s *Server) handleTaskRemoteOperation(data json.RawMessage, kind task.RemoteOperationKind) Response {
	var req TaskRemoteOperationRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if !req.Confirm || strings.TrimSpace(req.IdempotencyKey) == "" {
		return Response{Success: false, Error: fmt.Sprintf("remote %s requires --confirm and an idempotency key", kind)}
	}
	info, ok := s.taskManager.Get(req.TaskID)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("task not found: %s", req.TaskID)}
	}
	execution, err := latestRemoteExecution(info)
	if err != nil || execution.Remote.RemoteExecutionID == "" {
		return Response{Success: false, Error: fmt.Sprintf("remote %s requires a bound remote execution", kind)}
	}
	link := execution.Remote
	targetConfig, remoteRepository, revision, err := s.configMgr.ResolveRemoteTarget(link.TargetID, link.RepositoryLabel)
	if err != nil || revision != link.TargetRevision || remoteRepository != link.RepositoryID {
		return Response{Success: false, Error: "remote target configuration changed"}
	}
	updated, receipt, shouldCall, err := s.taskManager.ReserveRemoteOperation(info.ID, execution.ID, kind, req.IdempotencyKey)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if !shouldCall {
		return taskSyncSuccess(updated)
	}
	controllerID, err := s.stateMgr.EnsureRemoteControllerID()
	if err != nil {
		return Response{Success: false, Error: "remote controller identity is unavailable"}
	}
	request := remote.ExecutionOperationRequest{
		ControllerExecutionID: link.ControllerExecutionID, RemoteExecutionID: link.RemoteExecutionID,
		IdempotencyKey: req.IdempotencyKey,
	}
	target := remoteTarget(link.TargetID, revision, targetConfig)
	target.ExpectedServerInstanceID = link.ServerInstanceID
	var result remote.ExecutionOperationResponse
	operation := remote.OperationExecutionCancel
	if kind == task.RemoteOperationCleanup {
		operation = remote.OperationExecutionCleanup
	}
	handshake, callErr := s.remoteCaller.Call(context.Background(), target, controllerID, operation, request, &result)
	if callErr != nil {
		receipt.Status = task.RemoteOperationUnknown
		receipt.Error = "remote operation outcome is unknown"
		receipt.Guidance = "retry the same operation with the same idempotency key"
		if _, applyErr := s.taskManager.ApplyRemoteOperation(info.ID, execution.ID, kind, receipt); applyErr != nil {
			return Response{Success: false, Error: "remote operation outcome could not be persisted"}
		}
		updated = s.recordRemoteCallFailure(info.ID, execution.ID, callErr)
		return taskSyncSuccess(updated)
	}
	updated, err = s.taskManager.ApplyRemoteInspection(info.ID, execution.ID,
		handshake.Server.InstanceID, handshake.Server.BootID,
		result.RemoteTaskID, result.RemoteExecutionID, result.Summary)
	if err != nil {
		return taskSyncSuccess(updated)
	}
	updated, err = s.taskManager.ApplyRemoteOperation(info.ID, execution.ID, kind, result.Receipt)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return taskSyncSuccess(updated)
}

func (s *Server) syncRemoteExecution(info task.Info, execution task.ExecutionInfo, targetConfig config.RemoteTargetConfig, remoteRepository, revision string) task.Info {
	link := execution.Remote
	if link == nil {
		return info
	}
	if revision != link.TargetRevision || remoteRepository != link.RepositoryID {
		return s.setRemoteFailure(info.ID, execution.ID, task.RemoteSyncBlocked, "remote target configuration changed")
	}
	if link.RemoteExecutionID == "" {
		return s.setRemoteFailure(info.ID, execution.ID, task.RemoteSyncUnreachable,
			"remote start outcome is not bound; retry task new with the same idempotency key and prompt")
	}
	controllerID, err := s.stateMgr.EnsureRemoteControllerID()
	if err != nil {
		return s.setRemoteFailure(info.ID, execution.ID, task.RemoteSyncBlocked, "remote controller identity is unavailable")
	}
	var inspected remote.InspectResponse
	target := remoteTarget(link.TargetID, revision, targetConfig)
	target.ExpectedServerInstanceID = link.ServerInstanceID
	handshake, callErr := s.remoteCaller.Call(context.Background(), target, controllerID,
		remote.OperationExecutionInspect, remote.InspectRequest{
			ControllerExecutionID: link.ControllerExecutionID, RemoteExecutionID: link.RemoteExecutionID,
		}, &inspected)
	if callErr != nil {
		return s.recordRemoteCallFailure(info.ID, execution.ID, callErr)
	}
	updated, err := s.taskManager.ApplyRemoteInspection(info.ID, execution.ID,
		handshake.Server.InstanceID, handshake.Server.BootID,
		inspected.RemoteTaskID, inspected.RemoteExecutionID, inspected.Summary)
	if err != nil {
		return updated
	}
	return updated
}

func latestRemoteExecution(info task.Info) (task.ExecutionInfo, error) {
	if len(info.Executions) == 0 {
		return task.ExecutionInfo{}, fmt.Errorf("task has no executions")
	}
	execution := info.Executions[len(info.Executions)-1]
	if execution.Backend != task.ExecutionBackendRemote || execution.Remote == nil {
		return task.ExecutionInfo{}, fmt.Errorf("latest task execution is not remote")
	}
	return execution, nil
}

func remoteTarget(id, revision string, target config.RemoteTargetConfig) remote.Target {
	return remote.Target{ID: id, Revision: revision, SSHHost: target.SSHHost, JinPath: target.JinPath}
}

func (s *Server) recordRemoteCallFailure(taskID, executionID string, err error) task.Info {
	state := task.RemoteSyncBlocked
	message := "remote operation failed"
	var callErr *remote.CallError
	if errors.As(err, &callErr) {
		message = fmt.Sprintf("remote %s error (%s): %s", callErr.Kind, callErr.Code, callErr.Message)
		if callErr.Retryable || callErr.Kind == remote.ErrorAuth || callErr.Kind == remote.ErrorTimeout || callErr.Kind == remote.ErrorTransport {
			state = task.RemoteSyncUnreachable
		}
	}
	return s.setRemoteFailure(taskID, executionID, state, message)
}

func (s *Server) setRemoteFailure(taskID, executionID string, state task.RemoteSyncState, message string) task.Info {
	updated, err := s.taskManager.SetRemoteSyncState(taskID, executionID, state, message)
	if err != nil {
		if current, ok := s.taskManager.Get(taskID); ok {
			return current
		}
	}
	return updated
}

func remoteTaskNewSuccess(info task.Info, executionID string) Response {
	execution := task.ExecutionInfo{}
	for _, candidate := range info.Executions {
		if candidate.ID == executionID {
			execution = candidate
			break
		}
	}
	payload, _ := json.Marshal(TaskNewResponse{Task: info, Execution: execution})
	return Response{Success: true, Data: payload}
}

func taskSyncSuccess(info task.Info) Response {
	payload, _ := json.Marshal(info)
	return Response{Success: true, Data: payload}
}
