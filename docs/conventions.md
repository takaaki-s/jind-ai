# Coding Conventions

## Language & Formatting

- Go 1.24.5
- Always run `make fmt` (go fmt) before committing
- Comments should be in English
- Technical terms, struct/function names remain in English

## Error Handling

- Propagate errors to the caller via return
- Log only at boundaries (daemon server, manager, etc.)
- Wrap with `fmt.Errorf("context: %w", err)`

## Debug Logging

debugEnabled / debugLog() are duplicated in the daemon and session packages (not shared).

```go
var debugEnabled = os.Getenv("JIN_DEBUG") == "1"

func debugLog(format string, args ...interface{}) {
    // Append to file: [HH:MM:SS] message
}
```

If a new package needs debug logging, duplicate the same pattern.

## Configuration Access

- Always access settings through `config.Manager`
- Do not call `viper` directly (outside the config package)
- `config.Manager` and `config.StateManager` are separate instances

## Concurrency

- `sync.RWMutex` field name is `mu`
- Lock ordering: session.Manager.mu is the central lock
- Perform I/O operations (Store.Save, transcript reads) outside the lock
  - Example: `List()` takes a snapshot under RLock, then reads transcripts after releasing the lock

## Naming

- Package names: singular (`session`, `daemon`, `tmux`)
- JSON tags: snake_case (`json:"work_dir"`)
- Runtime-only fields: `json:"-"` tag
- Constants: `StatusXxx` format (`StatusRunning`, `StatusIdle`)

## Struct Design

- `Session`: Persisted fields + runtime fields (`json:"-"`)
- `Info`: Read-only struct for external use (converted via `ToInfo()`)
- `Request`/`Response`: IPC messages (type flexibility via `json.RawMessage`)

## Agent Adapters

- New agent-specific behaviour must go through `session.Agent` (declared in
  `internal/session/agent_types.go`, re-exported as aliases from
  `internal/agent/agent.go`). Never introduce a new switch on `AgentKind`
  in the session or daemon package — the switch already exists inside the
  adapter's `StatusSource.Interpret` / `SpawnCommand`, and that's the only
  place agent-specific vocabulary is allowed to live.
- Register each adapter from `internal/agent/register/register.go` (blank
  import into `cmd/jin/cmd/root.go`). Do not register from init() inside
  the adapter package itself: that would create a hidden dependency edge.
- `internal/session/` MUST NOT import `internal/agent/*`. If a new
  cross-package need appears, extend the interface (or add a new one) in
  `session/agent_types.go`, then satisfy it from the adapter side.

## Plugin Manifest (popup declaration)

A plugin author can declare a preferred popup size for its `jin pane popup
--here` calls in `jind-ai-plugin.yaml`:

```yaml
schema_version: 1
name: my-notifier
version: 0.1.0
description: ...
jin: ">=0.7.0"
install:
  source:
    build: ["true"]
    entrypoint: ./notifier.sh
on: [status_changed]
popup:                # optional; percent int 1-100
  width: 40
  height: 20
```

Both `popup` fields are optional (unset means "no preference — dispatcher
falls through to the plugin_default"). Out-of-range values (e.g.
`width: 150`) are rejected by `pkg/plugin/manifest.Check` and land the
plugin in `StateBroken`. Users can override the manifest per-plugin in
their own config under `popups.plugins.<name>` — that path takes
precedence over the manifest. See
[architecture.md](architecture.md#popup-size-resolution) for the full
resolution chain.

## Testing

Coverage ~40%. Test files exist for all packages.
Uses only the standard library (no testify, etc.). Same-package tests (`package X`) allow testing unexported functions.
The `tmux.Runner` interface was introduced for testability.
Add `_test.go` files for new code.
