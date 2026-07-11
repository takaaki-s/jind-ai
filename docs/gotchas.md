# Gotchas

Common pitfalls and caveats that agents tend to fall into.

## tmux

- **remain-on-exit is set at the pane level** (not globally).
  `TagManagedPane()` applies it only to managed panes.
  Panes added by the user are destroyed immediately.
  (Fixed in commit 980e99f)

- **tmux session name** is the `tmux.SessionName` constant ("jin"). Do not change it.

- **inner tmux**: jind-ai uses its own tmux socket (`-L jin`).
  It runs as a separate server process from the user's main tmux.

- **base-index issue**: If `base-index=1` is set in the user's `~/.tmux.conf`,
  the `:0.0` target becomes invalid. Use pane IDs (`%N`) instead.

## Hook

- **Session identification uses the `JIN_SESSION_ID` environment variable** (most reliable).
  Claude Code's session ID is used as a fallback.
  (Improved in commit a0bd6f7)

- **CWD tracking uses the hook's `cwd` field**.
  tmux's `pane_current_path` is also polled, but the hook takes priority.
  (Added in commit a705a80)

## Codex adapter

- **Initial `/hooks` trust approval is required.** The first time `jin
  session new --agent codex` runs in a given install (or after the `jin`
  binary path changes), Codex shows a `Hooks need review — N hooks are new
  or changed` dialog. Select **"Trust all and continue"** to enable status
  tracking. The trust hash is persisted to `~/.codex/config.toml` under
  `[hooks.state]`, so subsequent spawns skip the dialog as long as the
  command path stays the same. `--dangerously-bypass-hook-trust` is not
  used by jind-ai on purpose (see 02_design.md §3.3).

- **30 s poll fallback during the trust dialog is harmless.** Between
  session spawn and the user's trust confirmation, no hook fires. The
  daemon's `[POLL] no hook received for 30s, fallback` path takes the
  status from `running` down to `idle`. Once trust lands, subsequent
  `UserPromptSubmit` / `Stop` hooks drive the status correctly. If you see
  the poll fallback in normal use, the trust dialog is usually still open
  in the pane.

- **Directory trust ("Do you trust this directory?")** is a separate
  Codex sandbox prompt shown on the first launch in a given cwd; it is
  unrelated to `/hooks` and answered independently.

- **`AgentSessionID` is unknown until SessionStart.** Codex has no
  `--session-id` equivalent (openai/codex#13242). jind-ai spawns fresh
  `codex` on first start, ignores the pre-minted UUID it created for the
  Session record, and lets the `SessionStart` hook's stdin JSON carry the
  real Codex UUID back — the existing re-key path
  (`manager.go:1231-1234`) latches it without any daemon change. On
  resume, `codex resume <UUID>` fast-fails in a few seconds for unknown
  IDs, so the existing 10-second quick-fail auto-recovery covers the
  "session removed by hand" edge case without a defensive pre-glob.

- **`Layer C-transcript` reads the rollout JSONL.** The Codex enhancer
  extracts the first `role: "user"` message that is not a
  `<environment_context>` pseudo-user injection. See
  `internal/agent/codex/rollout.go`.

## Code Structure

- **Debug logging uses `internal/debug.NewLogger`**.
  Call `var debugLog = debug.NewLogger("filename.log")` to get a logger for any package.

- **config.Manager and config.StateManager are separate** instances. Do not confuse them.

- **Session.WorkDir is dynamically updated** (via hooks and tmux polling).
  Initial value = directory at creation time, but it follows when claude changes directory.

## Testing

- **Test coverage is ~40%**. Test files exist for all packages.
  Uses only the standard library (no testify, etc.). Add tests for new code.
  The `tmux.Runner` interface was introduced for testability.

## Concurrency

- **Session creation is protected by `createMu`** (at the daemon.Server level).
  This is a separate lock from `session.Manager.mu`.

- **I/O operations should be performed outside the lock** (to prevent deadlocks).
  Refer to the `List()` pattern: take a snapshot under RLock → release lock → read transcripts
