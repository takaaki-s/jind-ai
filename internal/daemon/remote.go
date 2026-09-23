package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/takaaki-s/jind-ai/internal/agent"
	jingit "github.com/takaaki-s/jind-ai/internal/git"
	"github.com/takaaki-s/jind-ai/internal/remote"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/task"
	"github.com/takaaki-s/jind-ai/internal/version"
)

type RemoteTargetPreflightRequest struct {
	Target     string `json:"target"`
	Repository string `json:"repository"`
}

type remoteBackendHandshakeResult struct {
	Value remote.HandshakeResponse `json:"value,omitzero"`
	Error *remote.WireError        `json:"error,omitempty"`
}

type remoteBackendPreflightRequest struct {
	ControllerID string                  `json:"controller_id"`
	Request      remote.PreflightRequest `json:"request"`
}

type remoteBackendPreflightResult struct {
	Value remote.PreflightResponse `json:"value,omitzero"`
	Error *remote.WireError        `json:"error,omitempty"`
}

type remoteBackendStartRequest struct {
	ControllerID string              `json:"controller_id"`
	Request      remote.StartRequest `json:"request"`
}

type remoteBackendStartResult struct {
	Value remote.StartResponse `json:"value,omitzero"`
	Error *remote.WireError    `json:"error,omitempty"`
}

type remoteBackendInspectRequest struct {
	ControllerID string                `json:"controller_id"`
	Request      remote.InspectRequest `json:"request"`
}

type remoteBackendInspectResult struct {
	Value remote.InspectResponse `json:"value,omitzero"`
	Error *remote.WireError      `json:"error,omitempty"`
}

type remoteBackendOperationRequest struct {
	ControllerID string                           `json:"controller_id"`
	Request      remote.ExecutionOperationRequest `json:"request"`
}

type remoteBackendOperationResult struct {
	Value remote.ExecutionOperationResponse `json:"value,omitzero"`
	Error *remote.WireError                 `json:"error,omitempty"`
}

func (s *Server) handleRemoteTargetPreflight(data json.RawMessage) Response {
	var req RemoteTargetPreflightRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	targetConfig, remoteRepository, revision, err := s.configMgr.ResolveRemoteTarget(req.Target, req.Repository)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	controllerID, err := s.stateMgr.EnsureRemoteControllerID()
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	target := remote.Target{
		ID: req.Target, Revision: revision, SSHHost: targetConfig.SSHHost, JinPath: targetConfig.JinPath,
	}
	var preflight remote.PreflightResponse
	handshake, err := s.remoteCaller.Call(context.Background(), target, controllerID,
		remote.OperationRepositoryPreflight, remote.PreflightRequest{RepositoryID: remoteRepository}, &preflight)
	if err != nil {
		var callErr *remote.CallError
		if errors.As(err, &callErr) {
			return Response{Success: false, Error: fmt.Sprintf("remote %s error (%s): %s", callErr.Kind, callErr.Code, callErr.Message)}
		}
		return Response{Success: false, Error: "remote transport failed"}
	}
	result := remote.TargetPreflight{
		TargetID: req.Target, TargetRevision: revision, Server: handshake.Server,
		Capabilities: append([]string(nil), handshake.Capabilities...), Repository: preflight,
	}
	payload, _ := json.Marshal(result)
	return Response{Success: true, Data: payload}
}

func (s *Server) handleRemoteBackendHandshake(data json.RawMessage) Response {
	var req remote.HandshakeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	result := remoteBackendHandshakeResult{}
	serve := s.configMgr.GetRemoteServeConfig()
	if !serve.Enabled {
		result.Error = remote.NewWireError("serving_disabled", "remote serving is disabled on this host", false)
	} else if req.MinimumVersion > remote.ProtocolVersion || req.MaximumVersion < remote.ProtocolVersion {
		result.Error = remote.NewWireError("unsupported_version", "no mutually supported remote protocol version", false)
	} else {
		instanceID, err := s.stateMgr.EnsureRemoteServerInstanceID()
		if err != nil {
			result.Error = remote.NewWireError("internal", "remote server identity is unavailable", false)
		} else {
			result.Value = remote.HandshakeResponse{
				SelectedVersion: remote.ProtocolVersion,
				Server:          remote.ServerIdentity{InstanceID: instanceID, BootID: s.remoteBootID, JinVersion: version.Version},
				Capabilities: []string{
					remote.CapabilityRepositoryPreflight,
					remote.CapabilityExecutionStart,
					remote.CapabilityExecutionInspect,
					remote.CapabilityExecutionCancel,
					remote.CapabilityExecutionCleanup,
					remote.CapabilityStructuredSummary,
				},
				Limits: remote.Limits{
					MaxFrameBytes: remote.MaxFrameBytes, MaxPromptBytes: task.MaxPromptBytes,
					MaxDiagnosticBytes: remote.MaxDiagnosticBytes,
				},
			}
		}
	}
	payload, _ := json.Marshal(result)
	return Response{Success: true, Data: payload}
}

