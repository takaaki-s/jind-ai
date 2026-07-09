# Architecture

## Overview

jindaiko is a CLI/TUI tool that manages multiple interactive agent sessions on
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

## Hook Flow (State Detection)

```
Claude Code (hook event)
  → jin hook (cmd/jin/cmd/hook.go)
    → daemon.Client.Send("hook", HookRequest)
      → daemon.Server.handleHook()
        → session.Manager.HandleHookEvent()
          → agent.StatusSource.Interpret()  ── adapter-owned event→status mapping
          → Session.Status update + Store.Save + notify
```

The event vocabulary (which hook name means what status) lives entirely in
the agent adapter — `HandleHookEvent` itself is agent-agnostic wiring. The
Claude Code mapping is documented in `internal/agent/claude/status.go`; the
common cases are:

- `UserPromptSubmit` → StatusThinking + Layer C Description upgrade attempt (may pick up a stronger CC-authored name if CC has renamed the session since SessionStart)
- `Stop` → StatusIdle + task-complete notification
- `Notification(permission_prompt)` → StatusPermission + permission notification

## Plugin Event Flow

Every status transition `HandleHookEvent` commits also fans out to installed
plugins, asynchronously and fail-open (a stuck or failing plugin never blocks
the status pipeline):

```
session.Manager.HandleHookEvent()  (status actually changed)
  → plugin.Dispatcher.Publish(Event{status_changed, notify_kind, ...})   (returns immediately)
    → for each matching, enabled plugin: exec.ExecPlugin()  (background goroutine,
                                                               debounced, JIN_* env + stdin JSON)
```

The Event carries the adapter-determined notification kind (`notify_kind` /
`JIN_NOTIFY_KIND`: task-complete / error / permission, empty when none) so
plugins never re-derive notification semantics from status pairs.

`jin plugin run <name> [--session <id>]` takes a separate, synchronous-trigger
path (`Dispatcher.RunAction`) that skips matcher/debounce but shares the same
`ExecPlugin` runner; without `--session` it dispatches a global action with
empty session fields, carrying the invoking CLI's tmux context
(`JIN_CALLER_TMUX_SOCKET`/`JIN_CALLER_TMUX_PANE`) when available. See `internal/plugin/` (`manifest.go`, `dispatcher.go`,
`exec.go`) and [ipc-protocol.md](ipc-protocol.md) for the `plugin-run` /
`pane-*` actions plugins use as their CLI-facing API.

## Agent Adapters

Every session records an `AgentKind` (`"claude"` today; more later). The
Manager fetches the concrete adapter through the `session.AgentResolver`
interface:

```
internal/session/       owns Agent + AgentResolver interfaces (no import of agent/)
internal/agent/         re-exports the same types as aliases; hosts registry
internal/agent/register blank-import → wires every kind into the registry at init()
internal/agent/claude/  Claude Code adapter: SpawnCommand / StatusSource / Setup
                         (hooks-settings.json + trust-dialog files), Description
                         (Layer C enhancer over ~/.claude/sessions/<PID>.json)
```

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
  - **Layer C-name (weak)** (`DescriptionLayerAgentNameDerived`) — the `name` field in `~/.claude/sessions/<PID>.json` with `nameSource="derived"`: the tmux window hint jindaiko itself handed CC round-tripped back to disk (e.g. `jin-395bce5c-71`). Better than the Layer A baseline because it matches CC's own `/resume` picker, but any stronger name (later `aiTitle`, `/rename`, …) is allowed to overwrite it.

  The CC adapter intentionally does NOT mine the raw first user prompt for a Layer C description. Claude Code owns the naming and eventually replaces the derived hint with `aiTitle`, so pulling the prompt text into `Description` would just clobber that CC-native title with what the user typed. `DescriptionLayerTranscript` is reserved for future adapters that lack a native session-name path.

  Future agents (Codex, Aider, …) plug in the same way — return an enhancer from `Description()` (any subset of layers) and the plumbing works for them unchanged.

The `Name` field was retired in favour of `Description` + `DescriptionLocked`. Existing session JSON is migrated in place on daemon startup by `internal/session/migration.go`.

## Package Dependency

```
cmd/jin/cmd/       → daemon (client), config, session (types only), tui, tmux, plugin,
                       _ agent/register (blank import so kinds are registered)
                      │
daemon/            → session, config, notify, tmux, agent (registry Lookup), plugin
                      │
session/           → config, tmux, notify, transcript, plugin (Dispatcher seam only)
                      │
agent/             → session (borrows Agent + supporting types via aliases)
agent/claude/      → agent, session, transcript, debug   (CC-specific adapter)
agent/register/    → agent, agent/claude (init-time Register)
                      │
plugin/            → config (PluginsConfig), debug   (no import of tmux/ or session/)
                      │
tui/               → daemon (client), config, session (Info type)
                      │
config/            → (external: viper)
tmux/              → (external: tmux CLI)
notify/            → (external: OS notification)
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

jindaiko follows the [XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/):

```
$XDG_CONFIG_HOME/jindaiko/        (default: ~/.config/jindaiko)
  └─ config.yaml                 ... User settings (keybindings, plugins config)

$XDG_STATE_HOME/jindaiko/         (default: ~/.local/state/jindaiko)
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

$XDG_DATA_HOME/jindaiko/          (default: ~/.local/share/jindaiko)
  └─ plugins/
      └─ {name}/                 ... Installed plugin (git clone, or symlink for --link)
          └─ jin-plugin.yaml     ... Plugin manifest

$XDG_RUNTIME_DIR/jindaiko/        (fallback: $TMPDIR/jindaiko-<uid>)
  └─ daemon.sock                 ... Unix domain socket
```
