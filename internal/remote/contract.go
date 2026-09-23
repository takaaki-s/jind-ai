// Package remote owns the bounded protocol used between a controller and a
// remote jin process over SSH stdio. It deliberately knows nothing about user
// configuration or daemon IPC.
package remote

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/task"
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
	OperationExecutionStart      Operation = "execution.start"
	OperationExecutionInspect    Operation = "execution.inspect"
	OperationExecutionCancel     Operation = "execution.cancel"
	OperationExecutionCleanup    Operation = "execution.cleanup"
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

type StartRequest struct {
	ControllerExecutionID      string         `json:"controller_execution_id"`
	IdempotencyKey             string         `json:"idempotency_key"`
	RepositoryID               string         `json:"repository_id"`
	ExpectedRepositoryIdentity string         `json:"expected_repository_identity"`
	Title                      string         `json:"title"`
	Source                     task.Source    `json:"source"`
	RequestedBase              string         `json:"requested_base,omitempty"`
	RelativeWorkDir            string         `json:"relative_work_dir,omitempty"`
	AgentKind                  string         `json:"agent_kind"`
	Model                      string         `json:"model,omitempty"`
	Fleet                      string         `json:"fleet,omitempty"`
	NoHook                     bool           `json:"no_hook,omitempty"`
	Prompt                     PromptEnvelope `json:"prompt"`
}

type PromptEnvelope struct {
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
	Body   string `json:"body"`
}

type StartResponse struct {
	RemoteTaskID      string             `json:"remote_task_id"`
	RemoteExecutionID string             `json:"remote_execution_id"`
	Summary           task.RemoteSummary `json:"summary"`
}

type InspectRequest struct {
	ControllerExecutionID string `json:"controller_execution_id"`
	RemoteExecutionID     string `json:"remote_execution_id"`
}

type InspectResponse struct {
	RemoteTaskID      string             `json:"remote_task_id"`
	RemoteExecutionID string             `json:"remote_execution_id"`
	Summary           task.RemoteSummary `json:"summary"`
}

type ExecutionOperationRequest struct {
	ControllerExecutionID string `json:"controller_execution_id"`
	RemoteExecutionID     string `json:"remote_execution_id"`
	IdempotencyKey        string `json:"idempotency_key"`
}

type ExecutionOperationResponse struct {
	RemoteTaskID      string                      `json:"remote_task_id"`
	RemoteExecutionID string                      `json:"remote_execution_id"`
	Receipt           task.RemoteOperationReceipt `json:"receipt"`
	Summary           task.RemoteSummary          `json:"summary"`
}

