package remote

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type fakeBackend struct {
	handshake HandshakeResponse
	preflight PreflightResponse
	preErr    *WireError
	seen      []string
}

func (b *fakeBackend) Handshake(HandshakeRequest) (HandshakeResponse, *WireError) {
	b.seen = append(b.seen, "handshake")
	return b.handshake, nil
}

func (b *fakeBackend) Preflight(controllerID string, _ PreflightRequest) (PreflightResponse, *WireError) {
	b.seen = append(b.seen, controllerID)
	return b.preflight, b.preErr
}

func validHandshake() HandshakeResponse {
	return HandshakeResponse{
		SelectedVersion: ProtocolVersion,
		Server: ServerIdentity{
			InstanceID: "srv-test", BootID: "boot-test", JinVersion: "0.1.0+test",
		},
		Capabilities: []string{CapabilityRepositoryPreflight},
		Limits: Limits{
			MaxFrameBytes: MaxFrameBytes, MaxPromptBytes: 1024, MaxDiagnosticBytes: MaxDiagnosticBytes,
		},
	}
}

func request(t *testing.T, id, controller string, operation Operation, payload any) Request {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return Request{
		Protocol: ProtocolName, ProtocolVersion: ProtocolVersion, RequestID: id,
		ControllerID: controller, Operation: operation, Payload: data,
	}
}

func runServer(t *testing.T, backend Backend, requests ...Request) []Response {
	t.Helper()
	var input, output bytes.Buffer
	for _, req := range requests {
		if err := WriteFrame(&input, req); err != nil {
			t.Fatal(err)
		}
	}
	if err := Serve(&input, &output, backend); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	responses := make([]Response, 0, len(requests))
	for range requests {
		var response Response
		if err := ReadFrame(&output, &response); err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
	}
	return responses
}

func TestServeNegotiatesThenPreflights(t *testing.T) {
	backend := &fakeBackend{
		handshake: validHandshake(),
		preflight: PreflightResponse{
			RepositoryID: "jind-ai", RepositoryIdentity: "sha256:test", DefaultBranch: "main",
			AvailableAgentKinds: []string{"claude"},
		},
	}
	responses := runServer(t, backend,
		request(t, "req-handshake", "ctl-test", OperationHandshake, HandshakeRequest{
			MinimumVersion: ProtocolVersion, MaximumVersion: ProtocolVersion,
			RequiredCapabilities: []string{CapabilityRepositoryPreflight},
		}),
		request(t, "req-preflight", "ctl-test", OperationRepositoryPreflight, PreflightRequest{RepositoryID: "jind-ai"}),
	)
	for i, response := range responses {
		if response.Status != "ok" || response.Error != nil {
			t.Fatalf("response %d = %+v", i, response)
		}
	}
	if len(backend.seen) != 2 || backend.seen[1] != "ctl-test" {
		t.Fatalf("backend calls = %#v", backend.seen)
	}
}

func TestServeRequiresHandshakeAndBindsController(t *testing.T) {
	backend := &fakeBackend{handshake: validHandshake()}
	preflight := request(t, "req-preflight", "ctl-other", OperationRepositoryPreflight, PreflightRequest{RepositoryID: "jind-ai"})
	t.Run("handshake required", func(t *testing.T) {
		response := runServer(t, backend, preflight)[0]
		if response.Error == nil || response.Error.Code != "identity_mismatch" {
			t.Fatalf("response = %+v", response)
		}
	})
	t.Run("controller fixed after handshake", func(t *testing.T) {
		responses := runServer(t, backend,
			request(t, "req-handshake", "ctl-test", OperationHandshake, HandshakeRequest{
				MinimumVersion: 1, MaximumVersion: 1, RequiredCapabilities: []string{CapabilityRepositoryPreflight},
			}),
			preflight,
		)
		if responses[1].Error == nil || responses[1].Error.Code != "identity_mismatch" {
			t.Fatalf("response = %+v", responses[1])
		}
	})
}

