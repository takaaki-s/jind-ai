package worktreehook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// killGracePeriod is how long the hook's process group has to exit after
// SIGTERM before Run escalates to a group-wide SIGKILL. Matches spec F5/N2.
const killGracePeriod = 5 * time.Second

const scriptRelPath = ".jin/worktree-post-create.sh"

// inheritedEnvKeys is the minimal set of parent-process env vars forwarded to
// the hook. It covers what pnpm / mise / node etc. need to bootstrap without
// leaking arbitrary caller state. LC_* is handled separately by prefix match.
var inheritedEnvKeys = map[string]bool{
	"PATH":  true,
	"HOME":  true,
	"USER":  true,
	"SHELL": true,
	"LANG":  true,
	"TERM":  true,
}

type runner struct {
	allowlist *Allowlist
}

// NewRunner loads the on-disk allowlist and returns a Runner that shells out
// to bash. A missing allowlist file is treated as empty (see LoadAllowlist).
func NewRunner(stateDir string) (Runner, error) {
	al, err := LoadAllowlist(stateDir)
	if err != nil {
		return nil, err
	}
	return &runner{allowlist: al}, nil
}

// Discover reports whether repoRoot contains the fixed hook path
// .jin/worktree-post-create.sh. Any stat error other than "does not exist"
// is treated as "not present" — the caller cannot do anything useful with a
// half-visible script and Run would fail with a clearer error anyway.
func (r *runner) Discover(repoRoot string) (string, bool) {
	scriptPath := filepath.Join(repoRoot, scriptRelPath)
	info, err := os.Stat(scriptPath)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return "", false
	}
	return scriptPath, true
}

// Verify checks the script's SHA256 against the allowlist entry for the
// normalized repoRoot. A missing entry yields VerdictNotAllowed; a mismatched
// SHA yields VerdictChanged.
func (r *runner) Verify(scriptPath, repoRoot string) (Verdict, error) {
	absRepo, err := filepath.Abs(repoRoot)
	if err != nil {
		return VerdictNotAllowed, fmt.Errorf("abs repo root: %w", err)
	}
	sha, err := ComputeSHA256(scriptPath)
	if err != nil {
		return VerdictNotAllowed, fmt.Errorf("hash script: %w", err)
	}
	entry, ok := r.allowlist.Get(absRepo)
	if !ok {
		return VerdictNotAllowed, nil
	}
	if entry.SHA256 != sha {
		return VerdictChanged, nil
	}
	return VerdictOK, nil
}

// HookLogPath returns the log file location for a given session's hook run.
// The parent directory is created lazily by Run so callers only pay for it
// when a hook actually executes.
func (r *runner) HookLogPath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, "hook-logs", sessionID+".log")
}

// Run executes the hook via `bash <scriptPath>` with a curated environment
// and captures stdout/stderr to opts.LogPath. When JIN_DEBUG=1 the output is
// also teed to the caller's stderr for interactive debugging. On timeout the
// returned error explicitly mentions the timeout so callers can surface a
// friendlier message than the raw exit-status text.
//
// Signal escalation on ctx cancellation: cmd.Cancel sends SIGTERM to the
// hook's whole process group (Setpgid places children in a fresh group). A
// deferred timer then sends SIGKILL to the same group after killGracePeriod
// so a hook that ignores SIGTERM (e.g. via `trap` on TERM) cannot outlive the
// escalation. We drive this ourselves rather than relying on cmd.WaitDelay,
// which only sends SIGKILL to the leader PID and leaves grandchildren alive.
func (r *runner) Run(ctx context.Context, opts RunOptions) error {
	if err := os.MkdirAll(filepath.Dir(opts.LogPath), 0o755); err != nil {
		return fmt.Errorf("mkdir hook log dir: %w", err)
	}
	logFile, err := os.OpenFile(opts.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open hook log: %w", err)
	}
	defer logFile.Close()

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer devNull.Close()

	var out io.Writer = logFile
	if os.Getenv("JIN_DEBUG") == "1" {
		out = io.MultiWriter(logFile, os.Stderr)
	}

	cmd := exec.CommandContext(ctx, "bash", opts.ScriptPath)
	cmd.Dir = opts.WorktreePath
	cmd.Env = buildEnv(opts)
	cmd.Stdin = devNull
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancel is invoked by the ctx-watcher goroutine that Start spawns; it must
	// be set before Start to avoid a data race on cmd.Cancel. Reads
	// cmd.Process.Pid, which Start populates before the watcher can observe
	// ctx cancellation. The AfterFunc timer is fire-and-forget on purpose:
	// once Cancel fires we treat the escalation as unconditional — a wasted
	// SIGKILL syscall to an already-reaped group is harmless (ESRCH) and
	// avoids sharing the *Timer across the watcher and caller goroutines.
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		time.AfterFunc(killGracePeriod, func() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		})
		return syscall.Kill(-pid, syscall.SIGTERM)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start hook: %w", err)
	}

	runErr := cmd.Wait()

	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		if opts.Timeout > 0 {
			return fmt.Errorf("hook timed out after %s (log: %s)", opts.Timeout, opts.LogPath)
		}
		return fmt.Errorf("hook timed out (log: %s)", opts.LogPath)
	}
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			return fmt.Errorf("exit status %d", exitErr.ExitCode())
		}
		return fmt.Errorf("run hook: %w", runErr)
	}
	return nil
}

func buildEnv(opts RunOptions) []string {
	env := make([]string, 0, 16)
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		key := kv[:i]
		if inheritedEnvKeys[key] || strings.HasPrefix(key, "LC_") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"JIN_WORKTREE_PATH="+opts.WorktreePath,
		"JIN_WORKTREE_BRANCH="+opts.Branch,
		"JIN_WORKTREE_BASE="+opts.Base,
		"JIN_SESSION_ID="+opts.SessionID,
		"JIN_SESSION_NAME="+opts.SessionName,
		"JIN_REPO_ROOT="+opts.RepoRoot,
	)
	return env
}
