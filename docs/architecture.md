# Architecture

## Overview

jind-ai is a CLI/TUI tool that manages multiple interactive agent sessions on
tmux — Claude Code is the first-class citizen and, at present, the only
adapter shipped in-tree; other adapters (Codex CLI, Aider, …) plug in via
`internal/agent/<kind>/`. A daemon process provides IPC via Unix socket,
with CLI and TUI connecting as clients.

## Data Flow

```
User
 ├─ jin tui              ─┐
 ├─ jin session new       │  daemon.Client (Unix socket)
 ├─ jin session list      ├─────────────────────────────→ daemon.Server
 └─ jin session kill     ─┘                                   │
                                                              ▼
                                                      session.Manager
                                                         │       │
                                                         ▼       ▼
                                                    tmux.Client  Store (JSON)
                                                         │
                                                         ▼
                                                   tmux -L jin
                                                    (inner tmux)
                                                         │
                                                         ▼
                                                   claude / claude --resume
```

## Switch Session Popup

Fuzzy session picker, launched as a tmux popup — same shape as the action
palette (`jin action-popup`, see `cmd/jin/cmd/tui.go`'s
`applyActionPanelBinding`/`applySessionFilterBinding` pair): a hidden CLI
subcommand runs a standalone bubbletea program, and hands its result back to
the parent TUI through an outer-tmux environment variable rather than direct
IPC.

```
[outer tmux, user presses "/"]
  → display-popup -E 'jin session-filter-popup'
    → [popup process] jin session-filter-popup
        ├─ daemon.Client.List() → []session.Info
        ├─ bubbletea + sahilm/fuzzy (SessionFilterModel)
        └─ on Enter: tmux env JIN_FOCUS_SESSION = selected session ID
    → [parent TUI, next envTick (~250ms)]
        consume("JIN_FOCUS_SESSION") → m.focusSessionID
    → [parent TUI, next sessionsMsg]
        switchToSession(id) → RespawnPane (immediate attach)
```

