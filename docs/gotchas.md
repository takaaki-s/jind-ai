# Gotchas

Common pitfalls and caveats that agents tend to fall into.

## tmux

- **remain-on-exit is set at the pane level** (not globally).
  `TagManagedPane()` applies it only to managed panes.
  Panes added by the user are destroyed immediately.
  (Fixed in commit 980e99f)

- **tmux session name** is the `tmux.SessionName` constant ("jin"). Do not change it.

- **inner tmux**: jindaiko uses its own tmux socket (`-L jin`).
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
