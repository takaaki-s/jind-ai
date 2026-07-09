package plugin

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sampleEvent is a fully-populated Event used across exec tests.
func sampleEvent() Event {
	return Event{
		Name:       "status_changed",
		SessionID:  "sess-uuid-1",
		Status:     "idle",
		PrevStatus: "thinking",
		AgentKind:  "claude",
		WorkDir:    "/tmp/fake-work",
		TmuxPaneID: "%3",
		NotifyKind: "task-complete",
	}
}

func TestExecPlugin_Success(t *testing.T) {
	pluginDir := t.TempDir()
	envDump := filepath.Join(t.TempDir(), "env.txt")
	stdinDump := filepath.Join(t.TempDir(), "stdin.json")
	logPath := filepath.Join(t.TempDir(), "plugin.log")

	// Dump the injected env and piped stdin so the test can assert on both.
	run := "env > " + envDump + "\ncat > " + stdinDump + "\nexit 0\n"

	// Seed a parent env var that must NOT leak through the curated filter.
	t.Setenv("JIN_SHOULD_NOT_LEAK", "secret")

	err := ExecPlugin(context.Background(), ExecOptions{
		PluginDir:  pluginDir,
		Run:        run,
		Env:        sampleEvent(),
		APIVersion: 1,
		Depth:      0,
		SocketPath: "/run/jin.sock",
		LogPath:    logPath,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ExecPlugin: %v", err)
	}

	envBytes, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	env := string(envBytes)

	wantEnv := []string{
		"JIN_EVENT=status_changed",
		"JIN_SESSION_ID=sess-uuid-1",
		"JIN_STATUS=idle",
		"JIN_PREV_STATUS=thinking",
		"JIN_AGENT_KIND=claude",
		"JIN_WORKDIR=/tmp/fake-work",
		"JIN_TMUX_PANE_ID=%3",
		"JIN_NOTIFY_KIND=task-complete",
		"JIN_PLUGIN_API_VERSION=1",
		"JIN_PLUGIN_DEPTH=0",
		"JIN_SOCKET=/run/jin.sock",
	}
	for _, want := range wantEnv {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %q; env:\n%s", want, env)
		}
	}
	// JIN_BIN carries os.Executable() of the dispatching process — here the
	// test binary — so assert presence and non-emptiness, not an exact path.
	if !regexp.MustCompile(`(?m)^JIN_BIN=.+$`).MatchString(env) {
		t.Errorf("env missing non-empty JIN_BIN; env:\n%s", env)
	}
	if strings.Contains(env, "JIN_SHOULD_NOT_LEAK") {
		t.Errorf("curated env leaked JIN_SHOULD_NOT_LEAK; env:\n%s", env)
	}

	stdinBytes, err := os.ReadFile(stdinDump)
	if err != nil {
		t.Fatalf("read stdin dump: %v", err)
	}
	stdin := string(stdinBytes)
	wantJSON := []string{
		`"name":"status_changed"`,
		`"session_id":"sess-uuid-1"`,
		`"status":"idle"`,
		`"prev_status":"thinking"`,
		`"agent_kind":"claude"`,
		`"work_dir":"/tmp/fake-work"`,
		`"tmux_pane_id":"%3"`,
		`"notify_kind":"task-complete"`,
	}
	for _, want := range wantJSON {
		if !strings.Contains(stdin, want) {
			t.Errorf("stdin JSON missing %q; stdin:\n%s", want, stdin)
		}
	}
}

func TestExecPlugin_Failure(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "plugin.log")

	err := ExecPlugin(context.Background(), ExecOptions{
		PluginDir: t.TempDir(),
		Run:       "exit 7\n",
		Env:       sampleEvent(),
		LogPath:   logPath,
	})
	if err == nil {
		t.Fatal("ExecPlugin should return error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "exit status 7") {
		t.Errorf("error %q should mention exit status 7", err.Error())
	}
}

func TestExecPlugin_Timeout(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "plugin.log")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := ExecPlugin(ctx, ExecOptions{
		PluginDir: t.TempDir(),
		Run:       "sleep 5\n",
		Env:       sampleEvent(),
		LogPath:   logPath,
		Timeout:   200 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ExecPlugin should error on timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q should mention timeout", err.Error())
	}
	if elapsed >= 3*time.Second {
		t.Errorf("ExecPlugin took %s, expected to be cancelled early", elapsed)
	}
}

// TestExecPlugin_TimeoutKillsGroup verifies that when ctx times out, the run
// (a bash leader) *and* a background grandchild it spawned are both signalled.
// Without cmd.Cancel targeting the process group, exec.CommandContext would
// SIGKILL only the leader and the grandchild would keep running under init.
func TestExecPlugin_TimeoutKillsGroup(t *testing.T) {
	workDir := t.TempDir()
	pidFile := filepath.Join(workDir, "child.pid")
	// bash starts `sleep 30` in the background, records its PID, then waits.
	// SIGTERM to the group must kill sleep too — otherwise it stays alive.
	run := "sleep 30 & echo $! > " + pidFile + "\nwait\n"
	logPath := filepath.Join(t.TempDir(), "plugin.log")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := ExecPlugin(ctx, ExecOptions{
		PluginDir: workDir,
		Run:       run,
		Env:       sampleEvent(),
		LogPath:   logPath,
		Timeout:   300 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("ExecPlugin should error on timeout")
	}

	pidBytes, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("pidfile: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if convErr != nil {
		t.Fatalf("parse pid %q: %v", pidBytes, convErr)
	}

	// Give the OS a short window to reap the group. If the child survives past
	// this window, the group-signal fix did not take effect.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// syscall.Kill(pid, 0) returns ESRCH once the process has been reaped.
		if err := syscall.Kill(pid, 0); err != nil {
			return // Process gone — test passes.
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Best-effort cleanup so a failed test doesn't leak the sleep process.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("grandchild pid %d survived group signal", pid)
}

func TestExecPlugin_LogAppends(t *testing.T) {
	pluginDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "sub", "plugin.log")

	ev := sampleEvent()
	for i := 0; i < 2; i++ {
		err := ExecPlugin(context.Background(), ExecOptions{
			PluginDir: pluginDir,
			Run:       "echo run-output\n",
			Env:       ev,
			LogPath:   logPath,
		})
		if err != nil {
			t.Fatalf("ExecPlugin run %d: %v", i, err)
		}
	}

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(b)

	headers := strings.Count(log, "--- ")
	if headers != 2 {
		t.Errorf("expected 2 separator headers, got %d; log:\n%s", headers, log)
	}
	if !strings.Contains(log, "session=sess-uuid-1") {
		t.Errorf("header missing session marker; log:\n%s", log)
	}
	if outputs := strings.Count(log, "run-output"); outputs != 2 {
		t.Errorf("expected 2 command outputs, got %d; log:\n%s", outputs, log)
	}
}

func TestLogPath(t *testing.T) {
	got := LogPath("/state", "notifier")
	want := filepath.Join("/state", "plugin-logs", "notifier.log")
	if got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
}