func TestServeRejectsUnknownPayloadFields(t *testing.T) {
	req := request(t, "req-handshake", "ctl-test", OperationHandshake, map[string]any{
		"minimum_version": 1, "maximum_version": 1, "required_capabilities": []string{}, "unexpected": true,
	})
	response := runServer(t, &fakeBackend{handshake: validHandshake()}, req)[0]
	if response.Error == nil || response.Error.Code != "invalid_request" {
		t.Fatalf("response = %+v", response)
	}
}

func TestServePreservesBackendError(t *testing.T) {
	backend := &fakeBackend{
		handshake: validHandshake(),
		preErr:    NewWireError("repository_not_found", "remote repository is not enabled", false),
	}
	responses := runServer(t, backend,
		request(t, "req-handshake", "ctl-test", OperationHandshake, HandshakeRequest{
			MinimumVersion: 1, MaximumVersion: 1, RequiredCapabilities: []string{CapabilityRepositoryPreflight},
		}),
		request(t, "req-preflight", "ctl-test", OperationRepositoryPreflight, PreflightRequest{RepositoryID: "missing"}),
	)
	if responses[1].Error == nil || responses[1].Error.Code != "repository_not_found" {
		t.Fatalf("response = %+v", responses[1])
	}
}

func TestValidateHandshakeFailsClosed(t *testing.T) {
	request := HandshakeRequest{
		MinimumVersion: 1, MaximumVersion: 1, RequiredCapabilities: []string{CapabilityRepositoryPreflight},
	}
	tests := []struct {
		name string
		edit func(*HandshakeResponse)
		code string
	}{
		{name: "version", edit: func(response *HandshakeResponse) { response.SelectedVersion = 2 }, code: "unsupported_version"},
		{name: "capability", edit: func(response *HandshakeResponse) { response.Capabilities = nil }, code: "missing_capability"},
		{name: "identity", edit: func(response *HandshakeResponse) { response.Server.BootID = "" }, code: "invalid_response"},
		{name: "version text", edit: func(response *HandshakeResponse) { response.Server.JinVersion = "bad\x1bversion" }, code: "invalid_response"},
		{name: "capability text", edit: func(response *HandshakeResponse) { response.Capabilities = []string{"bad capability"} }, code: "invalid_response"},
		{name: "limit", edit: func(response *HandshakeResponse) { response.Limits.MaxFrameBytes = MaxFrameBytes + 1 }, code: "invalid_response"},
		{name: "prompt limit", edit: func(response *HandshakeResponse) { response.Limits.MaxPromptBytes = MaxFrameBytes + 1 }, code: "invalid_response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validHandshake()
			test.edit(&response)
			if wireErr := ValidateHandshake(request, response); wireErr == nil || wireErr.Code != test.code {
				t.Fatalf("ValidateHandshake = %+v", wireErr)
			}
		})
	}
}

func TestValidatePreflightRejectsUnboundOrUnsafeProjection(t *testing.T) {
	request := PreflightRequest{RepositoryID: "jind-ai"}
	valid := PreflightResponse{
		RepositoryID: "jind-ai", RepositoryIdentity: "sha256:" + strings.Repeat("a", 64),
		DefaultBranch: "feature/remote", AvailableAgentKinds: []string{"claude", "codex"},
	}
	if wireErr := ValidatePreflight(request, valid); wireErr != nil {
		t.Fatalf("ValidatePreflight(valid) = %+v", wireErr)
	}
	tests := []func(*PreflightResponse){
		func(response *PreflightResponse) { response.RepositoryID = "other" },
		func(response *PreflightResponse) { response.RepositoryIdentity = "sha256:test" },
		func(response *PreflightResponse) { response.DefaultBranch = "main\x1b[2J" },
		func(response *PreflightResponse) { response.DefaultBranch = "../main" },
		func(response *PreflightResponse) { response.AvailableAgentKinds = []string{"bad kind"} },
	}
	for i, edit := range tests {
		response := valid
		edit(&response)
		if wireErr := ValidatePreflight(request, response); wireErr == nil || wireErr.Code != "invalid_response" {
			t.Fatalf("case %d: ValidatePreflight = %+v", i, wireErr)
		}
	}
}
