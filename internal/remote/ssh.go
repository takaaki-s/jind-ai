package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/takaaki-s/jind-ai/internal/procgroup"
)

type ErrorKind string

const (
	ErrorAuth       ErrorKind = "auth"
	ErrorTimeout    ErrorKind = "timeout"
	ErrorTransport  ErrorKind = "transport"
	ErrorProtocol   ErrorKind = "protocol"
	ErrorVersion    ErrorKind = "version"
	ErrorCapability ErrorKind = "capability"
	ErrorRemote     ErrorKind = "remote"
)

type CallError struct {
	Kind      ErrorKind
	Code      string
	Message   string
	Retryable bool
}

func (e *CallError) Error() string { return e.Message }

type Caller interface {
	Call(context.Context, Target, string, Operation, any, any) (HandshakeResponse, error)
}

type SSHClient struct {
	Command string
	Timeout time.Duration
}

func NewSSHClient() *SSHClient {
	return &SSHClient{Command: "ssh", Timeout: 30 * time.Second}
}

var (
	sshHostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@:-]{0,255}$`)
	jinPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

func ValidateTarget(target Target) error {
	if err := ValidateIdentifier("target id", target.ID); err != nil {
		return err
	}
	if target.Revision == "" {
		return fmt.Errorf("target revision is required")
	}
	if target.ExpectedServerInstanceID != "" && ValidateIdentifier("expected server instance id", target.ExpectedServerInstanceID) != nil {
		return fmt.Errorf("invalid expected server instance id")
	}
	if !sshHostPattern.MatchString(target.SSHHost) {
		return fmt.Errorf("invalid SSH host alias")
	}
	if target.JinPath != "jin" && !jinPathPattern.MatchString(target.JinPath) {
		return fmt.Errorf("jin path must be 'jin' or a safe absolute path")
	}
	return nil
}

func (c *SSHClient) Call(parent context.Context, target Target, controllerID string, operation Operation, payload any, result any) (HandshakeResponse, error) {
	if err := ValidateTarget(target); err != nil {
		return HandshakeResponse{}, &CallError{Kind: ErrorProtocol, Code: "invalid_target", Message: err.Error()}
	}
	if err := ValidateIdentifier("controller id", controllerID); err != nil {
		return HandshakeResponse{}, &CallError{Kind: ErrorProtocol, Code: "invalid_controller", Message: err.Error()}
	}
	capabilities, ok := operationCapabilities(operation)
	if !ok {
		return HandshakeResponse{}, &CallError{Kind: ErrorProtocol, Code: "unsupported_operation", Message: "unsupported remote operation"}
	}
	budget := c.Timeout
	if budget <= 0 {
		budget = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	commandName := c.Command
	if commandName == "" {
		commandName = "ssh"
	}
	args := []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no",
		"-o", "RequestTTY=no",
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=1",
		target.SSHHost, "--", target.JinPath, "remote", "serve", "--stdio",
	}
	cmd := procgroup.CommandContext(ctx, commandName, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return HandshakeResponse{}, transportError(ctx, nil, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return HandshakeResponse{}, transportError(ctx, nil, err)
	}
	diagnostics := &boundedDiagnostics{limit: MaxDiagnosticBytes}
	cmd.Stderr = diagnostics
	if err := cmd.Start(); err != nil {
		return HandshakeResponse{}, transportError(ctx, diagnostics.Bytes(), err)
	}
	finished := false
	defer func() {
		if !finished {
			cancel()
			_ = stdin.Close()
			_ = cmd.Wait()
		}
	}()
	finishFailedExchange := func(exchangeErr error) error {
		_ = stdin.Close()
		processErr := cmd.Wait()
		finished = true
		return classifyExchangeError(ctx, diagnostics.Bytes(), exchangeErr, processErr)
	}

	handshakeRequest := HandshakeRequest{
		MinimumVersion: ProtocolVersion, MaximumVersion: ProtocolVersion,
		RequiredCapabilities: capabilities,
	}
	var handshakeResponse HandshakeResponse
	if err := exchange(stdin, stdout, controllerID, OperationHandshake, handshakeRequest, &handshakeResponse); err != nil {
		return HandshakeResponse{}, finishFailedExchange(err)
	}
	if wireErr := ValidateHandshake(handshakeRequest, handshakeResponse); wireErr != nil {
		return HandshakeResponse{}, callErrorFromWire(wireErr)
	}
	if target.ExpectedServerInstanceID != "" && handshakeResponse.Server.InstanceID != target.ExpectedServerInstanceID {
		return HandshakeResponse{}, finishFailedExchange(&CallError{
			Kind: ErrorProtocol, Code: "server_identity_mismatch", Message: "remote server instance changed",
		})
	}
	if err := exchange(stdin, stdout, controllerID, operation, payload, result); err != nil {
		return handshakeResponse, finishFailedExchange(err)
	}
	switch operation {
	case OperationRepositoryPreflight:
		request, requestOK := payload.(PreflightRequest)
		response, responseOK := result.(*PreflightResponse)
		if !requestOK || !responseOK {
			return handshakeResponse, finishFailedExchange(&CallError{
				Kind: ErrorProtocol, Code: "invalid_response", Message: "invalid repository preflight result target",
			})
		}
		if wireErr := ValidatePreflight(request, *response); wireErr != nil {
			return handshakeResponse, finishFailedExchange(callErrorFromWire(wireErr))
		}
	case OperationExecutionStart:
		request, requestOK := payload.(StartRequest)
		response, responseOK := result.(*StartResponse)
		if !requestOK || !responseOK {
			return handshakeResponse, finishFailedExchange(&CallError{
				Kind: ErrorProtocol, Code: "invalid_response", Message: "invalid execution start result target",
			})
		}
		if wireErr := ValidateStartResponse(request, *response); wireErr != nil {
			return handshakeResponse, finishFailedExchange(callErrorFromWire(wireErr))
		}
	case OperationExecutionInspect:
		request, requestOK := payload.(InspectRequest)
		response, responseOK := result.(*InspectResponse)
		if !requestOK || !responseOK {
			return handshakeResponse, finishFailedExchange(&CallError{
				Kind: ErrorProtocol, Code: "invalid_response", Message: "invalid execution inspect result target",
			})
		}
		if wireErr := ValidateInspectResponse(request, *response); wireErr != nil {
			return handshakeResponse, finishFailedExchange(callErrorFromWire(wireErr))
		}
	case OperationExecutionCancel, OperationExecutionCleanup:
		request, requestOK := payload.(ExecutionOperationRequest)
		response, responseOK := result.(*ExecutionOperationResponse)
		if !requestOK || !responseOK {
			return handshakeResponse, finishFailedExchange(&CallError{
				Kind: ErrorProtocol, Code: "invalid_response", Message: "invalid execution operation result target",
			})
		}
		if wireErr := ValidateExecutionOperationResponseFor(operation, request, *response); wireErr != nil {
			return handshakeResponse, finishFailedExchange(callErrorFromWire(wireErr))
		}
	}
	if err := stdin.Close(); err != nil {
		return handshakeResponse, transportError(ctx, diagnostics.Bytes(), err)
	}
	if err := cmd.Wait(); err != nil {
		return handshakeResponse, transportError(ctx, diagnostics.Bytes(), err)
	}
	finished = true
	return handshakeResponse, nil
}

func operationCapabilities(operation Operation) ([]string, bool) {
	switch operation {
	case OperationRepositoryPreflight:
		return []string{CapabilityRepositoryPreflight}, true
	case OperationExecutionStart:
		return []string{CapabilityExecutionStart, CapabilityStructuredSummary}, true
	case OperationExecutionInspect:
		return []string{CapabilityExecutionInspect, CapabilityStructuredSummary}, true
	case OperationExecutionCancel:
		return []string{CapabilityExecutionCancel, CapabilityStructuredSummary}, true
	case OperationExecutionCleanup:
		return []string{CapabilityExecutionCleanup, CapabilityStructuredSummary}, true
	default:
		return nil, false
	}
}

func exchange(stdin io.Writer, stdout io.Reader, controllerID string, operation Operation, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req := Request{
		Protocol: ProtocolName, ProtocolVersion: ProtocolVersion,
		RequestID: "req-" + uuid.NewString(), ControllerID: controllerID,
		Operation: operation, Payload: data,
	}
	if err := WriteFrame(stdin, req); err != nil {
		return err
	}
	var resp Response
	if err := ReadFrame(stdout, &resp); err != nil {
		return err
	}
	if resp.Protocol != ProtocolName || resp.ProtocolVersion != ProtocolVersion ||
		resp.RequestID != req.RequestID || resp.Operation != operation {
		return &CallError{Kind: ErrorProtocol, Code: "invalid_response", Message: "remote response identity mismatch"}
	}
	if resp.Status == "error" {
		if resp.Error == nil || len(resp.Payload) != 0 {
			return &CallError{Kind: ErrorProtocol, Code: "invalid_response", Message: "malformed remote error response"}
		}
		return callErrorFromWire(resp.Error)
	}
	if resp.Status != "ok" || resp.Error != nil || len(resp.Payload) == 0 {
		return &CallError{Kind: ErrorProtocol, Code: "invalid_response", Message: "malformed remote response"}
	}
	if err := json.Unmarshal(resp.Payload, result); err != nil {
		return &CallError{Kind: ErrorProtocol, Code: "invalid_response", Message: "decode remote response"}
	}
	return nil
}

func callErrorFromWire(wireErr *WireError) *CallError {
	if wireErr == nil || ValidateIdentifier("remote error code", wireErr.Code) != nil {
		return &CallError{Kind: ErrorProtocol, Code: "invalid_response", Message: "remote returned an invalid error"}
	}
	kind := ErrorRemote
	switch wireErr.Code {
	case "unsupported_version":
		kind = ErrorVersion
	case "missing_capability":
		kind = ErrorCapability
	case "invalid_request", "invalid_response", "unsupported_operation", "identity_mismatch":
		kind = ErrorProtocol
	}
	message := sanitizeMessage(wireErr.Message)
	if message == "" {
		message = "remote operation failed"
	}
	return &CallError{Kind: kind, Code: wireErr.Code, Message: message, Retryable: wireErr.Retryable}
}

func sanitizeMessage(message string) string {
	message = strings.TrimSpace(message)
	var safe strings.Builder
	for _, r := range message {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		encoded := string(r)
		if safe.Len()+len(encoded) > 512 {
			break
		}
		safe.WriteString(encoded)
	}
	return strings.Join(strings.Fields(safe.String()), " ")
}

func classifyExchangeError(ctx context.Context, diagnostics []byte, exchangeErr, processErr error) error {
	var callErr *CallError
	if errors.As(exchangeErr, &callErr) {
		return callErr
	}
	if ctx.Err() != nil {
		return transportError(ctx, diagnostics, exchangeErr)
	}
	if errors.Is(exchangeErr, ErrFrameTooLarge) || errors.Is(exchangeErr, ErrInvalidFrame) ||
		errors.Is(exchangeErr, io.ErrUnexpectedEOF) {
		return &CallError{Kind: ErrorProtocol, Code: "invalid_response", Message: "remote returned a malformed or oversized frame"}
	}
	if processErr != nil {
		return transportError(ctx, diagnostics, processErr)
	}
	return transportError(ctx, diagnostics, exchangeErr)
}

func transportError(ctx context.Context, diagnostics []byte, err error) *CallError {
	if ctx.Err() != nil {
		return &CallError{Kind: ErrorTimeout, Code: "timeout", Message: "remote SSH operation timed out", Retryable: true}
	}
	detail := strings.ToLower(string(diagnostics))
	if strings.Contains(detail, "permission denied") || strings.Contains(detail, "host key verification failed") ||
		strings.Contains(detail, "no supported authentication methods") {
		return &CallError{Kind: ErrorAuth, Code: "authentication_failed", Message: "remote SSH authentication or host verification failed"}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
		return &CallError{Kind: ErrorTransport, Code: "ssh_unreachable", Message: "remote SSH connection failed", Retryable: true}
	}
	return &CallError{Kind: ErrorTransport, Code: "transport_failed", Message: "remote SSH transport failed", Retryable: true}
}

type boundedDiagnostics struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *boundedDiagnostics) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		_, _ = b.buf.Write(p[:min(remaining, n)])
	}
	return n, nil
}

func (b *boundedDiagnostics) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}
