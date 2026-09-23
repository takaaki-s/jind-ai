package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/remote"
)

type recordingRemoteCaller struct {
	target       remote.Target
	controllerID string
	operation    remote.Operation
	request      remote.PreflightRequest
	handshake    remote.HandshakeResponse
	preflight    remote.PreflightResponse
	err          error
}

func (c *recordingRemoteCaller) Call(_ context.Context, target remote.Target, controllerID string,
	operation remote.Operation, payload any, result any,
) (remote.HandshakeResponse, error) {
	c.target = target
	c.controllerID = controllerID
	c.operation = operation
	c.request = payload.(remote.PreflightRequest)
	if c.err != nil {
		return remote.HandshakeResponse{}, c.err
	}
	*(result.(*remote.PreflightResponse)) = c.preflight
	return c.handshake, nil
}

func TestRemoteTargetPreflightResolvesConfigAndReturnsBoundIdentities(t *testing.T) {
	server := newRemoteTestServer(t, `remote:
  targets:
    build:
      ssh_host: build-host
      jin_path: /opt/jin
      repositories:
        jind-ai: product
`)
	caller := &recordingRemoteCaller{
		handshake: remote.HandshakeResponse{
			Server:       remote.ServerIdentity{InstanceID: "srv-test", BootID: "boot-test", JinVersion: "test"},
			Capabilities: []string{remote.CapabilityRepositoryPreflight},
		},
		preflight: remote.PreflightResponse{
			RepositoryID: "product", RepositoryIdentity: "sha256:repo", DefaultBranch: "main",
		},
	}
	server.remoteCaller = caller
	data, _ := json.Marshal(RemoteTargetPreflightRequest{Target: "build", Repository: "jind-ai"})
	response := server.handleRequest(&Request{Action: "remote-preflight", Data: data})
	if !response.Success {
		t.Fatalf("preflight: %s", response.Error)
	}
	var got remote.TargetPreflight
	if err := json.Unmarshal(response.Data, &got); err != nil {
		t.Fatal(err)
	}
	if caller.target.SSHHost != "build-host" || caller.target.JinPath != "/opt/jin" ||
		caller.target.Revision == "" || caller.request.RepositoryID != "product" ||
		caller.operation != remote.OperationRepositoryPreflight {
		t.Fatalf("caller = %+v", caller)
	}
	if !strings.HasPrefix(caller.controllerID, "ctl-") || got.TargetID != "build" ||
		got.TargetRevision != caller.target.Revision || got.Repository.RepositoryIdentity != "sha256:repo" {
		t.Fatalf("result = %+v; controller = %q", got, caller.controllerID)
	}
}

func TestRemoteTargetPreflightDoesNotExposeUnexpectedTransportErrors(t *testing.T) {
	server := newRemoteTestServer(t, `remote:
  targets:
    build:
      ssh_host: build-host
      repositories:
        jind-ai: product
`)
	server.remoteCaller = &recordingRemoteCaller{err: errors.New("secret transport detail")}
	data, _ := json.Marshal(RemoteTargetPreflightRequest{Target: "build", Repository: "jind-ai"})
	response := server.handleRemoteTargetPreflight(data)
	if response.Success || response.Error != "remote transport failed" {
		t.Fatalf("response = %+v", response)
	}
}

func TestRemoteBackendServingIsOptIn(t *testing.T) {
	server := newRemoteTestServer(t, "")
	data, _ := json.Marshal(remote.HandshakeRequest{MinimumVersion: 1, MaximumVersion: 1})
	response := server.handleRemoteBackendHandshake(data)
	if !response.Success {
		t.Fatalf("IPC response = %+v", response)
	}
	var got remoteBackendHandshakeResult
	if err := json.Unmarshal(response.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || got.Error.Code != "serving_disabled" {
		t.Fatalf("handshake = %+v", got)
	}
}

func TestRemoteBackendHandshakeAndRepositoryPreflight(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
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

	handshakeData, _ := json.Marshal(remote.HandshakeRequest{MinimumVersion: 1, MaximumVersion: 1})
	handshakeResponse := server.handleRemoteBackendHandshake(handshakeData)
	var handshake remoteBackendHandshakeResult
	if err := json.Unmarshal(handshakeResponse.Data, &handshake); err != nil {
		t.Fatal(err)
	}
	if handshake.Error != nil || handshake.Value.Server.InstanceID == "" ||
		handshake.Value.Server.BootID != server.remoteBootID ||
		len(handshake.Value.Capabilities) != 1 || handshake.Value.Capabilities[0] != remote.CapabilityRepositoryPreflight {
		t.Fatalf("handshake = %+v", handshake)
	}

	preflightData, _ := json.Marshal(remoteBackendPreflightRequest{
		ControllerID: "ctl-test", Request: remote.PreflightRequest{RepositoryID: "product"},
	})
	preflightResponse := server.handleRemoteBackendPreflight(preflightData)
	var preflight remoteBackendPreflightResult
	if err := json.Unmarshal(preflightResponse.Data, &preflight); err != nil {
		t.Fatal(err)
	}
	if preflight.Error != nil || preflight.Value.RepositoryID != "product" ||
		preflight.Value.RepositoryIdentity == "" || preflight.Value.DefaultBranch != "main" {
		t.Fatalf("preflight = %+v", preflight)
	}

	second := server.handleRemoteBackendHandshake(handshakeData)
	var secondHandshake remoteBackendHandshakeResult
	if err := json.Unmarshal(second.Data, &secondHandshake); err != nil {
		t.Fatal(err)
	}
	if secondHandshake.Value.Server.InstanceID != handshake.Value.Server.InstanceID {
		t.Fatal("server instance identity changed during one daemon lifetime")
	}
}

func TestRemoteBackendPreflightUsesRepositoryAllowlist(t *testing.T) {
	server := newRemoteTestServer(t, `remote:
  serve:
    enabled: true
    repositories: {}
`)
	data, _ := json.Marshal(remoteBackendPreflightRequest{
		ControllerID: "ctl-test", Request: remote.PreflightRequest{RepositoryID: "missing"},
	})
	response := server.handleRemoteBackendPreflight(data)
	var got remoteBackendPreflightResult
	if err := json.Unmarshal(response.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || got.Error.Code != "repository_not_found" {
		t.Fatalf("preflight = %+v", got)
	}
}

func newRemoteTestServer(t *testing.T, configBody string) *Server {
	t.Helper()
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if configBody != "" {
		if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server, err := NewServer(filepath.Join(dir, "daemon.sock"), filepath.Join(dir, "sessions"), configDir, filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server
}