func (s *Server) handleRemoteBackendPreflight(data json.RawMessage) Response {
	var req remoteBackendPreflightRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	result := remoteBackendPreflightResult{}
	if err := remote.ValidateIdentifier("controller id", req.ControllerID); err != nil {
		result.Error = remote.NewWireError("invalid_request", "invalid controller identity", false)
	} else {
		result.Value, result.Error = s.remoteRepositoryPreflight(req.Request)
	}
	payload, _ := json.Marshal(result)
	return Response{Success: true, Data: payload}
}

func (s *Server) remoteRepositoryPreflight(req remote.PreflightRequest) (remote.PreflightResponse, *remote.WireError) {
	response, _, wireErr := s.resolveRemoteRepository(req)
	return response, wireErr
}

func (s *Server) resolveRemoteRepository(req remote.PreflightRequest) (remote.PreflightResponse, string, *remote.WireError) {
	serve := s.configMgr.GetRemoteServeConfig()
	if !serve.Enabled {
		return remote.PreflightResponse{}, "", remote.NewWireError("serving_disabled", "remote serving is disabled on this host", false)
	}
	configuredPath, ok := serve.Repositories[req.RepositoryID]
	if !ok {
		return remote.PreflightResponse{}, "", remote.NewWireError("repository_not_found", "remote repository is not enabled", false)
	}
	repo, err := canonicalRepo(configuredPath)
	if err != nil {
		return remote.PreflightResponse{}, "", remote.NewWireError("repository_unavailable", "remote repository is unavailable", false)
	}
	instanceID, err := s.stateMgr.EnsureRemoteServerInstanceID()
	if err != nil {
		return remote.PreflightResponse{}, "", remote.NewWireError("internal", "remote server identity is unavailable", false)
	}
	defaultBranch, err := jingit.NewClient().DetectDefaultBranch(repo)
	if err != nil {
		defaultBranch = strings.TrimSpace(s.configMgr.GetWorktreeConfig().DefaultBranch)
		if defaultBranch == "" {
			return remote.PreflightResponse{}, "", remote.NewWireError("repository_unavailable", "remote repository default branch cannot be detected", false)
		}
	}
	identity := sha256.Sum256([]byte(instanceID + "\x00" + req.RepositoryID + "\x00" + repo))
	return remote.PreflightResponse{
		RepositoryID: req.RepositoryID, RepositoryIdentity: fmt.Sprintf("sha256:%x", identity),
		DefaultBranch: defaultBranch, AvailableAgentKinds: agent.Kinds(),
	}, repo, nil
}

func (s *Server) handleRemoteBackendStart(data json.RawMessage) Response {
	var req remoteBackendStartRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	result := remoteBackendStartResult{}
	if wireErr := remote.ValidateStartRequest(req.Request, task.MaxPromptBytes); wireErr != nil {
		result.Error = wireErr
	} else {
		result.Value, result.Error = s.remoteExecutionStart(req.ControllerID, req.Request)
	}
	payload, _ := json.Marshal(result)
	return Response{Success: true, Data: payload}
}

func (s *Server) handleRemoteBackendInspect(data json.RawMessage) Response {
	var req remoteBackendInspectRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	result := remoteBackendInspectResult{}
	if wireErr := remote.ValidateInspectRequest(req.Request); wireErr != nil {
		result.Error = wireErr
	} else {
		result.Value, result.Error = s.remoteExecutionInspect(req.ControllerID, req.Request)
	}
	payload, _ := json.Marshal(result)
	return Response{Success: true, Data: payload}
}

func (s *Server) handleRemoteBackendCancel(data json.RawMessage) Response {
	return s.handleRemoteBackendOperation(data, task.RemoteOperationCancel)
}

func (s *Server) handleRemoteBackendCleanup(data json.RawMessage) Response {
	return s.handleRemoteBackendOperation(data, task.RemoteOperationCleanup)
}

func (s *Server) handleRemoteBackendOperation(data json.RawMessage, kind task.RemoteOperationKind) Response {
	var req remoteBackendOperationRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	result := remoteBackendOperationResult{}
	if wireErr := remote.ValidateExecutionOperationRequest(req.Request); wireErr != nil {
		result.Error = wireErr
	} else {
		result.Value, result.Error = s.remoteExecutionOperation(req.ControllerID, req.Request, kind)
	}
	payload, _ := json.Marshal(result)
	return Response{Success: true, Data: payload}
}

