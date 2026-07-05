package worktreehook

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newRunnerAt loads a Runner backed by an allowlist in stateDir. Used by
// tests that need to seed or inspect the allowlist directly.
func newRunnerAt(t *testing.T, stateDir string) *runner {
	t.Helper()
	r, err := NewRunner(stateDir)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r.(*runner)
}

// writeScript writes a bash script under <repoRoot>/.jin/worktree-post-create.sh
// with the given body. Returns the script path.
func writeScript(t *testing.T, repoRoot, body string) string {
	t.Helper()
	dir := filepath.Join(repoRoot, ".jin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .jin: %v", err)
	}
	scriptPath := filepath.Join(dir, "worktree-post-create.sh")
	if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return scriptPath
}

func TestRunner_Discover(t *testing.T) {
	r := newRunnerAt(t, t.TempDir())

	repo := t.TempDir()
	if _, ok := r.Discover(repo); ok {
		t.Error("Discover should return false when script is missing")
	}

	scriptPath := writeScript(t, repo, "echo hi\n")
	got, ok := r.Discover(repo)
	if !ok {
		t.Fatal("Discover should return true when script exists")
	}
	if got != scriptPath {
		t.Errorf("Discover path = %q, want %q", got, scriptPath)
	}
}

func TestRunner_Verify(t *testing.T) {
	state := t.TempDir()
	r := newRunnerAt(t, state)

	repo := t.TempDir()
	scriptPath := writeScript(t, repo, "echo v1\n")

	// Not allowed yet.
	got, err := r.Verify(scriptPath, repo)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != VerdictNotAllowed {
		t.Errorf("Verify (unallowed) = %v, want VerdictNotAllowed", got)
	}

	// Allow with the current SHA.
	sha, err := ComputeSHA256(scriptPath)
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}
	if err := r.allowlist.Allow(repo, sha); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	got, err = r.Verify(scriptPath, repo)
	if err != nil {
		t.Fatalf("Verify (allowed): %v", err)
	}
	if got != VerdictOK {
		t.Errorf("Verify (allowed) = %v, want VerdictOK", got)
	}

	// Mutate the script — SHA drift.
	if err := os.WriteFile(scriptPath, []byte("echo v2\n"), 0o644); err != nil {
		t.Fatalf("rewrite script: %v", err)
	}
	got, err = r.Verify(scriptPath, repo)
	if err != nil {
		t.Fatalf("Verify (changed): %v", err)
	}
	if got != VerdictChanged {
		t.Errorf("Verify (changed) = %v, want VerdictChanged", got)
	}
}

