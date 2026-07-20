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

## Session send

- **`SendPrompt` verifies keystrokes landed before pressing Enter.**
  `tmux send-keys` reports success unconditionally, even when the target TUI
  is still redrawing after startup and drops the incoming keys. To make this
  observable, `Manager.SendPrompt` captures the pane before and after the
  send and checks that the tail of the prompt appeared in the visible buffer
  (`sendVerifyOK` in `internal/session/manager.go`). Attempts repeat with
  backoff for up to `sendVerifyTimeout` (default 5s); Enter is only pressed
  after a successful verify. This means the CLI contract is stronger than it
  looks: when `jin session send` returns nil, the prompt is in the input
  buffer — orchestration callers do NOT need to interleave
  `jin session wait --status idle` between `session new` and the first
  `session send`.

- **`send --wait-running` only verifies the agent took the prompt.** Since
  `SendPrompt` itself guarantees keystroke reception, `--wait-running` is
  now purely about "did the agent transition into running/thinking/permission
  after the prompt landed?". Callers that only care about "was my prompt
  seen?" can drop the flag entirely.

- **The verify check keys off the prompt's tail, not full text.** TUIs wrap
  long input across visible rows and may add ANSI styling. `promptTail` /
  `collapseWS` normalize both sides to whitespace-collapsed plain text and
  match only the last `sendVerifyTailBytes` bytes. A prompt whose entire
  tail happens to already exist in the pane (rare — e.g. re-sending the same
  short phrase seen elsewhere on screen) will not falsely satisfy verify
  because the check compares occurrence counts before/after, not mere
  presence.

- **Verify guarantees "the tail landed", not "exactly the prompt landed once".**
  If a TUI accepts a strict prefix of the keys on the first attempt and
  drops the rest, the retry re-sends the full prompt and the input area
  ends up carrying `<prefix><full prompt>`. Verify still passes (the tail
  appears one more time than before) and Enter commits the concatenated
  form. We accept the risk in transport because the fixes we considered
  (kill-line before each retry, echo-diff on the exact prompt) all leak
  per-TUI assumptions into the agent-agnostic layer. In practice tmux
  `send-keys` tends to deliver keystrokes atomically, so we have not
  observed corruption against Claude Code or Codex — but if it ever
  shows up (garbled first message on a slow-startup pane), that is
  the escape hatch to revisit.

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

## opencode adapter

- **`OPENCODE_CONFIG_DIR` is additive, not a replacement.** opencode's
  `ConfigPaths.directories()` returns
  `unique([~/.config/opencode, …project .opencode dirs, $OPENCODE_CONFIG_DIR])`,
  so pointing it at jind-ai state does **not** hide the user's own agents,
  commands or plugins. Setting it does suppress opencode's "seed an empty
  `~/.config/opencode/opencode.json`" behaviour, which is a bonus rather
  than a problem. Verified against opencode 1.17.18.

- **opencode treats the config dir as its own.** On start it writes a
  `.gitignore` into `<StateDir>/opencode/` and installs
  `@opencode-ai/plugin` into a `node_modules/` beside it. That is expected
  — it does the same to `~/.config/opencode` — and is exactly why the
  directory must live under jind-ai state rather than anywhere the user
  owns.

- **Bun does not need to be on `PATH`.** The opencode binary bundles its
  own Bun runtime, so the file-based plugin loads and runs even with bun
  entirely absent from `PATH` (verified: all three status events still
  fired). An external bun only matters for npm-specified plugins, which
  jind-ai does not use.

- **`session.status` fires once per step.** A single trivial turn
  publishes ~9 `session.status{type:"busy"}` events. The plugin suppresses
  consecutive duplicates of the same canonical event, so one turn yields
  one `UserPromptSubmit`. Removing that suppression multiplies daemon IPC
  for no gain.

- **Going idle publishes two events.** `SessionStatus.set()` publishes
  `session.status{type:"idle"}` *and* `session.idle`. Only `session.idle`
  is mapped to `Stop`; mapping both would double-report every turn.

