// Package remote owns the bounded protocol used between a controller and a
// remote jin process over SSH stdio. It deliberately knows nothing about user
// configuration or daemon IPC.
package remote

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	ProtocolName       = "jind-ai.remote"
	ProtocolVersion    = 1
	MaxFrameBytes      = 1 << 20
	MaxDiagnosticBytes = 32 << 10
	MaxIdentifierBytes = 256
)

type Operation string

const (
	OperationHandshake           Operation = "handshake"
	OperationRepositoryPreflight Operation = "repository.preflight"
)

const (
	CapabilityRepositoryPreflight = "repository.preflight.v1"
	CapabilityExecutionStart      = "execution.start.v1"
	CapabilityExecutionInspect    = "execution.inspect.v1"
	CapabilityExecutionCancel     = "execution.cancel.v1"
	CapabilityExecutionCleanup    = "execution.cleanup.v1"
	CapabilityStructuredSummary   = "summary.structured.v1"
)

var version1Capabilities = []string{
	CapabilityRepositoryPreflight,
	CapabilityExecutionStart,
	CapabilityExecutionInspect,
	CapabilityExecutionCancel,
	CapabilityExecutionCleanup,
	CapabilityStructuredSummary,
}

func Version1Capabilities() []string {
	return slices.Clone(version1Capabilities)
}

type Request struct {
	Protocol        string          `json:"protocol"`
	ProtocolVersion int             `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	ControllerID    string          `json:"controller_id"`
	Operation       Operation       `json:"operation"`
	Payload         json.RawMessage `json:"payload"`
}

type Response struct {
	Protocol        string          `json:"protocol"`
	ProtocolVersion int             `json:"protocol_version"`
	RequestID       string          `json:"request_id"`
	Operation       Operation       `json:"operation"`
	Status          string          `json:"status"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	Error           *WireError      `json:"error,omitempty"`
}

type WireError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *WireError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func ErrorResponse(req Request, wireErr *WireError) Response {
	if wireErr == nil {
		wireErr = NewWireError("internal", "remote operation failed", false)
	}
	return Response{
		Protocol: ProtocolName, ProtocolVersion: ProtocolVersion,
		RequestID: req.RequestID, Operation: req.Operation, Status: "error", Error: wireErr,
	}
}

func SuccessResponse(req Request, payload any) (Response, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Response{}, err
	}
	return Response{
		Protocol: ProtocolName, ProtocolVersion: ProtocolVersion,
		RequestID: req.RequestID, Operation: req.Operation, Status: "ok", Payload: data,
	}, nil
}

func NewWireError(code, message string, retryable bool) *WireError {
	message = strings.TrimSpace(message)
	if len(message) > 512 {
		message = message[:512]
	}
	return &WireError{Code: code, Message: message, Retryable: retryable}
}

type HandshakeRequest struct {
	MinimumVersion       int      `json:"minimum_version"`
	MaximumVersion       int      `json:"maximum_version"`
	RequiredCapabilities []string `json:"required_capabilities"`
}

type HandshakeResponse struct {
	SelectedVersion int            `json:"selected_version"`
	Server          ServerIdentity `json:"server"`
	Capabilities    []string       `json:"capabilities"`
	Limits          Limits         `json:"limits"`
}

type ServerIdentity struct {
	InstanceID string `json:"instance_id"`
	BootID     string `json:"boot_id"`
	JinVersion string `json:"jin_version"`
}

type Limits struct {
	MaxFrameBytes      int `json:"max_frame_bytes"`
	MaxPromptBytes     int `json:"max_prompt_bytes"`
	MaxDiagnosticBytes int `json:"max_diagnostic_bytes"`
}

type PreflightRequest struct {
	RepositoryID string `json:"repository_id"`
}

type PreflightResponse struct {
	RepositoryID        string   `json:"repository_id"`
	RepositoryIdentity  string   `json:"repository_identity"`
	DefaultBranch       string   `json:"default_branch"`
	AvailableAgentKinds []string `json:"available_agent_kinds"`
}