func TestRunner_Run_Success(t *testing.T) {
	r := newRunnerAt(t, t.TempDir())

	workDir := t.TempDir()
	scriptPath := writeScript(t, t.TempDir(), "exit 0\n")
	logPath := filepath.Join(t.TempDir(), "hook.log")

	err := r.Run(context.Background(), RunOptions{
		ScriptPath:   scriptPath,
		WorktreePath: workDir,
		LogPath:      logPath,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("log file should exist: %v", err)
	}
}

func TestRunner_Run_Failure(t *testing.T) {
	r := newRunnerAt(t, t.TempDir())

	workDir := t.TempDir()
	scriptPath := writeScript(t, t.TempDir(), "exit 7\n")
	logPath := filepath.Join(t.TempDir(), "hook.log")

	err := r.Run(context.Background(), RunOptions{
		ScriptPath:   scriptPath,
		WorktreePath: workDir,
		LogPath:      logPath,
	})
	if err == nil {
		t.Fatal("Run should return error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "exit status 7") {
		t.Errorf("error %q should mention exit status 7", err.Error())
	}
}

func TestRunner_Run_Timeout(t *testing.T) {
	r := newRunnerAt(t, t.TempDir())

	workDir := t.TempDir()
	scriptPath := writeScript(t, t.TempDir(), "sleep 5\n")
	logPath := filepath.Join(t.TempDir(), "hook.log")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := r.Run(ctx, RunOptions{
		ScriptPath:   scriptPath,
		WorktreePath: workDir,
		LogPath:      logPath,
		Timeout:      200 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run should error on timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q should mention timeout", err.Error())
	}
	if elapsed >= 3*time.Second {
		t.Errorf("Run took %s, expected to be cancelled early", elapsed)
	}
}

// TestRunner_Run_TimeoutKillsGroup verifies that when ctx times out, the hook
// (a bash leader) *and* a background grandchild it spawned are both signalled.
// Without cmd.Cancel targeting the process group, exec.CommandContext would
// SIGKILL only the leader and the grandchild would keep running under init.
//
// The test uses the child's own PID file plus a poll loop to detect exit — we
// deliberately avoid `kill -0 <pid>` because reparented processes can survive
// briefly before init reaps them.
func TestRunner_Run_TimeoutKillsGroup(t *testing.T) {
	r := newRunnerAt(t, t.TempDir())

	workDir := t.TempDir()
	pidFile := filepath.Join(workDir, "child.pid")
	// bash starts `sleep 30` in the background, records its PID, then waits.
	// SIGTERM to the group must kill sleep too — otherwise it stays alive.
	script := "sleep 30 & echo $! > " + pidFile + "\nwait\n"
	scriptPath := writeScript(t, t.TempDir(), script)
	logPath := filepath.Join(t.TempDir(), "hook.log")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := r.Run(ctx, RunOptions{
		ScriptPath:   scriptPath,
		WorktreePath: workDir,
		LogPath:      logPath,
		Timeout:      300 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run should error on timeout")
	}

	// Read the PID the script recorded before it was killed.
	pidBytes, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("pidfile: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if convErr != nil {
		t.Fatalf("parse pid %q: %v", pidBytes, convErr)
	}

	// Give the OS a short window to reap the group. If the child survives
	// past this window, the group-signal fix did not take effect.
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

// TestRunner_Run_TimeoutSigKillsGroup verifies the SIGKILL escalation: if the
// hook's process group ignores SIGTERM (via bash trap on TERM), the runner
// must still take it down within killGracePeriod. Without an explicit group-
// wide SIGKILL, exec.CommandContext / Cmd.WaitDelay only SIGKILLs the leader,
// and the grandchild survives — this test would then time out at the outer
// deadline.
func TestRunner_Run_TimeoutSigKillsGroup(t *testing.T) {
	r := newRunnerAt(t, t.TempDir())

	workDir := t.TempDir()
	pidFile := filepath.Join(workDir, "child.pid")
	// bash traps SIGTERM (ignores it) and starts `sleep 60` in the
	// background. If SIGKILL is not delivered to the whole group after the
	// grace period, sleep survives past the test deadline.
	script := "trap '' TERM\nsleep 60 & echo $! > " + pidFile + "\nwait\n"
	scriptPath := writeScript(t, t.TempDir(), script)
	logPath := filepath.Join(t.TempDir(), "hook.log")

	// Outer deadline must exceed killGracePeriod (5s) so the SIGKILL path
	// actually fires within Run — otherwise cmd.Wait would return on ctx
	// timeout before AfterFunc gets a chance to run.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := r.Run(ctx, RunOptions{
		ScriptPath:   scriptPath,
		WorktreePath: workDir,
		LogPath:      logPath,
		Timeout:      300 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Run should error on timeout")
	}
	// Run should return once cmd.Wait unblocks after SIGKILL — well within
	// killGracePeriod + a small margin.
	if elapsed >= 10*time.Second {
		t.Errorf("Run took %s, expected SIGKILL escalation within killGracePeriod", elapsed)
	}

	pidBytes, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("pidfile: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if convErr != nil {
		t.Fatalf("parse pid %q: %v", pidBytes, convErr)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // Process reaped — test passes.
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("grandchild pid %d survived group SIGKILL escalation", pid)
}

func TestRunner_Run_EnvVars(t *testing.T) {
	r := newRunnerAt(t, t.TempDir())

	workDir := t.TempDir()
	scriptPath, err := filepath.Abs("testdata/worktree-post-create.sh")
	if err != nil {
		t.Fatalf("abs testdata: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "hook.log")

	err = r.Run(context.Background(), RunOptions{
		ScriptPath:   scriptPath,
		WorktreePath: workDir,
		RepoRoot:     "/tmp/fake-repo",
		Branch:       "wip/jin-abcd1234",
		Base:         "main",
		SessionID:    "sess-uuid-1",
		SessionName:  "test-session",
		LogPath:      logPath,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(logBytes)

	wants := []string{
		"JIN_WORKTREE_PATH=" + workDir,
		"JIN_WORKTREE_BRANCH=wip/jin-abcd1234",
		"JIN_WORKTREE_BASE=main",
		"JIN_SESSION_ID=sess-uuid-1",
		"JIN_SESSION_NAME=test-session",
		"JIN_REPO_ROOT=/tmp/fake-repo",
		"PWD=" + workDir,
	}
	for _, want := range wants {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q; log:\n%s", want, log)
		}
	}
}

func TestRunner_Run_Logging(t *testing.T) {
	r := newRunnerAt(t, t.TempDir())

	workDir := t.TempDir()
	scriptPath := writeScript(t, t.TempDir(), "echo from-stdout\necho from-stderr >&2\nexit 0\n")
	logPath := filepath.Join(t.TempDir(), "sub", "hook.log")

	err := r.Run(context.Background(), RunOptions{
		ScriptPath:   scriptPath,
		WorktreePath: workDir,
		LogPath:      logPath,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(b)
	if !strings.Contains(log, "from-stdout") {
		t.Errorf("log missing stdout line: %s", log)
	}
	if !strings.Contains(log, "from-stderr") {
		t.Errorf("log missing stderr line: %s", log)
	}
}

func TestRunner_HookLogPath(t *testing.T) {
	r := newRunnerAt(t, t.TempDir())
	got := r.HookLogPath("/state", "abc-123")
	want := filepath.Join("/state", "hook-logs", "abc-123.log")
	if got != want {
		t.Errorf("HookLogPath = %q, want %q", got, want)
	}
}