- **`AgentSessionID` is unknown until `session.created`.** opencode has no
  flag that assigns a session id at startup (`--session` only continues an
  existing one). jind-ai spawns fresh, and the plugin's `SessionStart`
  carries the real id back through the usual re-key path. The resume
  branch keys off the **`ses_` prefix** rather than `AgentSessionStarted`,
  because `startSessionTmux` sets that flag before the process is even
  spawned — without the prefix test a pre-minted UUID would be passed to
  `--session`. For genuinely stale ids, `opencode --session <unknown>`
  exits 1 with `Session not found` in about a second, well inside the 10 s
  quick-fail auto-recovery window.

- **Every export in `plugin/jin.ts` must be a function.** The file has no
  default export, so opencode falls back to `getLegacyPlugins()`, which
  walks `Object.values(mod)` and throws `Plugin export is not a function`
  on the first export that is neither a function nor an object exposing
  `.server` (`packages/opencode/src/plugin/index.ts`). Adding one
  `export const VERSION = "1"` takes the entire plugin down, and opencode
  swallows a load failure as a warning — so the symptom is status silently
  never updating. The name `server` is conventional, not required: only
  the default-export path (npm-packaged plugins) validates names.

- **Subagents create child sessions, and their events must be dropped.**
  opencode's task tool calls `sessions.create({ parentID: … })`
  (`packages/opencode/src/tool/task.ts`), and the child publishes
  `session.created` / `session.status` / `session.idle` on the same bus as
  the parent. Forwarding them is actively harmful: `HandleHookEvent`
  re-keys `Session.AgentSessionID` on *any* hook whose `session_id`
  differs, so a child's `SessionStart` repoints the jin session at the
  subagent and breaks resume, and a child's idle reports the turn finished
  while the parent is still working. The plugin therefore reports on an
  **allow-list** of root sessions, never on "everything except known
  children" — see the next entry for why the deny-list shape is unsound.
  The allow-list is seeded from `JIN_OPENCODE_ROOT_SESSION` (set by
  `SpawnCommand` only when resuming) and extended by any `session.created`
  without a `parentID`, which covers a fresh spawn and `/new` mid-session.

- **Unknown session ids are resolved by asking opencode, not by guessing.**
  **Three** paths reach a session with no `session.created`: resuming with
  `--session`, switching sessions from the TUI (`/sessions`, and its
  `/resume` / `/continue` aliases, or `<leader>l`), and continuing a
  subagent through the task tool's `task_id`. An id arriving that way can
  be a root or a subagent, and both guesses are wrong in one of those
  cases — assuming "root" hands Manager a subagent id to re-key onto,
  assuming "child" silently freezes status after every session switch — so
  the plugin calls `client.session.get({ path: { id } })` once per unknown
  id and caches the answer. Lookup failures report nothing (fail-closed)
  and are deliberately not cached, so the next event retries rather than
  marking a real root permanently unreportable.

  Because opencode dispatches the hook as `void hook.event?.(...)`, handlers
  overlap; the plugin caches the in-flight promise, not just the result, so
  one turn's ~9 `session.status` events share a single lookup.

- **An opencode modal swallows every keystroke.** Some time after launch
  opencode raises an "Update Available — A new release vX.Y.Z is available"
  dialog that captures all keyboard input. While it is up, neither `tmux
  send-keys` nor `jin session send` can put a character in the prompt box;
  `Escape` dismisses it and input works again immediately. `jin session
  send` behaves correctly here — it retries, never sees the text land, and
  returns an error **without pressing Enter**, so a half-formed prompt is
  never committed. If a send fails with "the TUI may not have been ready to
  receive input", capture the pane before assuming the verify heuristic is
  at fault.

