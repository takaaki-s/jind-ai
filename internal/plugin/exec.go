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
	"time"

	"github.com/takaaki-s/jind-ai/internal/debug"
	"github.com/takaaki-s/jind-ai/internal/jinenv"
	"github.com/takaaki-s/jind-ai/internal/procgroup"
)

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
// deadline is carried by the ctx passed to ExecPlugin, matching the
// worktreehook convention where the caller owns cancellation and the option
// only shapes the error message.
//
// PopupWidth / PopupHeight carry the pre-resolved popup size, already in tmux
// notation. Empty strings mean "no size hint" — buildEnv skips the env var so
// the CLI falls through to its own chain (flag > env > empty).
//
// ActionID is the manifest action being run, exported as JIN_ACTION_ID so a
// shared entrypoint script can tell which of its plugin's actions invoked it.
// Identity is which jin dispatched the run.
type ExecOptions struct {
	PluginDir   string
	Run         string
	ActionID    string
	Env         Event
	Caller      ActionContext
	Depth       int
	Identity    jinenv.Identity
	LogPath     string
	Timeout     time.Duration
	PopupWidth  string
	PopupHeight string
	HandoffKey  string
}

const MaxHandoffPayload = 16 << 10
const MaxHandoffResult = 32 << 10

// boundedCapture keeps only the bounded stdout contract while continuing to
// consume the provider's output. Returning len(p) prevents a chatty provider
// from seeing a broken pipe and obscuring the real "result too large" error.
type boundedCapture struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *boundedCapture) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = w.buf.Write(p[:remaining])
	}
	if remaining < len(p) {
		w.truncated = true
	}
	return len(p), nil
}

// LogPath returns the append-only log location for a plugin's runs. The parent
// directory is created lazily by ExecPlugin so callers only pay for it when a
// plugin actually executes.
func LogPath(stateDir, pluginName string) string {
	return filepath.Join(stateDir, "plugin-logs", pluginName+".log")
}

// ExecPlugin runs opts.Run once via `bash -c` in opts.PluginDir with a curated
// environment and the event marshalled to stdin as JSON. stdout/stderr are
// appended to opts.LogPath (each run prefixed by a separator header) and teed
// to the caller's stderr when JIN_DEBUG=1. On timeout the returned error names
// the timeout, so callers can surface something friendlier than raw exit text.
//
// Teardown on ctx cancellation is procgroup.KillOnCancel's: SIGTERM to the
// run's whole process group, then SIGKILL to the same group after
// procgroup.GracePeriod. cmd.WaitDelay alone would not do — it reaches only the
// leader PID — but it is set as well, and that is a behaviour change worth
// knowing here: under JIN_DEBUG the output goes through an io.MultiWriter
// rather than a plain file, so os/exec pipes it, and a descendant that escapes
// the group while holding that pipe ends the wait with exec.ErrWaitDelay
// instead of blocking for good.
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

	// This process's own flag, not opts.Identity.Debug: where the plugin's
	// output goes is a decision jind-ai makes about itself, while the identity
	// says what the plugin is told. In production they agree.
	var out io.Writer = logFile
	if debug.Enabled() {
		out = io.MultiWriter(logFile, os.Stderr)
	}

	_, _ = fmt.Fprintf(out, "--- %s %s session=%s ---\n",
		time.Now().Format(time.RFC3339), opts.Env.Name, opts.Env.SessionID)

	cmd := procgroup.CommandContext(ctx, "bash", "-c", opts.Run)
	cmd.Dir = opts.PluginDir
	cmd.Env = buildEnv(opts)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = out
	cmd.Stderr = out

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

