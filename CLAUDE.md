# jindaiko

CLI tool for managing multiple agent sessions (Claude Code as first-class) via tmux TUI.

## Build & Test

```
make build          # → bin/jin
make test           # go test -v ./...
make test-race      # go test -race ./...
make test-coverage  # Generate coverage report
make fmt            # go fmt ./...
make lint           # golangci-lint run ./...
make install        # go install ./cmd/jin
```

## Project Layout

```
cmd/jin/cmd/         Cobra CLI commands (root, daemon, session, tui, hook, ...)
internal/
  agent/             Agent adapter interface + kind registry
    claude/          Claude Code adapter (spawn cmd, hooks-settings, trust, transcript enhancer)
    register/        Blank-import package that Register()s every known kind at init()
  config/            Viper config management (~/.config/jindaiko/config.yaml)
  daemon/            Unix socket IPC server/client
  session/           Session lifecycle (agent-agnostic; owns Agent interface)
  tui/               BubbleTea TUI (largest codebase)
  tmux/              tmux -L jin session control
  notify/            Desktop notifications (macOS/Linux)
  transcript/        Claude Code transcript reader (~/.claude/projects/)
```

## Docs

See each file for details:

- [docs/architecture.md](docs/architecture.md) — Architecture, dependencies, data flow
- [docs/conventions.md](docs/conventions.md) — Coding conventions and patterns
- [docs/session-lifecycle.md](docs/session-lifecycle.md) — Session state transitions, creation, recovery
- [docs/ipc-protocol.md](docs/ipc-protocol.md) — IPC protocol spec and action list
- [docs/tui-guide.md](docs/tui-guide.md) — TUI development guide and adding views
- [docs/adding-commands.md](docs/adding-commands.md) — Adding new CLI commands
- [docs/gotchas.md](docs/gotchas.md) — Known pitfalls and caveats

## Debug

```
JIN_DEBUG=1 jin daemon start
```

Logs: `~/.local/state/jindaiko/daemon-debug.log`, `~/.local/state/jindaiko/hook-debug.log`

## Key Dependencies

Go 1.24.5 / cobra (CLI) / bubbletea (TUI) / viper (config) / lipgloss (styling)

## Data Directories

XDG Base Directory compliant. Defaults shown; override with `XDG_CONFIG_HOME`/`XDG_STATE_HOME`/`XDG_RUNTIME_DIR`.

```
~/.config/jindaiko/
  config.yaml                  User settings
~/.local/state/jindaiko/
  state.yaml                   Persistent state
  sessions/{uuid}.json         Session data
  hooks-settings.json          Generated Claude Code hooks settings
$XDG_RUNTIME_DIR/jindaiko/      (fallback $TMPDIR/jindaiko-<uid>/)
  daemon.sock                  IPC Unix socket
```

## Claude Code Hooks

Configured in `~/.claude/settings.json`:
- `UserPromptSubmit` → Set session to "thinking"
- `Stop` → Set to "idle" + task completion notification
- `Notification` → Set to "permission" + permission-waiting notification

See the "Claude Code Hooks Setup" section in README.md for details.

## Commit Convention

Commit messages follow Conventional Commits format (used by goreleaser for changelog generation):
- `feat:` New feature
- `fix:` Bug fix
- `refactor:` Refactoring
- `docs:` Documentation
- `test:` Tests
- `chore:` Other (CI, dependencies, etc.)

## Testing

Coverage ~40%. Uses only the standard library (no testify, etc.).
Same-package tests (`package X`) allow testing unexported functions.
Add `_test.go` files for new code.

The `tmux.Runner` interface (`internal/tmux/interfaces.go`) was introduced for testability.
Tests for session.Manager use `mockTmuxRunner` (`internal/session/mock_tmux_test.go`).