type Target struct {
	ID       string
	Revision string
	SSHHost  string
	JinPath  string
}

type TargetPreflight struct {
	TargetID       string            `json:"target_id"`
	TargetRevision string            `json:"target_revision"`
	Server         ServerIdentity    `json:"server"`
	Capabilities   []string          `json:"capabilities"`
	Repository     PreflightResponse `json:"repository"`
}

type Backend interface {
	Handshake(HandshakeRequest) (HandshakeResponse, *WireError)
	Preflight(string, PreflightRequest) (PreflightResponse, *WireError)
}

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+:-]{0,255}$`)
var branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)
var repositoryIdentityPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func ValidateIdentifier(label, value string) error {
	if !identifierPattern.MatchString(value) {
		return fmt.Errorf("invalid %s", label)
	}
	return nil
}

func ValidateHandshake(request HandshakeRequest, response HandshakeResponse) *WireError {
	if request.MinimumVersion < 1 || request.MaximumVersion < request.MinimumVersion ||
		ProtocolVersion < request.MinimumVersion || ProtocolVersion > request.MaximumVersion {
		return NewWireError("unsupported_version", "no mutually supported remote protocol version", false)
	}
	if response.SelectedVersion != ProtocolVersion {
		return NewWireError("unsupported_version", "remote selected an unsupported protocol version", false)
	}
	if ValidateIdentifier("server instance id", response.Server.InstanceID) != nil ||
		ValidateIdentifier("server boot id", response.Server.BootID) != nil {
		return NewWireError("invalid_response", "remote returned an invalid server identity", false)
	}
	if !versionPattern.MatchString(response.Server.JinVersion) {
		return NewWireError("invalid_response", "remote returned an invalid jin version", false)
	}
	if len(response.Capabilities) > 64 {
		return NewWireError("invalid_response", "remote returned too many capabilities", false)
	}
	for _, capability := range response.Capabilities {
		if ValidateIdentifier("capability", capability) != nil {
			return NewWireError("invalid_response", "remote returned an invalid capability", false)
		}
	}
	for _, required := range request.RequiredCapabilities {
		if !slices.Contains(response.Capabilities, required) {
			return NewWireError("missing_capability", "remote does not provide required capability "+required, false)
		}
	}
	if response.Limits.MaxFrameBytes <= 0 || response.Limits.MaxFrameBytes > MaxFrameBytes ||
		response.Limits.MaxPromptBytes <= 0 || response.Limits.MaxPromptBytes > response.Limits.MaxFrameBytes ||
		response.Limits.MaxDiagnosticBytes <= 0 || response.Limits.MaxDiagnosticBytes > MaxDiagnosticBytes {
		return NewWireError("invalid_response", "remote returned invalid protocol limits", false)
	}
	return nil
}

func ValidatePreflight(request PreflightRequest, response PreflightResponse) *WireError {
	if response.RepositoryID != request.RepositoryID || ValidateIdentifier("repository id", response.RepositoryID) != nil {
		return NewWireError("invalid_response", "remote returned a different repository identity", false)
	}
	if !repositoryIdentityPattern.MatchString(response.RepositoryIdentity) {
		return NewWireError("invalid_response", "remote returned an invalid repository digest", false)
	}
	if !branchPattern.MatchString(response.DefaultBranch) || strings.Contains(response.DefaultBranch, "..") ||
		strings.HasSuffix(response.DefaultBranch, "/") || strings.Contains(response.DefaultBranch, "//") {
		return NewWireError("invalid_response", "remote returned an invalid default branch", false)
	}
	if len(response.AvailableAgentKinds) > 64 {
		return NewWireError("invalid_response", "remote returned too many agent kinds", false)
	}
	for _, kind := range response.AvailableAgentKinds {
		if ValidateIdentifier("agent kind", kind) != nil {
			return NewWireError("invalid_response", "remote returned an invalid agent kind", false)
		}
	}
	return nil
}
