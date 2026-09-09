package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/jinenv"
)

// testIdentity is the jin a test's dispatch claims to come from. Every field is
// distinctive and none is derivable from this process, so a buildEnv that
// answers "which jin am I" for itself — with os.Executable(), with the live
// debug flag — renders something this file never named.
func testIdentity() jinenv.Identity {
	return jinenv.Identity{
		SocketPath: "/run/jin.sock",
		BinPath:    "/run/jin-test-only/bin/jin",
		Debug:      true,
	}
}

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
		PluginDir: pluginDir,
		Run:       run,
		ActionID:  "notify",
		Env:       sampleEvent(),
		Depth:     0,
		Identity:  testIdentity(),
		LogPath:   logPath,
		Timeout:   5 * time.Second,
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
		"JIN_ACTION_ID=notify",
		"JIN_PLUGIN_DEPTH=0",
		"JIN_SOCKET=" + testIdentity().SocketPath,
	}
	for _, want := range wantEnv {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %q; env:\n%s", want, env)
		}
	}
	// The unified manifest dropped the plugin-facing api_version concept;
	// make sure the env var is not re-introduced by accident.
	if strings.Contains(env, "JIN_PLUGIN_API_VERSION") {
		t.Errorf("JIN_PLUGIN_API_VERSION should not be exported anymore; env:\n%s", env)
	}
	// JIN_BIN is whatever the caller named, not a path this process resolves
	// for itself. Asserting the exact value is the point: os.Executable() here
	// would be the test binary, which is also a non-empty path.
	if want := "JIN_BIN=" + testIdentity().BinPath; !strings.Contains(env, want) {
		t.Errorf("env missing %q; env:\n%s", want, env)
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

func TestExecHandoff_CapturesBoundedJSONAndKey(t *testing.T) {
	requestFile := filepath.Join(t.TempDir(), "request.json")
	keyFile := filepath.Join(t.TempDir(), "key.txt")
	logPath := filepath.Join(t.TempDir(), "plugin.log")
	run := "cat > " + requestFile + "\nprintf '%s' \"$JIN_HANDOFF_KEY\" > " + keyFile + "\nprintf '{\"status\":\"succeeded\",\"id\":\"42\"}'\n"
	out, err := ExecHandoff(context.Background(), ExecOptions{
		PluginDir: t.TempDir(), Run: run, ActionID: "create", Env: sampleEvent(),
		HandoffKey: "stable-key", Identity: testIdentity(), LogPath: logPath,
		Timeout: time.Second,
	}, []byte(`{"kind":"pull-request"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != `{"status":"succeeded","id":"42"}` {
		t.Fatalf("stdout = %q", got)
	}
	request, err := os.ReadFile(requestFile)
	if err != nil || string(request) != `{"kind":"pull-request"}` {
		t.Fatalf("request = %q, %v", request, err)
	}
	key, err := os.ReadFile(keyFile)
	if err != nil || string(key) != "stable-key" {
		t.Fatalf("key = %q, %v", key, err)
	}
}

func TestExecHandoff_RejectsOversizedResult(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "plugin.log")
	run := "head -c 40000 /dev/zero | tr '\\0' x\n"
	_, err := ExecHandoff(context.Background(), ExecOptions{
		PluginDir: t.TempDir(), Run: run, Env: sampleEvent(), LogPath: logPath,
		Timeout: time.Second,
	}, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "result exceeds") {
		t.Fatalf("oversized result error = %v", err)
	}
}

func TestExecHandoff_TimeoutIsReported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := ExecHandoff(ctx, ExecOptions{
		PluginDir: t.TempDir(), Run: "sleep 5\n", Env: sampleEvent(),
		LogPath: filepath.Join(t.TempDir(), "plugin.log"), Timeout: 100 * time.Millisecond,
	}, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
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

// JIN_ACTION_ID follows the other JIN_* event vars: always exported, empty
// when the caller did not name an action (unlike JIN_CALLER_TMUX_*, which
// are omitted entirely when absent).
func TestBuildEnv_ExportsActionID(t *testing.T) {
	withID := strings.Join(buildEnv(ExecOptions{
		Env:      sampleEvent(),
		ActionID: "send-dm",
		Identity: testIdentity(),
	}), "\n")
	if !strings.Contains(withID, "JIN_ACTION_ID=send-dm") {
		t.Errorf("missing JIN_ACTION_ID=send-dm; env:\n%s", withID)
	}

	withoutID := strings.Join(buildEnv(ExecOptions{
		Env:      sampleEvent(),
		Identity: testIdentity(),
	}), "\n")
	if !strings.Contains(withoutID, "JIN_ACTION_ID=\n") {
		t.Errorf("JIN_ACTION_ID should be exported empty when unset; env:\n%s", withoutID)
	}
}

// TestBuildEnv_PassesTheDebugFlagToThePlugin pins the half of the callback
// identity a plugin used to be denied. A plugin reaches back into jind-ai
// through $JIN_BIN, and that process decides for itself whether to record what
// it does — so with the flag stopping at the daemon, running the daemon under
// it turned on only jind-ai's own side of the exchange. Measured: 3/3 runs
// where the daemon's log filled up and the callback's stayed empty, and the
// same callback with the flag forced on wrote its line.
//
// The flag arrives in the identity now, so the case is driven by the argument
// rather than by a package-level seam standing in for the real flag.
func TestBuildEnv_PassesTheDebugFlagToThePlugin(t *testing.T) {
	for _, tt := range []struct {
		name string
		on   bool
	}{
		{"on, so a plugin's own callback records too", true},
		{"off, so nothing is injected", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := testIdentity()
			id.Debug = tt.on

			env := strings.Join(buildEnv(ExecOptions{
				Env:      sampleEvent(),
				Identity: id,
			}), "\n")

			if got := strings.Contains(env, "JIN_DEBUG=1"); got != tt.on {
				t.Errorf("JIN_DEBUG=1 present = %v, want %v; env:\n%s", got, tt.on, env)
			}
		})
	}
}

func TestBuildEnv_ExportsPopupSize_WhenSet(t *testing.T) {
	env := buildEnv(ExecOptions{
		Env:         sampleEvent(),
		Identity:    testIdentity(),
		PopupWidth:  "40%",
		PopupHeight: "20%",
	})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "JIN_PLUGIN_POPUP_WIDTH=40%") {
		t.Errorf("missing JIN_PLUGIN_POPUP_WIDTH; env:\n%s", joined)
	}
	if !strings.Contains(joined, "JIN_PLUGIN_POPUP_HEIGHT=20%") {
		t.Errorf("missing JIN_PLUGIN_POPUP_HEIGHT; env:\n%s", joined)
	}
}

func TestBuildEnv_OmitsPopupSize_WhenEmpty(t *testing.T) {
	env := buildEnv(ExecOptions{
		Env:      sampleEvent(),
		Identity: testIdentity(),
	})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "JIN_PLUGIN_POPUP_WIDTH") {
		t.Errorf("unexpected JIN_PLUGIN_POPUP_WIDTH when unset; env:\n%s", joined)
	}
	if strings.Contains(joined, "JIN_PLUGIN_POPUP_HEIGHT") {
		t.Errorf("unexpected JIN_PLUGIN_POPUP_HEIGHT when unset; env:\n%s", joined)
	}
}

func TestBuildEnv_OmitsWidthOnlyWhenHeightUnset(t *testing.T) {
	env := buildEnv(ExecOptions{
		Env:        sampleEvent(),
		Identity:   testIdentity(),
		PopupWidth: "40%",
	})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "JIN_PLUGIN_POPUP_WIDTH=40%") {
		t.Errorf("missing JIN_PLUGIN_POPUP_WIDTH; env:\n%s", joined)
	}
	if strings.Contains(joined, "JIN_PLUGIN_POPUP_HEIGHT") {
		t.Errorf("unexpected JIN_PLUGIN_POPUP_HEIGHT when unset; env:\n%s", joined)
	}
}

// TestBuildEnv_ForwardsTheInheritedAllowlist covers the wiring, not the table:
// the allowlist lives in jinenv and is tested there. Dropping the call here
// left this package green, because the only coverage of a curated environment
// was on the build path (TestRunBuilds_EnvIsCuratedNotInherited), not dispatch.
func TestBuildEnv_ForwardsTheInheritedAllowlist(t *testing.T) {
	t.Setenv("PATH", "/pinned/by/the/test")

	env := strings.Join(buildEnv(ExecOptions{Env: sampleEvent(), Identity: testIdentity()}), "\n")

	if !strings.Contains(env, "PATH=/pinned/by/the/test") {
		t.Errorf("the plugin is not given the inherited PATH; env:\n%s", env)
	}
}