`Esc`/`Ctrl+C` dismisses the popup without writing `JIN_FOCUS_SESSION`, so a
cancelled pick leaves the parent TUI's cursor and attached session
untouched. Implementation: `internal/tui/session_filter_model.go`
(`SessionFilterModel`, fuzzy ranking via
[sahilm/fuzzy](https://github.com/sahilm/fuzzy)) +
`cmd/jin/cmd/session_filter_popup.go` (CLI entry point). No new daemon IPC
or persisted state — `daemon.Client.List()` is the same call the parent
TUI's own polling already uses. `JIN_FOCUS_SESSION` shares the
`focusSessionID → switchToSession` consume path already used by
`JIN_CREATED_SESSION` / `JIN_NOTIFY_SESSION` (`internal/tui/model.go`); see
[tui-guide.md](tui-guide.md#switch-session-popup) for keybindings and the
matched-field list.

## Popup Size Resolution

Every popup opened by jind-ai has its width and height resolved from a
single config schema. Two delivery paths carry the resolved size to tmux:

**Core popups** (`create`, `session_filter`, `help`, `action`, `confirm`):

```
Model.openPopup(name)                  cmd/jin/cmd/tui.go
  └─ Manager.GetPopupSize(name)         apply*Binding (bind-key)
       ├─ user config popups.<name>       └─ Manager.GetPopupSize(name)
       └─ hardcoded DefaultPopupSizes         ↓
                                          tmux bind-key display-popup -w W -h H
                                          (written once at `jin ui` startup —
                                          config changes need a TUI restart)
```

**Plugin popups** (dispatcher → plugin script → `jin pane popup --here`):

```
[daemon startup]
  configMgr + plugin.NewDispatcher(..., resolverClosure)
                                     ↑
                                     └─ wraps GetPluginPopupSize

[dispatch]
  d.run(entry, action)
    ├─ d.popupResolver(name, action.ID, action.Popup)
    │    priority: user config popups.plugins[name]     # per-action popup config
    │            > action.Popup (v2 manifest per-action) # is out of scope this release,
    │            > user config popups.plugin_default    # so actionID is currently
    │            > hardcoded plugin_default              # ignored by the resolver
    ├─ ExecOptions.PopupWidth/Height ← resolved
    └─ buildEnv: JIN_PLUGIN_POPUP_{WIDTH,HEIGHT} exported
                   ↓
[plugin script calls] jin pane popup --here [--width W] [--height H] -- CMD
                                            ↑
                                            └─ flag overrides env; env is
                                               the fallback dispatcher injected.
                                               Empty in both → tmux default.
```

Import boundary: `internal/plugin` does not depend on `internal/config`.
The resolver is passed as a `PopupSizeResolver` callback at daemon startup;
`internal/daemon/server.go` converts between `plugin.PopupConfig` and
`config.PopupSizeConfig` at the boundary. The resolver signature carries
the action ID so a later config schema can widen to per-action popup size
without another breaking signature change; today the daemon-side resolver
ignores it and treats popup size as a plugin-level knob.

See [tui-guide.md](tui-guide.md#popup-sizes) for the config schema and
defaults, and [conventions.md](conventions.md#plugin-manifest-popup-declaration)
for the plugin author's side (`popup:` field in `jin-plugin.yaml`).

## Hook Flow (State Detection)

```
Claude Code (hook event)
  → jin hook (cmd/jin/cmd/hook.go)
    → daemon.Client.Send("hook", HookRequest)
      → daemon.Server.handleHook()
        → session.Manager.HandleHookEvent()
          → agent.StatusSource.Interpret()  ── adapter-owned event→status mapping
          → Session.Status update + Store.Save + plugin dispatch (see below)
```

The event vocabulary (which hook name means what status) lives entirely in
the agent adapter — `HandleHookEvent` itself is agent-agnostic wiring. Each
adapter owns the mapping and any needed vocabulary normalisation.

**Canonical events** the manager's side effects key on
(`SessionStart` → `AgentSessionStarted` bookkeeping + stale-stop correction
+ Layer C trigger, `UserPromptSubmit` / `Stop` → Layer C trigger,
`CwdChanged` → git branch reprobe). Adapters whose native events do not
match these names must normalise before calling `jin hook` — Codex needs no
normalisation (its native events already match Claude Code by name; see
`internal/agent/codex/status.go` and the exact mapping below), while
`internal/agent/opencode/plugin/jin.ts` is a plugin-side normaliser
(opencode's bus vocabulary shares no names with the canonical set).

The Claude Code adapter's mapping is documented in
`internal/agent/claude/status.go`; typical entries are:

- `UserPromptSubmit` → StatusThinking + Layer C Description upgrade attempt (may pick up a stronger CC-authored name if CC has renamed the session since SessionStart)
- `Stop` → StatusIdle + task-complete notification
- `Notification(permission_prompt)` → StatusPermission + permission notification

The Codex adapter's mapping mirrors the same shape, with one deliberate
divergence:

- `UserPromptSubmit` / `PreToolUse` / `PostToolUse` → StatusThinking
- `PermissionRequest` → StatusThinking, no notification
- `Stop` → StatusIdle + task-complete notification

`PermissionRequest` does not map to StatusPermission despite the name: nothing
reaching the adapter reports that a human was asked. Why that follows from the
payload, what it gives up (a codex session never reaching `permission`), and
what has not been measured are under "Codex adapter" in
[gotchas.md](gotchas.md#codex-adapter).

The opencode adapter consumes the same canonical names, but they are
produced by the bundled plugin rather than by the agent. The plugin
subscribes to opencode's `event` bus hook and translates:

- `session.created` → `SessionStart` (carries the real `ses_…` id)
- `session.status{type != idle}` → `UserPromptSubmit`
- `session.idle` → `Stop`
- `permission.asked` / `permission.replied` → `PermissionRequest` / `UserPromptSubmit`
- `session.error` → `StopFailure`

Consecutive duplicates are suppressed plugin-side: opencode publishes
`session.status{busy}` once per step (~9 times for a trivial turn), and
collapsing them keeps one turn to one status report.

## Plugin Event Flow

Every status transition `HandleHookEvent` commits also fans out to installed
plugins, asynchronously and fail-open (a stuck or failing plugin never blocks
the status pipeline):

```
session.Manager.HandleHookEvent()  (status actually changed)
  → plugin.Dispatcher.Publish(Event{status_changed, notify_kind, ...})   (returns immediately)
    → for each enabled plugin:
        for each manifest.Actions[i]:                # per-action match/debounce/run
          if action.On matches event:
            go exec.ExecPlugin(entry, action, ...)   # background goroutine,
                                                        debounce key includes action ID,
                                                        JIN_* env (incl. JIN_ACTION_ID)
                                                        + stdin JSON
```

The Event carries the adapter-determined notification kind (`notify_kind` /
`JIN_NOTIFY_KIND`: task-complete / error / permission, empty when none) so
plugins never re-derive notification semantics from status pairs. Actions
match and debounce independently, so a single event can fan out to
multiple actions on the same plugin without any interfering with the
others.

`jin plugin run <name> [action] [--session <id>]` takes a separate,
synchronous-trigger path (`Dispatcher.RunAction(name, actionID, ev, depth,
actx)`) that skips matcher/debounce but shares the same `ExecPlugin`
runner. An empty `actionID` selects the default action (`actions[0]`); an
unknown ID returns a synchronous error that lists the available actions.
Without `--session` it dispatches a global action with empty session
fields, carrying the invoking CLI's tmux context
(`JIN_CALLER_TMUX_SOCKET`/`JIN_CALLER_TMUX_PANE`) when available. See
`internal/plugin/` (`manifest.go`, `dispatcher.go`, `exec.go`) and
[ipc-protocol.md](ipc-protocol.md) for the `plugin-run` / `pane-*`
actions plugins use as their CLI-facing API.

## Agent Adapters

Every session records an `AgentKind` (`"claude"`, `"codex"` and
`"opencode"` today). The
Manager fetches the concrete adapter through the `session.AgentResolver`
interface:

```
internal/session/       owns Agent + AgentResolver interfaces (no import of agent/)
internal/agent/         re-exports the same types as aliases; hosts registry
internal/agent/register blank-import → wires every kind into the registry at init()
internal/agent/claude/  Claude Code adapter: SpawnCommand / StatusSource / Setup
                         (hooks-settings.json + the ~/.claude.json trust entry),
                         Description
                         (Layer C enhancer over ~/.claude/sessions/<PID>.json)
internal/agent/codex/   Codex CLI adapter: SpawnCommand appends `--enable hooks
                         + -c 'hooks.X=[...]'` per invocation (Setup writes no
                         files), Description (Layer C-transcript enhancer over
                         $CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<UUID>.jsonl)
internal/agent/opencode/ opencode adapter: Setup materialises an embedded
                         TypeScript plugin at <StateDir>/opencode/plugin/jin.ts,
                         plus opencode.json + jin-agent.md in the same directory
                         (an `instructions` entry carrying the agent-facing
                         context — opencode's plugin discards `jin hook` stdout,
                         so the hook route the other adapters use cannot reach
                         it). SpawnCommand points opencode at the directory via
                         OPENCODE_CONFIG_DIR. Transcript shells out to
                         `opencode export --pure <ses_id>` — opencode's own
                         conversation lives in SQLite, and jind-ai stores
                         nothing of its own; `--pure` keeps the read from
                         loading the plugin. It is the one reader that is NOT a
                         session.PollableTranscriptSource (1.45-1.77s per call),
                         so a preview path on a timer must skip it. No
                         Description enhancer.
internal/agent/textutil.go  Cross-adapter helper — currently `SmartTruncate`
                         shared by claude and codex description enhancers.
```

Design principle: **adapters must not write to user-global config where the
agent gives them any other channel**. The Codex adapter injects hooks
per-invocation via `-c` overrides rather than touching `~/.codex/hooks.json`
or `~/.codex/config.toml`. This keeps uninstalls automatic (no residue) and
guarantees the user's own hooks are never accidentally clobbered by a stale
merge. The Claude Code adapter's generated `hooks-settings.json` lives under
jind-ai's own state directory for the same reason. The opencode adapter
follows it too: its plugin goes under jind-ai state and is reached through
`OPENCODE_CONFIG_DIR`, which opencode *appends* to its config search path —
the user's `~/.config/opencode` and any project `.opencode` keep loading
untouched.

The qualifier is load-bearing, and there is exactly one place it applies:
Claude Code's workspace trust flag, which the CLI reads only from
`projects[<dir>].hasTrustDialogAccepted` in `~/.claude.json`. No settings
file carries it and `--settings` injection cannot supply it, so the choice
is between writing there and leaving every session stuck on a trust dialog
in a pane nobody is watching. That write is deliberately the narrowest thing
that works — one boolean, merged into a file decoded as raw JSON so no other
key is touched, skipped entirely when a trusted ancestor already covers the
directory. **Do not read this as licence for the next adapter.** Reach for
it only after establishing that the agent offers no per-invocation or
injected channel, and record how you established it. See the Claude Code
adapter section of [gotchas.md](gotchas.md) for what that write costs.

### Adapter kind resolution

The kind sent to the daemon on session creation is resolved in this order:

1. Explicit `--agent <kind>` flag (`jin session new`, or the TUI create
   form's agent picker step)
2. `config.default_agent` (`~/.config/jind-ai/config.yaml`)
3. Hard-coded `"claude"` fallback

The daemon backfills the empty case and validates the final value against
the registry in `internal/daemon/server.go`. The TUI create form runs the
same resolution client-side to seed the picker's initial selection; the
`jin ui --agent <kind>` flag is a **transient** default propagated to the
create-popup via the outer-tmux env `JIN_UI_AGENT` (unset when no flag
was passed), so it never modifies `config.default_agent`. The picker step
itself is skipped when only one adapter is registered.

The daemon injects a thin resolver (`agent.Lookup`) into `session.Manager`,
so `internal/session/` never imports `internal/agent/*`. Adding a new
adapter is a matter of dropping `internal/agent/<kind>/` with an
implementation and adding one line to `internal/agent/register/register.go`.

## Session Description Model

Sessions carry a human-readable `Description` field decoupled from the technical `ID`. It is filled by a 3-layer generation pipeline (see [session-lifecycle.md](session-lifecycle.md) for the state machine):

- **Layer A (baseline)** — Always populated at creation from `<repo>[:<branch>][:<subpath>]`, or `<main-repo>:<worktree-name>` for worktree sessions. Agent-independent. Implemented in `internal/session/description.go`.
- **Layer B (manual)** — CLI `--description` on `jin session new` and the `jin session set-description` subcommand. Sets `DescriptionLocked = true` to freeze the value against auto-upgrade. (The TUI create form intentionally does not expose a manual description step: Layer A + Layer C cover the common case; users who need a manual label use the CLI paths.)
- **Layer C (agent-specific enhancer)** — Returned by the adapter through `Agent.Description() DescriptionEnhancer`; `HandleHookEvent` looks it up from the resolved adapter on every `SessionStart` / `UserPromptSubmit` / `Stop` hook (Manager holds no separate enhancer field). Enhancers return a `(candidate, DescriptionLayer, ok)` tuple; the Manager's `TryUpgradeDescription` only accepts a strictly higher `DescriptionLayer` than the session's current layer, so lower-quality signals never overwrite a better one. The Claude Code implementation (`internal/agent/claude/`) tries two signals in order of informativeness, splitting Layer C-name into two sub-layers:
  - **Layer C-name (strong)** (`DescriptionLayerAgentName`) — first tries the AI-generated session title CC writes to the transcript as `{"type":"ai-title","aiTitle":"…"}` (the same value CC shows next to "Session name" in `/status`). If no `aiTitle` is present, falls back to the name in `~/.claude/sessions/<PID>.json` when `nameSource` is anything other than `"derived"` (or the field is absent on older CC versions).
  - **Layer C-name (weak)** (`DescriptionLayerAgentNameDerived`) — the `name` field in `~/.claude/sessions/<PID>.json` with `nameSource="derived"`: the tmux window hint jind-ai itself handed CC round-tripped back to disk (e.g. `jin-395bce5c-71`). Better than the Layer A baseline because it matches CC's own `/resume` picker, but any stronger name (later `aiTitle`, `/rename`, …) is allowed to overwrite it.

  The CC adapter intentionally does NOT mine the raw first user prompt for a Layer C description. Claude Code owns the naming and eventually replaces the derived hint with `aiTitle`, so pulling the prompt text into `Description` would just clobber that CC-native title with what the user typed. `DescriptionLayerTranscript` is reserved for adapters that lack a native session-name path.

  The Codex adapter (`internal/agent/codex/`) is the first user of Layer C-transcript. Codex 0.144.1 stores a `title` in `~/.codex/state_5.sqlite`, but empirically the CLI never populates it with an AI summary — `title` always equals the first user message — so the adapter reads the first genuine user prompt straight from the rollout JSONL and returns it at `DescriptionLayerTranscript`. `<environment_context>` and other Codex-injected pseudo-user rows are filtered out before the first real turn is used. See `internal/agent/codex/description.go` for the implementation.

  Future agents (Aider, …) plug in the same way — return an enhancer from `Description()` (any subset of layers) and the plumbing works for them unchanged.

The `Name` field was retired in favour of `Description` + `DescriptionLocked`. Existing session JSON is migrated in place on daemon startup by `internal/session/migration.go`.

## Package Dependency

```
cmd/jin/cmd/       → daemon (client), config, session (types only), tui, tmux, plugin,
                       agentdocs,
                       _ agent/register (blank import so kinds are registered)
                      │
daemon/            → session, config, tmux, agent (registry Lookup), plugin
                      │
session/           → config, tmux, transcript, plugin (Dispatcher seam only)
                      │
agent/             → session (borrows Agent + supporting types via aliases)
agent/claude/      → agent, session, transcript, debug, agentdocs   (CC-specific adapter)
agent/codex/       → agent, session, transcript, agentdocs (Codex-specific adapter)
agent/opencode/    → agent, session, transcript, procgroup, debug, agentdocs (opencode adapter)
agent/register/    → agent, agent/claude, agent/codex, agent/opencode  (init-time Register)
                      │
procgroup/         → (stdlib only) run a child in its own process group so a
                      cancelled context reaches everything it started.
                      Used by agent/opencode, plugin and worktreehook.
worktreehook/      → procgroup (runs .jin/worktree-post-create.sh)
agentdocs/         → (embedded content only; no internal deps)
                      Being a leaf is what lets cmd/ and every adapter share it:
                      the docs served by `jin docs`, the skill `jin init` offers,
                      the context injected into child sessions, and HookCommand —
                      the single owner of the `--emit-context` hook flag.
                      │
plugin/            → config (PluginsConfig), debug   (no import of tmux/ or session/)
                      │
tui/               → daemon (client), config, session (Info type)
                      │
config/            → (external: viper)
tmux/              → (external: tmux CLI)
transcript/        → (file I/O: ~/.claude/projects/)
paths/             → (XDG dirs: config/state/data/runtime)
```

config is a foundational package referenced by many others.
The session package is the core domain with the most business logic.

`session/` depends only on `plugin.Dispatcher`, the small interface it
publishes events through (`internal/plugin/interfaces.go`) — the concrete
`plugin.EventDispatcher` is constructed and injected by `daemon/` at startup
(`Manager.SetPluginDispatcher`), the same seam pattern used for `tmux.Runner`.
`plugin/` itself never imports `tmux/` or `session/`: pane operations a
plugin requests go through `jin pane` (daemon IPC → `session.Manager`), not
through the plugin package directly.

## File Storage

jind-ai follows the [XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/):

```
$XDG_CONFIG_HOME/jind-ai/        (default: ~/.config/jind-ai)
  └─ config.yaml                 ... User settings (keybindings, plugins config)

$XDG_STATE_HOME/jind-ai/         (default: ~/.local/state/jind-ai)
  ├─ state.yaml                  ... Persistent state (StateManager)
  ├─ sessions/
  │   └─ {uuid}.json             ... Session persistence data
  ├─ hooks-settings.json         ... Generated Claude Code hooks settings
  ├─ plugins.lock.yaml           ... Installed-plugin ledger (source, ref, commit SHA, linked)
  ├─ plugin-logs/
  │   ├─ {name}.log              ... Per-plugin dispatch/run output (append-only)
  │   └─ {name}-build.log        ... Per-plugin install/update build output
  ├─ daemon-debug.log            ... Daemon debug log
  ├─ hook-debug.log              ... Hook debug log
  └─ plugin-debug.log            ... Plugin dispatcher debug log

$XDG_DATA_HOME/jind-ai/          (default: ~/.local/share/jind-ai)
  └─ plugins/
      └─ {name}/                 ... Installed plugin (git clone, or symlink for --link)
          └─ jin-plugin.yaml     ... Plugin manifest

$XDG_RUNTIME_DIR/jind-ai/        (fallback: $TMPDIR/jind-ai-<uid>)
  └─ daemon.sock                 ... Unix domain socket
```
