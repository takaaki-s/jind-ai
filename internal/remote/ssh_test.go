package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSSHClientHelperProcess(t *testing.T) {
	if os.Getenv("JIN_REMOTE_HELPER") != "1" {
		return
	}
	backend := &fakeBackend{
		handshake: validHandshake(),
		preflight: PreflightResponse{
			RepositoryID: "jind-ai", RepositoryIdentity: "sha256:" + strings.Repeat("a", 64), DefaultBranch: "main",
			AvailableAgentKinds: []string{"claude", "codex"},
		},
	}
	if err := Serve(os.Stdin, os.Stdout, backend); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestSSHClientCall(t *testing.T) {
	script := writeExecutable(t, `#!/bin/sh
case "$*" in
  *"-T -o BatchMode=yes -o ForwardAgent=no -o ClearAllForwardings=yes -o PermitLocalCommand=no -o RequestTTY=no -o ConnectTimeout=10 -o ConnectionAttempts=1 test-host -- /opt/jin remote serve --stdio"*) ;;
  *) echo "unsafe or incomplete ssh arguments" >&2; exit 90 ;;
esac
exec "$JIN_REMOTE_TEST_BINARY" -test.run '^TestSSHClientHelperProcess$'
`)
	t.Setenv("JIN_REMOTE_HELPER", "1")
	t.Setenv("JIN_REMOTE_TEST_BINARY", os.Args[0])
	client := &SSHClient{Command: script, Timeout: 5 * time.Second}
	var got PreflightResponse
	handshake, err := client.Call(context.Background(), Target{
		ID: "build", Revision: "sha256:target", SSHHost: "test-host", JinPath: "/opt/jin",
	}, "ctl-test", OperationRepositoryPreflight, PreflightRequest{RepositoryID: "jind-ai"}, &got)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if handshake.Server.InstanceID != "srv-test" || got.RepositoryID != "jind-ai" || got.DefaultBranch != "main" {
		t.Fatalf("Call = %+v, %+v", handshake, got)
	}
}

func TestSSHClientClassifiesAuthenticationWithoutLeakingDiagnostics(t *testing.T) {
	script := writeExecutable(t, "#!/bin/sh\necho 'Permission denied for secret-user@example' >&2\nexit 255\n")
	client := &SSHClient{Command: script, Timeout: time.Second}
	var got PreflightResponse
	_, err := client.Call(context.Background(), validTarget(), "ctl-test",
		OperationRepositoryPreflight, PreflightRequest{RepositoryID: "jind-ai"}, &got)
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Kind != ErrorAuth || callErr.Code != "authentication_failed" {
		t.Fatalf("Call error = %#v", err)
	}
	if strings.Contains(callErr.Error(), "secret-user") {
		t.Fatalf("diagnostic leaked: %q", callErr.Error())
	}
}

func TestSSHClientBoundsTotalOperationTime(t *testing.T) {
	script := writeExecutable(t, "#!/bin/sh\nsleep 10\n")
	client := &SSHClient{Command: script, Timeout: 50 * time.Millisecond}
	var got PreflightResponse
	started := time.Now()
	_, err := client.Call(context.Background(), validTarget(), "ctl-test",
		OperationRepositoryPreflight, PreflightRequest{RepositoryID: "jind-ai"}, &got)
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Kind != ErrorTimeout {
		t.Fatalf("Call error = %#v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
}

func TestValidateTargetRejectsShellSyntax(t *testing.T) {
	tests := []Target{
		{ID: "", Revision: "rev", SSHHost: "host", JinPath: "jin"},
		{ID: "target", Revision: "", SSHHost: "host", JinPath: "jin"},
		{ID: "target", Revision: "rev", SSHHost: "host;touch", JinPath: "jin"},
		{ID: "target", Revision: "rev", SSHHost: "host", JinPath: "jin --unsafe"},
		{ID: "target", Revision: "rev", SSHHost: "host", JinPath: "/opt/jin;touch"},
	}
	for _, target := range tests {
		if err := ValidateTarget(target); err == nil {
			t.Fatalf("ValidateTarget(%+v) succeeded", target)
		}
	}
}

func TestBoundedDiagnostics(t *testing.T) {
	diagnostics := &boundedDiagnostics{limit: 4}
	if n, err := diagnostics.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := string(diagnostics.Bytes()); got != "abcd" {
		t.Fatalf("Bytes = %q", got)
	}
}

func TestCallErrorFromWireSanitizesUntrustedMessage(t *testing.T) {
	err := callErrorFromWire(&WireError{Code: "repository_not_found", Message: "secret\x1b[2J\nnext"})
	if err.Kind != ErrorRemote || err.Message != "secret [2J next" {
		t.Fatalf("callErrorFromWire = %+v", err)
	}
	invalid := callErrorFromWire(&WireError{Code: "bad code", Message: "ignored"})
	if invalid.Kind != ErrorProtocol || invalid.Code != "invalid_response" {
		t.Fatalf("callErrorFromWire(invalid) = %+v", invalid)
	}
}

func validTarget() Target {
	return Target{ID: "build", Revision: "sha256:target", SSHHost: "test-host", JinPath: "jin"}
}

func writeExecutable(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ssh")
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
