# Session Lifecycle

## Status State Machine

```
                    CreateWithOptions()
                          │
                          ▼
                     StatusStopped
                          │
                   StartBackground()
                          │
                          ▼
                    StatusRunning ◄─── RecoverTmuxSessions()
                     │    │    │
    UserPromptSubmit │    │    │ Notification(permission_prompt)
                     ▼    │    ▼
              StatusThinking  StatusPermission
                     │    │    │
                Stop │    │    │ Stop
                     ▼    ▼    ▼
                    StatusIdle
                          │
              pane dead / Kill()
                          │
                          ▼
                    StatusStopped
```

Status constants (session/session.go):
- `creating`   - CC starting up (currently unused, reserved)
- `stopped`    - Process stopped
- `running`    - Running (initial state before any hook is received)
- `idle`       - Waiting for input (Stop hook)
- `thinking`   - Processing (UserPromptSubmit hook)
- `permission` - Waiting for permission (Notification hook)

## Session Structure

```go
Session (persisted)
├─ ID              string    // UUID (compatible with Claude Code --session-id)
├─ Name            string    // Display name (default: basename of WorkDir)
├─ WorkDir         string    // Working directory (dynamically updated via hook cwd)
├─ CreatedAt       time.Time // Creation timestamp
├─ Status          Status
├─ LastActiveAt    time.Time
├─ ErrorMessage    string    // Error message (e.g., on startup failure)
├─ ClaudeSessionID string    // Claude Code session ID
├─ ClaudeSessionStarted bool // Used to determine --resume vs --session-id
├─ TmuxWindowName  string    // Inner tmux session name
└─ TmuxPaneID      string    // CC pane ID (e.g., "%42")

Session (runtime only, json:"-")
├─ LastOutputTime  time.Time // For idle stability detection
├─ StartedAt      time.Time // Prevents false error detection right after startup
├─ SSHAuthSock    string    // For git operations
├─ CurrentWorkDir string    // tmux pane_current_path
├─ CurrentBranch  string    // git branch
└─ IsGitRepo      bool
```

## Creation Flow

1. `Manager.CreateWithOptions()` creates a Session and persists it via Store
2. `Manager.StartBackground()` → `startSession()` → `startSessionTmux()`
3. `ensureTmuxClient()` initializes the inner tmux (`-L jin`)
4. `ensureClaudeTrustState()` sets trust config in `~/.claude/settings.local.json`
5. Creates an inner tmux session and runs `claude --session-id {ID}`
6. `TagManagedPane()` tags the pane for remain-on-exit
7. Starts `captureOutputTmux()` goroutine for polling

## Worktree Creation (`opts.Worktree`)

When `CreateWithOptions` is called with `Worktree: true`, an additional block runs before the common session-creation path (duplicate-directory check, name assignment, `Session` construction):

1. Validate `opts.WorkDir` is a git root (`git.IsGitRoot`); resolve the base branch (`opts.WorktreeBase` → detected default branch → `WorktreeConfig.DefaultBranch`)
2. `git fetch origin <base>` (best-effort unless `worktree.fetch_failure: strict`)
3. Derive the worktree name/branch and resolve the worktree path from `WorktreeConfig.BaseDir`
4. `git worktree add` — on success, sets `worktreeCreated = true` and registers a `defer` that rolls the worktree/branch back (`RemoveWorktree` + `DeleteBranch`) if the function later returns an error
5. **Post-create hook** (see below) — runs synchronously, still inside the rollback window opened in step 4
6. `opts.WorkDir` is rewritten to the new worktree path, and the common session-creation path resumes

### Post-create hook (`.jin/worktree-post-create.sh`)

Runs after the worktree is created (step 4) and before Claude Code starts. `StartBackground` is a separate call the caller makes after `CreateWithOptions` returns, so the hook always finishes first:

1. **Discover**: look for `.jin/worktree-post-create.sh` at the original repository root. Missing → skip silently, worktree creation proceeds unchanged.
2. **Verify** against the allowlist (`internal/worktreehook`, SHA256-tracked like direnv):
   - Not yet allowed, or the script's content changed since it was allowed → skip with a warning (session creation still succeeds); the user must run `jin worktree allow`
   - Allowed and unchanged → run
3. **Run**: `bash <script>` executes with `cwd` set to the new worktree; default timeout 300s (`worktree.hook_timeout`). Exceeding the timeout kills the process (`exec.CommandContext`'s default cancel behavior).
4. **On failure** (non-zero exit or timeout): `CreateWithOptions` returns an error, which triggers the step 4 `defer` — the worktree and its branch are rolled back, leaving no partial state
5. Skipped without running when: no script is present, `opts.NoHook` (`--no-hook`), or `worktree.hook_enabled: false`

stdout/stderr are saved to `~/.local/state/honjin/hook-logs/<session-id>.log` regardless of outcome. See README.md ("Worktree Post-Create Hook") for the script's environment variables and the allow model.

## Recovery (On Daemon Restart)

`RecoverTmuxSessions()`:
1. Loads all persisted sessions (initialized as Status=Stopped)
2. For sessions with TmuxWindowName, checks if the inner tmux is alive
3. Alive → StatusRunning + restart `captureOutputTmux()`
4. Pane dead → StatusStopped (TmuxWindowName preserved for RespawnPane)
5. Session itself gone → Clear TmuxWindowName + StatusStopped

## Auto-Recovery on Resume Failure

Inside `captureOutputTmux()`, detects pane death within 10 seconds of startup:
1. Determines that `claude --resume` has failed
2. Generates a new ClaudeSessionID
3. Respawns pane with `claude --session-id {newID}`
4. If successful, continues as a new session

## WorkDir Tracking

WorkDir is updated through two paths:
1. **Via Hook**: `HandleHookEvent()` `cwd` field (Claude Code's actual CWD)
2. **Via Polling**: `captureOutputTmux()` `GetPaneCurrentPath()` (tmux pane's CWD)