type Target struct {
	ID                       string
	Revision                 string
	SSHHost                  string
	JinPath                  string
	ExpectedServerInstanceID string
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
	StartExecution(string, StartRequest) (StartResponse, *WireError)
	InspectExecution(string, InspectRequest) (InspectResponse, *WireError)
	CancelExecution(string, ExecutionOperationRequest) (ExecutionOperationResponse, *WireError)
	CleanupExecution(string, ExecutionOperationRequest) (ExecutionOperationResponse, *WireError)
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

func ValidateStartRequest(request StartRequest, maxPromptBytes int) *WireError {
	if ValidateIdentifier("controller execution id", request.ControllerExecutionID) != nil ||
		ValidateIdentifier("idempotency key", request.IdempotencyKey) != nil ||
		len(request.IdempotencyKey) > task.MaxIdempotencyKeyLength ||
		ValidateIdentifier("repository id", request.RepositoryID) != nil {
		return NewWireError("invalid_request", "invalid remote execution identity", false)
	}
	if !repositoryIdentityPattern.MatchString(request.ExpectedRepositoryIdentity) {
		return NewWireError("invalid_request", "invalid expected repository identity", false)
	}
	if err := task.ValidateCreateOptions(task.CreateOptions{
		Title: request.Title, Source: request.Source, RequestedBase: request.RequestedBase,
	}); err != nil {
		return NewWireError("invalid_request", "invalid task metadata", false)
	}
	if request.Source.Kind == "" || !safeWireText(request.Title, task.MaxTitleLength) ||
		!safeWireText(request.Source.Kind, task.MaxSourceKindLength) || !safeWireText(request.Source.Ref, task.MaxSourceRefLength) ||
		!safeWireText(request.Source.Provider, task.MaxProviderLength) || !safeWireText(request.Source.Repository, task.MaxRepositoryLength) ||
		!safeWireText(request.Source.ExternalID, task.MaxExternalIDLength) || !safeWireText(request.Source.URL, task.MaxSourceURLLength) ||
		!safeWireText(request.Source.SyncToken, task.MaxSyncTokenLength) || !safeWireText(request.RequestedBase, task.MaxRequestedBaseLength) {
		return NewWireError("invalid_request", "unsafe task metadata", false)
	}
	if request.RelativeWorkDir != "" && (filepath.IsAbs(request.RelativeWorkDir) ||
		filepath.Clean(request.RelativeWorkDir) == ".." || strings.HasPrefix(filepath.Clean(request.RelativeWorkDir), ".."+string(filepath.Separator))) {
		return NewWireError("invalid_request", "invalid relative work directory", false)
	}
	if !safeWireText(request.RelativeWorkDir, task.MaxRelativeWorkDirLength) {
		return NewWireError("invalid_request", "unsafe relative work directory", false)
	}
	if (request.AgentKind != "" && ValidateIdentifier("agent kind", request.AgentKind) != nil) ||
		len(request.AgentKind) > task.MaxAgentKindLength ||
		!safeWireText(request.Model, task.MaxModelLength) || !safeWireText(request.Fleet, task.MaxFleetLength) {
		return NewWireError("invalid_request", "invalid agent selection", false)
	}
	if maxPromptBytes <= 0 || maxPromptBytes > task.MaxPromptBytes {
		maxPromptBytes = task.MaxPromptBytes
	}
	if request.Prompt.Bytes <= 0 || request.Prompt.Bytes > maxPromptBytes ||
		request.Prompt.Bytes != len([]byte(request.Prompt.Body)) || request.Prompt.SHA256 != task.PromptDigest(request.Prompt.Body) {
		return NewWireError("invalid_request", "prompt evidence does not match body", false)
	}
	if !session.PromptVerifiable(request.Prompt.Body) {
		return NewWireError("invalid_request", "prompt has no verifiable content", false)
	}
	return nil
}

func ValidateStartResponse(request StartRequest, response StartResponse) *WireError {
	if ValidateIdentifier("remote task id", response.RemoteTaskID) != nil ||
		ValidateIdentifier("remote execution id", response.RemoteExecutionID) != nil {
		return NewWireError("invalid_response", "remote returned invalid execution identities", false)
	}
	return ValidateSummary(response.Summary)
}

func ValidateInspectRequest(request InspectRequest) *WireError {
	if ValidateIdentifier("controller execution id", request.ControllerExecutionID) != nil ||
		ValidateIdentifier("remote execution id", request.RemoteExecutionID) != nil {
		return NewWireError("invalid_request", "invalid remote execution identity", false)
	}
	return nil
}

func ValidateInspectResponse(request InspectRequest, response InspectResponse) *WireError {
	if ValidateIdentifier("remote task id", response.RemoteTaskID) != nil || response.RemoteExecutionID != request.RemoteExecutionID {
		return NewWireError("invalid_response", "remote returned different execution identities", false)
	}
	return ValidateSummary(response.Summary)
}

func ValidateExecutionOperationRequest(request ExecutionOperationRequest) *WireError {
	if ValidateIdentifier("controller execution id", request.ControllerExecutionID) != nil ||
		ValidateIdentifier("remote execution id", request.RemoteExecutionID) != nil ||
		ValidateIdentifier("idempotency key", request.IdempotencyKey) != nil ||
		len(request.IdempotencyKey) > task.MaxIdempotencyKeyLength {
		return NewWireError("invalid_request", "invalid remote operation identity", false)
	}
	return nil
}

func ValidateExecutionOperationResponse(request ExecutionOperationRequest, response ExecutionOperationResponse) *WireError {
	if ValidateIdentifier("remote task id", response.RemoteTaskID) != nil || response.RemoteExecutionID != request.RemoteExecutionID ||
		response.Receipt.IdempotencyKey != request.IdempotencyKey {
		return NewWireError("invalid_response", "remote returned different operation identities", false)
	}
	switch response.Receipt.Status {
	case task.RemoteOperationSucceeded, task.RemoteOperationFailed, task.RemoteOperationUnknown:
	default:
		return NewWireError("invalid_response", "remote returned invalid operation status", false)
	}
	return ValidateSummary(response.Summary)
}

func ValidateExecutionOperationResponseFor(operation Operation, request ExecutionOperationRequest, response ExecutionOperationResponse) *WireError {
	if wireErr := ValidateExecutionOperationResponse(request, response); wireErr != nil {
		return wireErr
	}
	if response.Receipt.Status != task.RemoteOperationSucceeded {
		return nil
	}
	switch operation {
	case OperationExecutionCancel:
		if response.Receipt.Removed != (task.RemoteRemovedResources{}) || response.Summary.Session == nil ||
			response.Summary.Session.Status != session.StatusStopped {
			return NewWireError("invalid_response", "remote cancellation success has no stopped session evidence", false)
		}
	case OperationExecutionCleanup:
		want := task.RemoteRemovedResources{Session: true, Worktree: true, Branch: true}
		if response.Receipt.Removed != want || response.Summary.Session != nil {
			return NewWireError("invalid_response", "remote cleanup success has incomplete removal evidence", false)
		}
	default:
		return NewWireError("invalid_response", "invalid remote execution operation", false)
	}
	return nil
}

func ValidateSummary(summary task.RemoteSummary) *WireError {
	if summary.Sequence == 0 || summary.ObservedAt.IsZero() || summary.ObservedAt.After(time.Now().Add(24*time.Hour)) {
		return NewWireError("invalid_response", "remote returned invalid summary metadata", false)
	}
	if !validExecutionPhase(summary.Execution.Phase) ||
		(summary.Execution.FailedPhase != "" && !validExecutionPhase(summary.Execution.FailedPhase)) ||
		!safeSummaryText(summary.Execution.Error) || !safeSummaryText(summary.Execution.Guidance) {
		return NewWireError("invalid_response", "remote returned invalid execution summary", false)
	}
	if summary.Session == nil {
		return nil
	}
	if ValidateIdentifier("remote session id", summary.Session.ID) != nil || !validSessionStatus(summary.Session.Status) {
		return NewWireError("invalid_response", "remote returned invalid session summary", false)
	}
	attention := summary.Session.Attention
	validAttentionState := attention.State == session.AttentionNone || attention.State == session.AttentionDone ||
		attention.State == session.AttentionReadyForReview || attention.State == session.AttentionChecksFailed
	expectedUnseen := attention.State != session.AttentionNone && attention.Generation > attention.SeenGeneration
	if !validAttentionState || attention.SeenGeneration > attention.Generation || attention.Unseen != expectedUnseen ||
		(attention.State == session.AttentionNone && attention.Generation != 0) {
		return NewWireError("invalid_response", "remote returned invalid attention summary", false)
	}
	if !validReviewFacts(summary.Session.ReviewFacts) || !validCheckReport(summary.Session.CheckReport) {
		return NewWireError("invalid_response", "remote returned invalid review summary", false)
	}
	return nil
}

func validReviewFacts(facts session.ReviewFacts) bool {
	if facts.IsZero() {
		return true
	}
	if facts.Status != session.ReviewFactsPending && facts.Status != session.ReviewFactsAvailable && facts.Status != session.ReviewFactsUnavailable {
		return false
	}
	if !safeWireText(facts.UnavailableReason, 128) || !safeWireText(facts.BaseCommit, 128) ||
		!safeWireText(facts.HeadCommit, 128) || !safeWireText(facts.Branch, task.MaxWorktreeBranchLength) ||
		!safeWireText(facts.WorkspaceFingerprint, 256) || facts.ChangedFiles < 0 || facts.Additions < 0 ||
		facts.Deletions < 0 || facts.BinaryFiles < 0 || facts.UntrackedFiles < 0 || facts.CommitCount < 0 {
		return false
	}
	return facts.ObservedAt.IsZero() || !facts.ObservedAt.After(time.Now().Add(24*time.Hour))
}

func validCheckReport(report session.CheckReportInfo) bool {
	if report.IsZero() {
		return true
	}
	return report.Source == session.CheckSourceReported &&
		(report.Status == session.CheckStatusPassed || report.Status == session.CheckStatusFailed) &&
		safeWireText(report.WorkspaceFingerprint, 256) && report.WorkspaceFingerprint != "" &&
		!report.ReportedAt.IsZero() && !report.ReportedAt.After(time.Now().Add(24*time.Hour))
}

func validExecutionPhase(phase task.ExecutionPhase) bool {
	switch phase {
	case task.ExecutionReserved, task.ExecutionProvisioning, task.ExecutionConfiguring, task.ExecutionStarting,
		task.ExecutionWaiting, task.ExecutionSubmitting, task.ExecutionSubmitted, task.ExecutionFailed, task.ExecutionInterrupted:
		return true
	default:
		return false
	}
}

func validSessionStatus(status session.Status) bool {
	switch status {
	case session.StatusCreating, session.StatusStopped, session.StatusRunning, session.StatusIdle,
		session.StatusThinking, session.StatusPermission, session.StatusDeleting:
		return true
	default:
		return false
	}
}

func safeSummaryText(value string) bool {
	return len(value) <= task.MaxRunMessageLength && !strings.ContainsAny(value, "\x00\r\n\x1b")
}

func safeWireText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && !strings.ContainsAny(value, "\x00\r\n\x1b")
}
