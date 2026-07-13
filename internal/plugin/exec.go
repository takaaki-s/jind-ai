package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// killGracePeriod is how long a plugin's process group has to exit after
// SIGTERM before ExecPlugin escalates to a group-wide SIGKILL. Mirrors the
// worktreehook runner's escalation window.
const killGracePeriod = 5 * time.Second

// setProcessGroupKill wires cmd so that ctx cancellation SIGTERMs the whole
// process group (Setpgid places children in a fresh group) and escalates to a
// group-wide SIGKILL after killGracePeriod. Shared by ExecPlugin and the build
// step so both get identical, leak-free teardown of a run that ignores SIGTERM.
//
// Cancel must be set before Start to avoid a data race with the ctx-watcher
// goroutine Start spawns; it reads cmd.Process.Pid, which Start populates first.
// The AfterFunc timer is fire-and-forget: once Cancel fires the escalation is
// unconditional — a wasted SIGKILL to an already-reaped group is harmless
// (ESRCH) and avoids sharing a *Timer across goroutines.
func setProcessGroupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		time.AfterFunc(killGracePeriod, func() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		})
		return syscall.Kill(-pid, syscall.SIGTERM)
	}
}

// inheritedEnvKeys is the minimal set of parent-process env vars forwarded to a
// plugin run. It covers what interpreters / toolchains need to bootstrap without
// leaking arbitrary daemon state. LC_* is handled separately by prefix match.
var inheritedEnvKeys = map[string]bool{
	"PATH":  true,
	"HOME":  true,
	"USER":  true,
	"SHELL": true,
	"LANG":  true,
	"TERM":  true,
}

// Event is the payload delivered to a plugin for one dispatch. It is defined
// here (not in session) so that session depends on plugin and not the reverse.
// The same value is both marshalled to stdin as JSON and exploded into JIN_*
// env vars for shell-level access.
type Event struct {
	Name       string `json:"name"`
	SessionID  string `json:"session_id"`
	Status     string `json:"status"`
	PrevStatus string `json:"prev_status"`
	AgentKind  string `json:"agent_kind"`
	WorkDir    string `json:"work_dir"`
	TmuxPaneID string `json:"tmux_pane_id,omitempty"`
	// NotifyKind carries the adapter-determined notification kind for this
	// transition: "task-complete", "error", or "permission". Empty when the
	// transition triggers no notification.
	NotifyKind string `json:"notify_kind,omitempty"`
}

// ActionContext carries caller-side context for on-demand action runs
// (`jin plugin run`). When the invoking CLI sits inside a tmux client, the
// caller's server socket and pane travel with the run so the plugin can
// address the pane it was launched from (e.g. `jin pane popup --here`).
// All fields are empty for event-driven runs and for callers outside tmux.
type ActionContext struct {
	TmuxSocket string
	TmuxPane   string
}

// ExecOptions configures a single plugin run. Timeout is display-only: the real
// deadline is carried by the ctx passed to ExecPlugin, matching the worktreehook
// convention where the caller owns cancellation and the option only shapes the
// error message.
//
// PopupWidth / PopupHeight carry the pre-resolved popup size (already in tmux
// notation, e.g. "70%") for the plugin's `jin pane popup --here` invocations.
// Empty strings mean "no size hint" — buildEnv skips the corresponding env var
// so the CLI falls through to its own fallback chain (flag > env > empty).
type ExecOptions struct {
	PluginDir   string
	Run         string
	Env         Event
	Caller      ActionContext
	Depth       int
	SocketPath  string
	LogPath     string
	Timeout     time.Duration
	PopupWidth  string
	PopupHeight string
}

// LogPath returns the append-only log location for a plugin's runs. The parent
// directory is created lazily by ExecPlugin so callers only pay for it when a
// plugin actually executes.
func LogPath(stateDir, pluginName string) string {
	return filepath.Join(stateDir, "plugin-logs", pluginName+".log")
}