- **The plugin is a pure observer.** It subscribes via the `event` hook,
  not the `permission.ask` hook. Note that `permission.ask` (a `Hooks`
  interface key, which can rewrite the user's allow/deny decision) and
  `permission.asked` (a bus event type) are different things that both
  exist — the upstream docs list them in a way that invites conflating
  them. Status reporting must never sit on the permission decision path.

## Agent picker (TUI)

- **Picker initial selection is snapshot at create-popup launch, not on
  each keystroke.** The create-popup reads `JIN_UI_AGENT` (transient
  default from `jin ui --agent`) and `config.default_agent` when it
  starts. Editing `config.yaml` while the TUI is already open does not
  change what the picker preselects on the next `n` press until the
  create-popup process re-launches (which it does per `n` press, so a
  new session created after saving config picks up the new default).

- **`jin ui --agent <kind>` writes an outer-tmux env, not a config
  value.** Starting `jin ui` without `--agent` on the same outer-tmux
  server clears the env (`UnsetEnvironment`) so a stale value from a
  previous `--agent codex` invocation does not silently preselect Codex.

- **The picker step disappears when only one adapter is registered.**
  `stepAgent` is skipped based on `len(agent.Kinds()) < 2`. Both create
  → agent and fleet-step Esc-back short-circuit past it so the flow
  matches the pre-picker UX in single-adapter builds.

## Session filter popup (TUI)

- **`/` opens a tmux popup, it does not filter inline.** Unlike `vi` / `less`
  / most other TUI apps where `/` starts an inline incremental search, jind-ai
  binds `/` at the outer tmux (`jin-mgr`) root key table to launch the
  session filter popup (`jin session-filter-popup`). Muscle memory from other
  tools ("press `/`, type, see the list narrow in place") will instead pop
  open a full-screen picker — this is intentional (see
  [architecture.md](architecture.md#session-filter-popup)), not a bug.

- **Requires tmux `display-popup` (tmux 3.2+).** The session filter shares
  its launch mechanism with the action palette popup — both call
  `tmux display-popup -E`. On tmux 3.1 or older,
  `display-popup` doesn't exist, so the outer-tmux `bind-key` for `/` fires
  but the popup command errors out instead of opening. jind-ai's documented
  minimum is tmux 3.3+ (see README's Requirements section), which already
  covers this.

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

## Popup Sizes

- **Outer-tmux bind-key popups need a `jin ui` restart** to pick up config
  changes. `action` (default `M-p`) and `session_filter` (default `M-f`) are
  bound at outer tmux (`jin-mgr`) root with hardcoded `-w`/`-h` args written
  once at TUI startup (`applyActionPanelBinding` / `applySessionFilterBinding`
  in `cmd/jin/cmd/tui.go`). Changing `popups.action` or `popups.session_filter`
  in config takes effect only after `jin ui` re-runs and re-issues the
  `bind-key` command. Inner popups (opened from inside the BubbleTea update
  loop — `create`, `help`, and the palette-launched
  `session_filter`) read config on each open, so they don't need a restart.

- **Popup sizes are percent-only**. `popups.<name>.width` / `.height` are
  `int` values in the range 1-100 (interpreted as percent of the outer tmux
  client area). tmux itself accepts absolute cell counts (`80`, `40c`) but
  jind-ai does not — the schema is deliberately narrower.

- **Range violations behave asymmetrically** between user config and plugin
  manifests. User config out-of-range (e.g. `width: 150`) logs a warning
  and falls back to the default — a broken config never blocks the TUI.
  Plugin manifest out-of-range (`popup.width: 150` in `jin-plugin.yaml`)
  hard-fails `Validate()` and the plugin lands in `StateBroken` — a plugin
  author is expected to fix the manifest.

- **`keybindings.plugins.<name>.keys` (0.7.x shape) is dropped on 0.8.0**.
  0.8.0 replaced it with `keybindings.plugins.<name>.actions.<id>.keys` to
  match the multi-action manifest schema. At startup, jind-ai logs one
  `plugin keybindings config: %s uses deprecated v1 shape ...` WARN per
  affected plugin and drops that plugin's bindings — the TUI itself still
  starts, so this is not a hard failure, but the shortcuts stay silent
  until the config is rewritten. For a plugin with only a default action,
  `actions.default.keys: [...]` is the shortest translation. The 0.8.0
  release note in [CHANGELOG.md](../CHANGELOG.md) has a full before/after
  example.
