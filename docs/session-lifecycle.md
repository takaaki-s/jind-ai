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
                    StatusRunning ◄─── RecoverTmuxSessions() (only when no
                     │    │    │        hook-derived status was persisted)
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
├─ ID                    string    // UUID (also mint-shape for Claude Code --session-id)
├─ Description           string    // Human-readable label (see Description Model below)
├─ DescriptionLocked     bool      // true = manual override, blocks Layer C auto-upgrade
├─ WorkDir               string    // Working directory (dynamically updated via hook cwd)
├─ CreatedAt             time.Time // Creation timestamp
├─ Status                Status
├─ LastActiveAt          time.Time
├─ ErrorMessage          string    // Error message (e.g., on startup failure)
├─ AgentKind             string    // Adapter identifier ("claude" etc.); always non-empty in persisted form
├─ AgentSessionID        string    // Adapter-side persistent id (CC --session-id / --resume value)
├─ AgentSessionStarted   bool      // Flipped once the agent has spawned; drives adapter's fresh-vs-resume branch
├─ TmuxWindowName        string    // Inner tmux session name
└─ TmuxPaneID            string    // Agent pane ID (e.g., "%42")

Session (runtime only, json:"-")
├─ LastOutputTime    time.Time         // For idle stability detection
├─ StartedAt        time.Time         // Prevents false error detection right after startup
├─ SSHAuthSock      string            // For git operations
├─ CurrentWorkDir   string            // tmux pane_current_path
├─ CurrentBranch    string            // git branch
├─ IsGitRepo        bool
└─ DescriptionLayer DescriptionLayer  // 0=Baseline, 1=Layer C-name (derived), 2=Layer C-name (strong), 3=Layer C-transcript; drives Manager.TryUpgradeDescription's promotion guard
```

## Description Model

Sessions carry a `Description` (human-readable label) that is separate from the technical `ID`. It is generated in three layers:

- **Layer A (baseline)** — `GenerateBaselineDescription(workDir, branch, isWorktree, tmuxHint)` produces `<repo>[:<branch>][:<subpath>]` (e.g. `jind-ai:main`). Always populated at session creation, never empty. Agent-independent.
- **Layer B (manual override)** — Set via `--description` on `session new`, the `set-description` subcommand, or the TUI description step. Sets `DescriptionLocked = true`, blocking Layer C.
- **Layer C (agent-specific enhancer)** — On `SessionStart` / `UserPromptSubmit` / `Stop` hooks, if `DescriptionLocked = false`, the registered `DescriptionEnhancer` (currently `internal/agent/claude/CCDescriptionEnhancer`) returns a `(candidate, DescriptionLayer, ok)` tuple. `TryUpgradeDescription` applies the candidate only when its `DescriptionLayer` is strictly higher than the session's current layer. The Claude Code enhancer tries two signals in order of informativeness:
  - **Layer C-name (strong)** (`DescriptionLayerAgentName`) — first checks the transcript for `{"type":"ai-title","aiTitle":"…"}` (the value CC surfaces as "Session name" in `/status`). If absent, falls back to `~/.claude/sessions/<PID>.json`'s `name` when `nameSource` is anything other than `"derived"` (or the field is missing on older CC versions). This is the definitive name — treated as final Layer C-name for the session.
  - **Layer C-name (derived)** (`DescriptionLayerAgentNameDerived`) — `~/.claude/sessions/<PID>.json` `name` with `nameSource == "derived"`: the tmux window hint jind-ai itself handed CC (e.g. `jin-395bce5c-71`). Fires at `SessionStart`, so the description leaves the Layer A baseline the moment the process boots, but any later stronger name (`aiTitle`, `/rename`) still overwrites it.

  The CC enhancer never returns `DescriptionLayerTranscript`: Claude Code owns the naming and eventually writes `aiTitle`, so the raw first user prompt is never promoted into `Description`. `DescriptionLayerTranscript` remains in the layer enum for future adapters that lack a native session-name field.

  `Session.DescriptionLayer` is a runtime-only field (`json:"-"`), so daemon restart resets it to zero. A separate guard (`Description != baseline && layer == 0 → skip`) prevents a lower layer from clobbering a higher-layer value that survived the restart in the persisted `Description`.

### DescriptionLocked Lifecycle

| Trigger | Description | DescriptionLocked |
|---|---|---|
| Session created (no `--description`) | Layer A output | `false` |
| Session created with `--description "<v>"` | `<v>` | `true` |
| `set-description <sel> "<v>"` (non-empty) | `<v>` | `true` |
| `set-description <sel> ""` | Layer A regenerated | `false` (unlock) |
| Layer C hook fires (locked = false, Description == baseline) | Enhancer output | `false` (unchanged) |
| Layer C hook fires (locked = true) | unchanged | `true` |

Legacy `Name` field is migrated on daemon startup: `store.Load()` reads the raw JSON, applies `migrateSessionJSON` (see `internal/session/migration.go`), and writes back the new schema. Migrated sessions are conservatively marked `DescriptionLocked = true` because a persisted Name is assumed to be a manual choice.

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
2. Derive the worktree name/branch and resolve the worktree path from `WorktreeConfig.BaseDir`
3. `git worktree add <path> origin/<base>` — cuts the branch from the locally cached `origin/<base>` (no fetch is performed; users who need a fresher tip run `git fetch` in the source repo beforehand or via the post-create hook). On success, sets `worktreeCreated = true` and registers a `defer` that rolls the worktree/branch back (`RemoveWorktree` + `DeleteBranch`) if the function later returns an error
4. **Post-create hook** (see below) — runs synchronously, still inside the rollback window opened in step 3
5. `opts.WorkDir` is rewritten to the new worktree path, and the common session-creation path resumes

### Post-create hook (`.jin/worktree-post-create.sh`)

Runs after the worktree is created (step 3) and before Claude Code starts. `StartBackground` is a separate call the caller makes after `CreateWithOptions` returns, so the hook always finishes first:

1. **Discover**: look for `.jin/worktree-post-create.sh` at the original repository root. Missing → skip silently, worktree creation proceeds unchanged.
2. **Verify** against the allowlist (`internal/worktreehook`, SHA256-tracked like direnv):
   - Not yet allowed, or the script's content changed since it was allowed → skip with a warning (session creation still succeeds); the user must run `jin worktree allow`
   - Allowed and unchanged → run
3. **Run**: `bash <script>` executes with `cwd` set to the new worktree; default timeout 300s (`worktree.hook_timeout`). Exceeding the timeout kills the process (`exec.CommandContext`'s default cancel behavior).
4. **On failure** (non-zero exit or timeout): `CreateWithOptions` returns an error, which triggers the step 3 `defer` — the worktree and its branch are rolled back, leaving no partial state
5. Skipped without running when: no script is present, `opts.NoHook` (`--no-hook`), or `worktree.hook_enabled: false`

stdout/stderr are saved to `~/.local/state/jind-ai/hook-logs/<session-id>.log` regardless of outcome. See README.md ("Worktree Post-Create Hook") for the script's environment variables and the allow model.

## Recovery (On Daemon Restart)

`RecoverTmuxSessions()`:
1. On load, in-memory Status is normalized to Stopped (the process may be
   gone); the on-disk value is stashed in the runtime-only
   `Session.PersistedStatus` for recovery to consume
2. For sessions with TmuxWindowName, checks if the inner tmux is alive
3. Alive → restart `captureOutputTmux()`. The status is decided in two steps:
   - The hook-derived status persisted before the restart
     (idle/thinking/permission) is restored as the best estimate; other
     states (stopped/creating/running) fall back to StatusRunning. A live
     in-memory status (hooks that fired since load) wins over the on-disk
     value
   - The agent adapter may then refine it via a `StatusSignal{Kind:"recover"}`
     (payload: `persisted_status`, `agent_session_id`, `workdir`). Hooks fired
     while the daemon was down are lost, so the persisted value can be stale;
     the Claude Code adapter re-derives the status from the transcript's last
     turn (`transcript.TurnState`): assistant message without a trailing
     tool_use → idle (a missed Stop hook), trailing tool_use → thinking
     (or permission when persisted, indistinguishable from the transcript
     alone), last entry user → thinking. Unknown/no transcript keeps the
     step-1 decision
4. Pane dead → StatusStopped (TmuxWindowName preserved for RespawnPane)
5. Session itself gone → Clear TmuxWindowName + StatusStopped

Known residual: a recovered session whose status ends up "running" (no
hook-derived status persisted and no transcript verdict) stays "running"
until the next hook — the running→idle poll fallback is intentionally
disabled for recovered sessions (`StartedAt` is runtime-only) to avoid false
idle transitions while a task is still executing.

## Auto-Recovery on Resume Failure

Inside `captureOutputTmux()`, detects pane death within 10 seconds of startup:
1. Determines that the adapter's `--resume` path (or equivalent) has failed
2. Mints a fresh AgentSessionID and flips AgentSessionStarted = false
3. Rebuilds the shell command via `Agent.SpawnCommand` (fresh-session branch) and respawns the pane
4. If successful, continues as a new session

## Status Detection via Agent Adapters

`HandleHookEvent()` is agent-agnostic wiring: it looks the session up, updates
CWD / AgentSessionStarted invariants, and then hands the raw event to
`Agent.StatusSource.Interpret()`. Every adapter owns its own event vocabulary
and Status mapping — the Claude Code mapping lives in
`internal/agent/claude/status.go`; other adapters plug their own
`StatusSource` into the same slot without touching `session/manager.go`.

`StatusSignal.Kind` currently has two values: `"hook"` (live hook callback)
and `"recover"` (daemon-restart recovery, see above). Adapters ignore kinds
they don't understand by returning a false verdict, so new kinds are additive.

## WorkDir Tracking

WorkDir is updated through two paths:
1. **Via Hook**: `HandleHookEvent()` `cwd` field (the agent's actual CWD)
2. **Via Polling**: `captureOutputTmux()` `GetPaneCurrentPath()` (tmux pane's CWD)
