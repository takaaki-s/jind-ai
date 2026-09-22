package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/takaaki-s/jind-ai/internal/agent"
	jingit "github.com/takaaki-s/jind-ai/internal/git"
	"github.com/takaaki-s/jind-ai/internal/remote"
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
				Capabilities:    []string{remote.CapabilityRepositoryPreflight},
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
	serve := s.configMgr.GetRemoteServeConfig()
	if !serve.Enabled {
		return remote.PreflightResponse{}, remote.NewWireError("serving_disabled", "remote serving is disabled on this host", false)
	}
	configuredPath, ok := serve.Repositories[req.RepositoryID]
	if !ok {
		return remote.PreflightResponse{}, remote.NewWireError("repository_not_found", "remote repository is not enabled", false)
	}
	repo, err := canonicalRepo(configuredPath)
	if err != nil {
		return remote.PreflightResponse{}, remote.NewWireError("repository_unavailable", "remote repository is unavailable", false)
	}
	instanceID, err := s.stateMgr.EnsureRemoteServerInstanceID()
	if err != nil {
		return remote.PreflightResponse{}, remote.NewWireError("internal", "remote server identity is unavailable", false)
	}
	defaultBranch, err := jingit.NewClient().DetectDefaultBranch(repo)
	if err != nil {
		defaultBranch = strings.TrimSpace(s.configMgr.GetWorktreeConfig().DefaultBranch)
		if defaultBranch == "" {
			return remote.PreflightResponse{}, remote.NewWireError("repository_unavailable", "remote repository default branch cannot be detected", false)
		}
	}
	identity := sha256.Sum256([]byte(instanceID + "\x00" + req.RepositoryID + "\x00" + repo))
	return remote.PreflightResponse{
		RepositoryID: req.RepositoryID, RepositoryIdentity: fmt.Sprintf("sha256:%x", identity),
		DefaultBranch: defaultBranch, AvailableAgentKinds: agent.Kinds(),
	}, nil
}