func (s *Server) remoteExecutionStart(controllerID string, req remote.StartRequest) (remote.StartResponse, *remote.WireError) {
	if err := remote.ValidateIdentifier("controller id", controllerID); err != nil {
		return remote.StartResponse{}, remote.NewWireError("invalid_request", "invalid controller identity", false)
	}
	preflight, repo, wireErr := s.resolveRemoteRepository(remote.PreflightRequest{RepositoryID: req.RepositoryID})
	if wireErr != nil {
		return remote.StartResponse{}, wireErr
	}
	if preflight.RepositoryIdentity != req.ExpectedRepositoryIdentity {
		return remote.StartResponse{}, remote.NewWireError("repository_identity_mismatch", "remote repository identity changed", false)
	}
	localKey := remoteLocalIdempotencyKey(controllerID, req.IdempotencyKey)
	source := req.Source
	response := s.startLocalTask(TaskNewRequest{
		IdempotencyKey: localKey, Title: req.Title, Prompt: req.Prompt.Body, Repo: repo,
		RelativeWorkDir: req.RelativeWorkDir, RequestedBase: req.RequestedBase,
		AgentKind: req.AgentKind, Model: req.Model, Fleet: req.Fleet, NoHook: req.NoHook,
		remoteOrigin: &task.RemoteOrigin{
			ControllerID: controllerID, ControllerExecutionID: req.ControllerExecutionID,
			IdempotencyKey: req.IdempotencyKey,
		},
		sourceOverride: &source,
	})
	if !response.Success {
		return remote.StartResponse{}, remote.NewWireError("start_rejected", response.Error, false)
	}
	var started TaskNewResponse
	if err := json.Unmarshal(response.Data, &started); err != nil {
		return remote.StartResponse{}, remote.NewWireError("internal", "remote task result is unavailable", true)
	}
	summary, err := s.remoteExecutionSummary(started.Task.ID, started.Execution.ID)
	if err != nil {
		return remote.StartResponse{}, remote.NewWireError("internal", "remote execution summary is unavailable", true)
	}
	return remote.StartResponse{
		RemoteTaskID: started.Task.ID, RemoteExecutionID: started.Execution.ID, Summary: summary,
	}, nil
}

func (s *Server) remoteExecutionInspect(controllerID string, req remote.InspectRequest) (remote.InspectResponse, *remote.WireError) {
	if err := remote.ValidateIdentifier("controller id", controllerID); err != nil {
		return remote.InspectResponse{}, remote.NewWireError("invalid_request", "invalid controller identity", false)
	}
	info, execution, err := s.taskManager.FindRemoteExecution(controllerID, req.ControllerExecutionID, req.RemoteExecutionID)
	if err != nil {
		return remote.InspectResponse{}, remote.NewWireError("execution_not_found", "remote execution binding was not found", false)
	}
	summary, err := s.remoteExecutionSummary(info.ID, execution.ID)
	if err != nil {
		return remote.InspectResponse{}, remote.NewWireError("internal", "remote execution summary is unavailable", true)
	}
	return remote.InspectResponse{RemoteTaskID: info.ID, RemoteExecutionID: execution.ID, Summary: summary}, nil
}