// ExecHandoff runs a synchronous structured plugin endpoint. stdin is the
// bounded provider-neutral request; stdout must be one bounded JSON result.
// stderr and a copy of stdout still go to the ordinary plugin log so provider
// failures remain diagnosable without leaking log content into session state.
func ExecHandoff(ctx context.Context, opts ExecOptions, payload []byte) ([]byte, error) {
	if len(payload) > MaxHandoffPayload {
		return nil, fmt.Errorf("handoff payload exceeds %d bytes", MaxHandoffPayload)
	}
	if err := os.MkdirAll(filepath.Dir(opts.LogPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir plugin log dir: %w", err)
	}
	logFile, err := os.OpenFile(opts.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open plugin log: %w", err)
	}
	defer logFile.Close()

	var diagnostic io.Writer = logFile
	if debug.Enabled() {
		diagnostic = io.MultiWriter(logFile, os.Stderr)
	}
	_, _ = fmt.Fprintf(diagnostic, "--- %s pr_handoff session=%s key=%s ---\n",
		time.Now().Format(time.RFC3339), opts.Env.SessionID, opts.HandoffKey)

	capture := &boundedCapture{limit: MaxHandoffResult}
	cmd := procgroup.CommandContext(ctx, "bash", "-c", opts.Run)
	cmd.Dir = opts.PluginDir
	cmd.Env = buildEnv(opts)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = io.MultiWriter(diagnostic, capture)
	cmd.Stderr = diagnostic
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start handoff plugin: %w", err)
	}
	runErr := cmd.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("handoff plugin timed out after %s", opts.Timeout)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return nil, fmt.Errorf("handoff plugin exited with status %d", exitErr.ExitCode())
		}
		return nil, fmt.Errorf("run handoff plugin: %w", runErr)
	}
	if capture.truncated {
		return nil, fmt.Errorf("handoff result exceeds %d bytes", MaxHandoffResult)
	}
	return bytes.TrimSpace(capture.buf.Bytes()), nil
}

// buildEnv assembles the plugin's environment: the curated inherited keys plus
// the injected JIN_* vars derived from opts.
func buildEnv(opts ExecOptions) []string {
	env := jinenv.InheritedEnv()
	env = append(env,
		"JIN_EVENT="+opts.Env.Name,
		"JIN_SESSION_ID="+opts.Env.SessionID,
		"JIN_STATUS="+opts.Env.Status,
		"JIN_PREV_STATUS="+opts.Env.PrevStatus,
		"JIN_AGENT_KIND="+opts.Env.AgentKind,
		"JIN_WORKDIR="+opts.Env.WorkDir,
		"JIN_TMUX_PANE_ID="+opts.Env.TmuxPaneID,
		"JIN_NOTIFY_KIND="+opts.Env.NotifyKind,
		"JIN_ACTION_ID="+opts.ActionID,
		jinenv.EnvDepth+"="+strconv.Itoa(opts.Depth),
	)
	if opts.HandoffKey != "" {
		env = append(env, "JIN_HANDOFF_KEY="+opts.HandoffKey)
	}
	// Caller tmux context exists only for action runs launched from inside a
	// tmux client; unlike the JIN_* event vars above these are omitted (not set
	// empty) so plugins can fall back to their own $TMUX with ${VAR:-...}.
	if opts.Caller.TmuxSocket != "" {
		env = append(env, "JIN_CALLER_TMUX_SOCKET="+opts.Caller.TmuxSocket)
	}
	if opts.Caller.TmuxPane != "" {
		env = append(env, "JIN_CALLER_TMUX_PANE="+opts.Caller.TmuxPane)
	}
	// Which jin dispatched this run. Taken whole from the caller — os.Executable
	// used to stand in for BinPath here, which is answering "which jin am I" a
	// second time, and the two answers were measured to differ.
	env = append(env, opts.Identity.Environ()...)
	if opts.PopupWidth != "" {
		env = append(env, "JIN_PLUGIN_POPUP_WIDTH="+opts.PopupWidth)
	}
	if opts.PopupHeight != "" {
		env = append(env, "JIN_PLUGIN_POPUP_HEIGHT="+opts.PopupHeight)
	}
	return env
}
