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
- `permission` - Waiting for permission (Notification hook). Not reachable
  from a hook on a codex session: that adapter maps `PermissionRequest` to
  `thinking` deliberately — see [architecture.md](architecture.md) and the
  "Codex adapter" section of [gotchas.md](gotchas.md#codex-adapter)

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
├─ AgentSessionStarted   bool      // Flipped once the agent has spawned (at spawn, not on hook arrival)
├─ AgentSessionIDConfirmed
│                        bool      // True once a hook reported AgentSessionID; each adapter decides what it needs to resume
├─ Model                 string    // Agent model in that CLI's own spelling; empty = the agent's default
├─ TmuxWindowName        string    // Inner tmux session name; kept across a kill (see Kill below)
└─ TmuxPaneID            string    // Agent pane ID (e.g., "%42"); kept across a kill

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
2. `Manager.StartBackground()` → `startSessionTmux()` (under `m.mu`)
3. `ensureTmuxClient()` initializes the inner tmux (`-L jin`)
4. The adapter's `Setup()` runs — for Claude Code that is
   `EnsureHooksSettingsFile()` (rewritten on every start, so a deleted or
   hand-edited file recovers) plus `EnsureTrustState()`, which sets
   `projects[<workDir>].hasTrustDialogAccepted` in `~/.claude.json`
   (not a settings file — see docs/gotchas.md)
5. Creates an inner tmux session and runs `claude --session-id "$JIN_CLAUDE_SESSION"`.
   The id travels in the environment rather than in the command text — see the
   shell-safety contract on `session.SpawnPlan`
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
   - "Last entry user" means a user entry the agent owes a reply to: one
     carrying a promptSource stamp, or a tool_result. Claude Code also writes
     entries in the user's voice that nobody submitted — the stdout of a local
     slash command, the notice raised when one is invoked, the echo of a `!`
     bash line — and those are skipped so the entry underneath decides. An
     interruption ends the turn outright (→ idle), taking the entries before it
     with it. Without this, a transcript ending on any of them read as a fresh
     prompt and pinned an idle session to "thinking", which closes SendPrompt's
     idle gate with no way to reopen it from jin: "thinking" is only left
     through a hook, every hook needs the agent to act, and the gate is what
     refuses to ask it to. Recovery re-reads the same transcript on the next
     daemon start and reaches the same verdict, so restarting does not clear it
     either — only driving the agent by hand does (attaching to the pane, or
     stopping and restarting the session, whose SessionStart writes idle)
   - The Codex adapter answers the same signal with one narrow correction: a
     persisted `permission` becomes `thinking`, and every other status gets a
     false verdict. It reads no rollout, so the verdict restates the persisted
     value rather than re-deriving one — see "Codex adapter" in
     [gotchas.md](gotchas.md#codex-adapter)
4. Pane dead → StatusStopped (TmuxWindowName preserved for RespawnPane).
   Killed sessions land here too, since a kill leaves the pane dead rather
   than destroying it
5. Session itself gone → Clear TmuxWindowName + StatusStopped

A decision is re-validated before it is applied, and dropped when the session
was deleted, started, or killed while the (unlocked) probes ran.

The adapter's verdict takes one more check: it is withheld when the session's
Status has moved since the snapshot, and the step-1 fallback — which already
recomputes from live status — decides instead. The verdict was derived before
the probes, so a hook that landed during them is the fresher report. What the
check is worth depends on the adapter: one that re-reads the agent would
reconverge on the next hook anyway, but one answering from the persisted value
alone (the Codex branch above) has nothing to reconverge with, and a `Stop`
overwritten by it would stay overwritten — the next restart reads the result
back and re-derives it identically, no timer clears it, and `send` refuses it.

The check compares Status values, so a hook that rewrites the same status is
invisible to it. Detecting every write would need a counter on the write path,
the way `killSeq` does for kills.

Known residual: a recovered session whose status ends up "running" (no
hook-derived status persisted and no transcript verdict) stays "running"
until the next hook — the running→idle poll fallback is intentionally
disabled for recovered sessions (`StartedAt` is runtime-only) to avoid false
idle transitions while a task is still executing.

## Kill

`Kill()` stops the agent; it does not tear the session's tmux state down.
It sends SIGTERM to the agent pane's process and leaves the pane in place
(`remain-on-exit`), so:

- the inner tmux session survives, and with it every other pane in that
  window — plugin splits, shells the user opened
- `TmuxWindowName` / `TmuxPaneID` stay set, so `StartBackground()` revives the
  agent with `RespawnPane` in the pane it already had, at its original size
  and position, instead of rebuilding the window
- the pane keeps its scrollback until the restart respawns it

The session reads as `stopped` from the moment the request is accepted, before
the tmux work runs, so a start arriving mid-kill is not mistaken for a no-op.

A pane that is already dead (killed twice, or an agent that crashed on its
own) is left alone rather than signalled: tmux keeps reporting the pid the
pane started with, which the OS may since have reissued elsewhere.

If the process outlives the signal (bounded wait, see `paneTerminateTries`)
the pane is killed outright, the inner session with it, and both fields are
cleared — that path loses the session's other panes, which is the price of an
agent that would not stop. A kill always stops the agent; the preserved window
is best-effort on top of that.

Otherwise, releasing the tmux resources is `Delete()`'s job: it kills the
inner session, window and all. A session that is only killed keeps its window
until it is restarted or deleted.

## Auto-Recovery on Resume Failure

Inside `captureOutputTmux()`, detects pane death within `quickResumeFailWindow`
of startup:
1. Determines that the adapter's `--resume` path (or equivalent) has failed
2. Mints a fresh AgentSessionID and clears AgentSessionStarted,
   AgentSessionIDConfirmed and the runtime spawnResumed
3. Rebuilds the shell command via `Agent.SpawnCommand` (fresh-session branch) and respawns the pane
4. If successful, continues as a new session

A stop already on record short-circuits all of this — see `classifyPaneDeath`.
Past that, three bounds decide whether the retry runs.

- The spawn must have been a resume (`SpawnPlan.Resumed`). A fresh start that
  died resumed nothing.
- The pane's process must have exited non-zero — quitting exits 0, a failed
  resume does not. See `classifyPaneDeath` for why nothing else here separates
  the two, and `tmux.PaneDeath` for the one case that reads as a clean exit
  without being one.
- The window must outlast `paneMonitorInterval`, because the monitor's first
  look at a pane lands one whole interval after startup: set equal, the window
  is shut before anything can open it.

The retry clears `AgentSessionIDConfirmed` and `spawnResumed` along with the
id, so what it starts is a fresh spawn by the first bound above: an agent that
cannot come up at all is recorded stopped on its second death rather than
respawned once per poll for as long as the session lives.

## Status Detection via Agent Adapters

`HandleHookEvent()` is agent-agnostic wiring: it looks the session up, updates
CWD / AgentSessionStarted invariants, and then hands the raw event to
`Agent.StatusSource.Interpret()`. The one value it takes from the payload
rather than deriving — the adapter-side session id — passes a safety gate and
the adapter's `RecognizesSessionID` before it is recorded; a refusal drops that
write alone and leaves the rest of the event intact (see docs/gotchas.md under
Hook). Every adapter owns its own event vocabulary
and Status mapping — the Claude Code mapping lives in
`internal/agent/claude/status.go`; other adapters plug their own
`StatusSource` into the same slot without touching `session/manager.go`.

**Two status decisions are the Manager's and not an adapter's.** What they share
is why the *policy* lives here: both are conditional on the status a session is
already in, and only the Manager can read that without racing.

They differ in who decides the rule applies. The first is the Manager's whole:
it keys off an event name directly, because its premise — a process announced
itself — belongs to no adapter's vocabulary. The second splits, because its
premise does: only the adapter knows which of its events can occur outside a
turn, so the adapter classifies (`StatusUpdate.Liveness`) and the Manager
enforces. A third rule of this kind should take the first shape only if it can
be stated without naming any adapter's events.

The first: **`SessionStart` clears a `stopped`, and touches no other status.**
Its premise is process
liveness — something announced it is running — which belongs to no
vocabulary, and the Manager already draws that inference from that event to
set `AgentSessionStarted`. Placement is also what makes it safe: `Interpret`
runs before `m.mu` is taken, so a conditional verdict computed in an adapter
(a route exists, via `StatusSignal.Payload` — that is how `persisted_status`
reaches `interpretRecover`) could be decided against a status the monitor
goroutine has already replaced. Recovery tolerates that because
`applyRecovery` revalidates; the hook path has no such stage, so it reads and
writes `Status` inside one critical section instead.

The narrowness matters as much as the rule: `SessionStart` also fires for
resume, `/clear` and `/compact`, so an unconditional verdict would drop a
session to `idle` mid-turn the moment an auto-compaction ran. See the
`stopped` note under [gotchas.md](gotchas.md#hook) for why the stale stop
exists at all.

The second: **a verdict an adapter marked `StatusUpdate.Liveness` is withheld
from a session sitting `idle`.** Such a verdict reports that the agent is
alive rather than that a turn began — a tool hook, which can only fire inside
a turn something else opened — and hooks do not arrive in the order their
events happened, so one can land after the `Stop` that ended the turn and
write `thinking` over a session that is finished. Every other transition it
asks for still applies, including `permission` → `thinking` and the
contradiction of a stale stop; on an `idle` session the verdict is withheld
whole, error field included. What the rule gives up, and the measurement
behind it, are in [gotchas.md](gotchas.md#hook).

`StatusSignal.Kind` currently has two values: `"hook"` (live hook callback)
and `"recover"` (daemon-restart recovery, see above). Adapters ignore kinds
they don't understand by returning a false verdict, so new kinds are additive.

## WorkDir Tracking

WorkDir is updated through two paths:
1. **Via Hook**: `HandleHookEvent()` `cwd` field (the agent's actual CWD)
2. **Via Polling**: `captureOutputTmux()` `GetPaneCurrentPath()` (tmux pane's CWD)