func (s *Server) remoteExecutionOperation(controllerID string, req remote.ExecutionOperationRequest, kind task.RemoteOperationKind) (remote.ExecutionOperationResponse, *remote.WireError) {
	if err := remote.ValidateIdentifier("controller id", controllerID); err != nil {
		return remote.ExecutionOperationResponse{}, remote.NewWireError("invalid_request", "invalid controller identity", false)
	}
	info, execution, err := s.taskManager.FindRemoteExecution(controllerID, req.ControllerExecutionID, req.RemoteExecutionID)
	if err != nil || execution.Run == nil || execution.Run.RemoteOrigin == nil {
		return remote.ExecutionOperationResponse{}, remote.NewWireError("execution_not_found", "remote execution binding was not found", false)
	}
	_, receipt, shouldRun, err := s.taskManager.ReserveTargetRemoteOperation(info.ID, execution.ID, kind, req.IdempotencyKey)
	if err != nil {
		return remote.ExecutionOperationResponse{}, remote.NewWireError("conflict", safeRemoteSummaryText(err.Error()), false)
	}
	if shouldRun {
		lifecycle, ok := s.taskDriver.(remoteExecutionLifecycleDriver)
		if !ok {
			return remote.ExecutionOperationResponse{}, remote.NewWireError("internal", "remote execution lifecycle is unavailable", true)
		}
		switch kind {
		case task.RemoteOperationCancel:
			err = lifecycle.Kill(execution.SessionID)
		case task.RemoteOperationCleanup:
			var removed session.RemoteCleanupResult
			removed, err = lifecycle.CleanupRemoteOwned(session.RemoteCleanupOwnership{
				SessionID: execution.SessionID, IdempotencyKey: req.IdempotencyKey,
				RepositoryPath: execution.Run.Repo, WorktreeName: execution.Run.WorktreeName,
				Branch: execution.Run.WorktreeBranch,
			})
			receipt.Removed = task.RemoteRemovedResources{
				Session: removed.Session, Worktree: removed.Worktree, Branch: removed.Branch,
			}
			if removed.Uncertain {
				receipt.Status = task.RemoteOperationUnknown
			}
		}
		if err != nil {
			if receipt.Status != task.RemoteOperationUnknown {
				receipt.Status = task.RemoteOperationFailed
			}
			if receipt.Removed != (task.RemoteRemovedResources{}) {
				receipt.Status = task.RemoteOperationUnknown
			}
			receipt.Error = "target-owned remote operation failed"
			receipt.Guidance = "inspect the target-owned session; retry with a new key only when this receipt is failed"
		} else {
			receipt.Status = task.RemoteOperationSucceeded
			receipt.Error, receipt.Guidance = "", ""
		}
		_, err = s.taskManager.FinishTargetRemoteOperation(info.ID, execution.ID, kind, receipt)
		if err != nil {
			return remote.ExecutionOperationResponse{}, remote.NewWireError("internal", "remote operation receipt could not be persisted", true)
		}
	}
	if receipt.Status == task.RemoteOperationRunning {
		// A concurrent identical request observed the durable reservation while
		// the first request still owns the side effect. Running is internal
		// journal state; the bounded wire receipt reports uncertainty instead.
		receipt.Status = task.RemoteOperationUnknown
		receipt.Error = "remote operation is still being reconciled"
		receipt.Guidance = "retry the same operation with the same idempotency key"
	}
	summary, err := s.remoteExecutionSummary(info.ID, execution.ID)
	if err != nil {
		return remote.ExecutionOperationResponse{}, remote.NewWireError("internal", "remote execution summary is unavailable", true)
	}
	return remote.ExecutionOperationResponse{
		RemoteTaskID: info.ID, RemoteExecutionID: execution.ID, Receipt: receipt, Summary: summary,
	}, nil
}

func remoteLocalIdempotencyKey(controllerID, idempotencyKey string) string {
	digest := sha256.Sum256([]byte(controllerID + "\x00" + idempotencyKey))
	return fmt.Sprintf("remote_%x", digest[:])
}

func (s *Server) remoteExecutionSummary(taskID, executionID string) (remoteSummary task.RemoteSummary, err error) {
	info, ok := s.taskManager.Get(taskID)
	if !ok {
		return task.RemoteSummary{}, fmt.Errorf("task not found")
	}
	var execution task.ExecutionInfo
	for _, candidate := range info.Executions {
		if candidate.ID == executionID {
			execution = candidate
			break
		}
	}
	if execution.ID == "" || execution.Run == nil {
		return task.RemoteSummary{}, fmt.Errorf("execution not found")
	}
	remoteSummary.Execution = task.RemoteExecutionSummary{
		Phase: execution.Run.Phase, FailedPhase: execution.Run.FailedPhase,
		Error: safeRemoteSummaryText(execution.Run.Error), Guidance: safeRemoteSummaryText(execution.Run.Guidance),
	}
	if sess, ok := s.taskDriver.Get(execution.SessionID); ok {
		remoteSummary.Session = &task.RemoteSessionSummary{
			ID: sess.ID, Status: sess.Status, Attention: sess.Attention,
			ReviewFacts: sess.ReviewFacts, CheckReport: sess.CheckReport,
		}
	}
	digestInput, err := json.Marshal(remoteSummary)
	if err != nil {
		return task.RemoteSummary{}, err
	}
	digest := sha256.Sum256(digestInput)
	sequence, observedAt, err := s.taskManager.RecordRemoteSummaryDigest(taskID, executionID, fmt.Sprintf("%x", digest))
	if err != nil {
		return task.RemoteSummary{}, err
	}
	remoteSummary.Sequence = sequence
	remoteSummary.ObservedAt = observedAt
	return remoteSummary, nil
}

func safeRemoteSummaryText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x1b {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= task.MaxRunMessageLength {
		return value
	}
	value = value[:task.MaxRunMessageLength]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
