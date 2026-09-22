package remotecontract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/procgroup"
)

// TestSSHControlHarness is opt-in because it binds a loopback port and starts
// an isolated sshd. It never reads the user's keys, agent, or known_hosts.
func TestSSHControlHarness(t *testing.T) {
	if os.Getenv("JIN_REMOTE_CONTRACT_SSH") != "1" {
		t.Skip("set JIN_REMOTE_CONTRACT_SSH=1 to run the localhost SSH measurement")
	}

	ssh := requireExecutable(t, "ssh")
	sshd := requireExecutable(t, "sshd")
	sshKeygen := requireExecutable(t, "ssh-keygen")
	currentUser, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	hostKey := filepath.Join(dir, "host_ed25519")
	clientKey := filepath.Join(dir, "client_ed25519")
	runCommand(t, sshKeygen, "-q", "-t", "ed25519", "-N", "", "-f", hostKey)
	runCommand(t, sshKeygen, "-q", "-t", "ed25519", "-N", "", "-f", clientKey)

	authorizedKey, err := os.ReadFile(clientKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	authorizedKeys := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(authorizedKeys, authorizedKey, 0o600); err != nil {
		t.Fatal(err)
	}

	port := reserveLoopbackPort(t)
	hostPublicKey, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(dir, "known_hosts")
	knownHostLine := fmt.Sprintf("[127.0.0.1]:%d %s", port, string(hostPublicKey))
	if err := os.WriteFile(knownHosts, []byte(knownHostLine), 0o600); err != nil {
		t.Fatal(err)
	}

	config := filepath.Join(dir, "sshd_config")
	configBody := strings.Join([]string{
		"AddressFamily inet",
		"ListenAddress 127.0.0.1",
		"Port " + strconv.Itoa(port),
		"HostKey " + hostKey,
		"PidFile " + filepath.Join(dir, "sshd.pid"),
		"AuthorizedKeysFile " + authorizedKeys,
		"PubkeyAuthentication yes",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"ChallengeResponseAuthentication no",
		"UsePAM no",
		"StrictModes no",
		"PermitRootLogin yes",
		"LogLevel ERROR",
	}, "\n") + "\n"
	if err := os.WriteFile(config, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, sshd, "-t", "-f", config)

	var serverLog bytes.Buffer
	server := exec.Command(sshd, "-D", "-e", "-f", config)
	server.Stderr = &serverLog
	server.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if server.Process != nil {
			_ = syscall.Kill(-server.Process.Pid, syscall.SIGTERM)
			_ = server.Wait()
		}
	})
	waitForSSHListener(t, port, server, &serverLog)

	baseArgs := []string{
		"-F", "/dev/null", "-T", "-p", strconv.Itoa(port), "-i", clientKey,
		"-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no",
		"-o", "RequestTTY=no",
		"-o", "ConnectTimeout=5",
		"-o", "ConnectionAttempts=1",
		currentUser.Username + "@127.0.0.1", "--",
	}

	first := measureSSHCommand(t, ssh, baseArgs, "printf remote-contract-ok")
	second := measureSSHCommand(t, ssh, baseArgs, "printf remote-contract-ok")

	failureStart := time.Now()
	stdout, stderr, failureErr := sshCommand(context.Background(), ssh, baseArgs,
		"sh -c 'printf stdout-marker; printf stderr-marker >&2; exit 42'")
	failureDuration := time.Since(failureStart)
	var exitErr *exec.ExitError
	if !errors.As(failureErr, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("remote failure = %v, stdout=%q stderr=%q", failureErr, stdout, stderr)
	}
	if stdout != "stdout-marker" || stderr != "stderr-marker" {
		t.Fatalf("stdio was not separated: stdout=%q stderr=%q", stdout, stderr)
	}

	cancelContext, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	cancelStart := time.Now()
	_, _, cancelErr := sshCommand(cancelContext, ssh, baseArgs, "sleep 30")
	cancelDuration := time.Since(cancelStart)
	if cancelContext.Err() != context.DeadlineExceeded || cancelErr == nil || cancelDuration > procgroup.TeardownBudget+time.Second {
		t.Fatalf("bounded cancellation failed: context=%v command=%v duration=%s", cancelContext.Err(), cancelErr, cancelDuration)
	}

	measurement := struct {
		FirstStartupMS    int64 `json:"first_startup_ms"`
		ReconnectMS       int64 `json:"reconnect_ms"`
		RemoteFailureMS   int64 `json:"remote_failure_ms"`
		CancellationMS    int64 `json:"cancellation_ms"`
		RemoteExitStatus  int   `json:"remote_exit_status"`
		StdoutStderrSplit bool  `json:"stdout_stderr_split"`
	}{
		FirstStartupMS: first.Milliseconds(), ReconnectMS: second.Milliseconds(),
		RemoteFailureMS: failureDuration.Milliseconds(), CancellationMS: cancelDuration.Milliseconds(),
		RemoteExitStatus: exitErr.ExitCode(), StdoutStderrSplit: true,
	}
	encoded, err := json.Marshal(measurement)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("remote_contract_measurement=%s", encoded)
}

func measureSSHCommand(t *testing.T, ssh string, baseArgs []string, remoteCommand string) time.Duration {
	t.Helper()
	started := time.Now()
	stdout, stderr, err := sshCommand(context.Background(), ssh, baseArgs, remoteCommand)
	duration := time.Since(started)
	if err != nil || stdout != "remote-contract-ok" || stderr != "" {
		t.Fatalf("SSH control command: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	return duration
}

func sshCommand(ctx context.Context, ssh string, baseArgs []string, remoteCommand string) (string, string, error) {
	args := append(append([]string{}, baseArgs...), remoteCommand)
	command := procgroup.CommandContext(ctx, ssh, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func requireExecutable(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("%s is required for the SSH control harness: %v", name, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func runCommand(t *testing.T, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
}

func reserveLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func waitForSSHListener(t *testing.T, port int, server *exec.Cmd, serverLog *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp4", address, 50*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		if server.ProcessState != nil && server.ProcessState.Exited() {
			t.Fatalf("sshd exited before listening: %s", serverLog.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("sshd did not listen on %s: %s", address, serverLog.String())
}