// ExecPlugin runs opts.Run once via `bash -c` in opts.PluginDir with a curated
// environment and the event marshalled to stdin as JSON. stdout/stderr are
// appended to opts.LogPath (each run prefixed by a separator header), and teed
// to the caller's stderr when JIN_DEBUG=1. On timeout the returned error names
// the timeout so callers can surface a friendlier message than raw exit text.
//
// Signal escalation on ctx cancellation: cmd.Cancel sends SIGTERM to the run's
// whole process group (Setpgid places children in a fresh group). A deferred
// timer then sends SIGKILL to the same group after killGracePeriod so a run that
// ignores SIGTERM cannot outlive the escalation. We drive this ourselves rather
// than relying on cmd.WaitDelay, which only SIGKILLs the leader PID and leaves
// grandchildren alive.
func ExecPlugin(ctx context.Context, opts ExecOptions) error {
	if err := os.MkdirAll(filepath.Dir(opts.LogPath), 0o755); err != nil {
		return fmt.Errorf("mkdir plugin log dir: %w", err)
	}

	payload, err := json.Marshal(opts.Env)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	logFile, err := os.OpenFile(opts.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open plugin log: %w", err)
	}
	defer logFile.Close()

	var out io.Writer = logFile
	if os.Getenv("JIN_DEBUG") == "1" {
		out = io.MultiWriter(logFile, os.Stderr)
	}

	_, _ = fmt.Fprintf(out, "--- %s %s session=%s ---\n",
		time.Now().Format(time.RFC3339), opts.Env.Name, opts.Env.SessionID)

	cmd := exec.CommandContext(ctx, "bash", "-c", opts.Run)
	cmd.Dir = opts.PluginDir
	cmd.Env = buildEnv(opts)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = out
	cmd.Stderr = out
	setProcessGroupKill(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start plugin: %w", err)
	}

	runErr := cmd.Wait()

	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		if opts.Timeout > 0 {
			return fmt.Errorf("plugin timed out after %s (log: %s)", opts.Timeout, opts.LogPath)
		}
		return fmt.Errorf("plugin timed out (log: %s)", opts.LogPath)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return fmt.Errorf("exit status %d", exitErr.ExitCode())
		}
		return fmt.Errorf("run plugin: %w", runErr)
	}
	return nil
}

// curatedEnv returns the allowlisted subset of the parent process environment
// (inheritedEnvKeys plus any LC_* locale vars). It is the shared base for both a
// plugin's dispatch run and its build step, so neither inherits arbitrary daemon
// state.
func curatedEnv() []string {
	env := make([]string, 0, 8)
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
	return env
}

// buildEnv assembles the plugin's environment: the curated inherited keys plus
// the injected JIN_* vars derived from opts.
func buildEnv(opts ExecOptions) []string {
	env := curatedEnv()
	env = append(env,
		"JIN_EVENT="+opts.Env.Name,
		"JIN_SESSION_ID="+opts.Env.SessionID,
		"JIN_STATUS="+opts.Env.Status,
		"JIN_PREV_STATUS="+opts.Env.PrevStatus,
		"JIN_AGENT_KIND="+opts.Env.AgentKind,
		"JIN_WORKDIR="+opts.Env.WorkDir,
		"JIN_TMUX_PANE_ID="+opts.Env.TmuxPaneID,
		"JIN_NOTIFY_KIND="+opts.Env.NotifyKind,
		"JIN_PLUGIN_DEPTH="+strconv.Itoa(opts.Depth),
		"JIN_SOCKET="+opts.SocketPath,
	)
	// Caller tmux context exists only for action runs launched from inside a
	// tmux client; unlike the JIN_* event vars above these are omitted (not set
	// empty) so plugins can fall back to their own $TMUX with ${VAR:-...}.
	if opts.Caller.TmuxSocket != "" {
		env = append(env, "JIN_CALLER_TMUX_SOCKET="+opts.Caller.TmuxSocket)
	}
	if opts.Caller.TmuxPane != "" {
		env = append(env, "JIN_CALLER_TMUX_PANE="+opts.Caller.TmuxPane)
	}
	// JIN_BIN points at the daemon's own binary so plugins can call back into
	// the exact version that dispatched them. A `jin` found on PATH may be an
	// older install that lacks newer subcommands (daemon/CLI version skew).
	if exe, err := os.Executable(); err == nil {
		env = append(env, "JIN_BIN="+exe)
	}
	if opts.PopupWidth != "" {
		env = append(env, "JIN_PLUGIN_POPUP_WIDTH="+opts.PopupWidth)
	}
	if opts.PopupHeight != "" {
		env = append(env, "JIN_PLUGIN_POPUP_HEIGHT="+opts.PopupHeight)
	}
	return env
}
