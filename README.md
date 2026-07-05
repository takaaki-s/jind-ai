**English** | [日本語](README.ja.md)

# honjin

A CLI tool for running and managing multiple Claude Code sessions simultaneously.

https://github.com/user-attachments/assets/62e9d64a-aa7d-42f8-8edf-03f724fe0ee4

## Features

- **Multi-session management**: Run multiple Claude Code sessions in the background simultaneously
- **tmux-native**: Each session runs in its own tmux pane, so your existing `~/.tmux.conf`, custom keybindings, status bar, and copy-mode setup work as-is
- **Decoupled UI / logic architecture**: All session management, state transitions, and hook handling live in the daemon. The TUI is a thin client that talks to the daemon over a Unix socket and holds no session-management logic. In principle any alternate UI (web, editor extension, ...) can drive the same IPC (see [docs/architecture.md](docs/architecture.md) / [docs/ipc-protocol.md](docs/ipc-protocol.md))
- **TUI**: Interactive terminal UI for listing, monitoring, and operating sessions
- **Attach/Detach**: Quickly switch between sessions (`Ctrl+]` to detach)
- **Real-time status tracking**: Live display of working directory, branch, and latest message
- **Search & Paging**: Incremental search by session name, directory, or branch
- **Desktop notifications**: OS notifications for permission requests and task completion (macOS / Linux)

## Installation

### Download from GitHub Releases

Download the binary for your OS/architecture from the [Releases page](https://github.com/takaaki-s/honjin/releases).

```bash
# Example: Linux amd64
curl -Lo honjin.tar.gz https://github.com/takaaki-s/honjin/releases/latest/download/honjin_0.1.0_linux_amd64.tar.gz
tar xzf honjin.tar.gz
sudo mv jin /usr/local/bin/
```

### Go install

```bash
go install github.com/takaaki-s/honjin/cmd/jin@latest
```

### Build from source

```bash
git clone https://github.com/takaaki-s/honjin.git
cd honjin
make build    # Build to bin/jin
make install  # Install to $GOPATH/bin
```

## Quick Start

### 1. Start the daemon

```bash
jin daemon start
```

### 2. Launch the TUI

```bash
jin ui
```

### 3. Create and attach to a session

Press `n` in the TUI to create a session, then `Enter` to attach.

Press `Ctrl+]` to detach and return to the TUI.

## Session Status

Session states are detected via Claude Code [hooks](https://docs.anthropic.com/en/docs/claude-code/hooks) in an event-driven manner.

| Status | Icon | Detection | Description |
|--------|------|-----------|-------------|
| `thinking` | ⚡ | `UserPromptSubmit` hook | Processing |
| `permission` | ? | `Notification` hook | Awaiting permission |
| `running` | ▶ | Internal | Running |
| `creating` | + | Internal | Creating (CC starting up) |
| `idle` | ○ | `Stop` hook | Waiting for input |
| `stopped` | ■ | Process death detection | Stopped |

## CLI Commands

### Daemon management

```bash
jin daemon start   # Start daemon
jin daemon stop    # Stop daemon
jin daemon status  # Check status
```

### Session management

```bash
# Create session (interactive via TUI - recommended)
jin session new

# Create session (specify working directory)
jin session new --workdir ~/repos/myrepo

# List sessions
jin session list

# List sessions in JSON format (for scripting / LLM integration)
jin session list --json

# Attach to a session
jin session attach <session-name>

# Get session details
jin session info <session-name>

# Send a prompt to a session
jin session send <session-name> "your prompt here"

# Wait for a session to become idle (default timeout: 300s)
jin session wait <session-name>
jin session wait <session-name> --timeout 60

# Get the last assistant message
jin session output <session-name>

# Get the last N conversation pairs
jin session output <session-name> --last 3

# Kill a session
jin session kill <session-name>

# Delete a session
jin session delete <session-name>

# Bulk delete stopped sessions
jin cleanup stopped
jin cleanup stopped --dry-run   # Preview what will be deleted
```

> **Aliases**: `session` can be shortened to `sess` (e.g., `jin sess list`). `list` to `ls`, `delete` to `rm`.

### LLM API (scripting / automation)

The following commands support `--json` for structured output, enabling integration with scripts and other LLM agents.

```bash
# All session commands support --json
jin session list --json
jin session new --workdir ~/repos/myrepo --json
jin session info <session-name> --json
jin session kill <session-name> --json

# Send a prompt and wait for completion
jin session send <session-name> "fix the failing test" --json
jin session wait <session-name> --timeout 120 --json
jin session output <session-name> --json

# Pipeline example: send a prompt, wait, get output
jin session send my-session "refactor main.go"
jin session wait my-session --timeout 300
jin session output my-session --last 1
```

#### Orchestration: parent Claude driving child sessions

For richer orchestration (e.g. a parent Claude that needs to inspect what a
child actually *did*, not just the assistant's text), use `session result`.
It returns structured `tool_use` / `tool_result` / `thinking` blocks parsed
directly from the Claude Code transcript JSONL — no tmux pane scraping, no
truncation by scrollback buffer.

```bash
# Send a prompt, wait until the child stops (idle OR waiting on permission),
# then fetch what it actually did.
jin session prompt my-session "run go test ./... and report failures"
jin session wait my-session --until idle,permission --timeout 600
jin session result my-session --json | jq '.entries[].blocks[] | select(.kind=="tool_result")'

# Incremental fetch: only entries after a checkpoint.
# --since is strictly exclusive: pass the last entry's timestamp to receive
# only entries that came after it (no duplicates). Claude Code emits timestamps
# with millisecond precision (e.g. "2026-04-09T13:23:10.456Z").
T1=$(jin session result my-session --json | jq -r '.entries[-1].timestamp')
jin session prompt my-session "now also run go vet"
jin session wait my-session --until idle,permission
jin session result my-session --since "$T1" --json

# Filter to a specific tool, or to errors only
jin session result my-session --tool Bash --json
jin session result my-session --errors-only --json
```

`prompt` is an alias for `send`.

#### Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | General error |
| 2 | Session not found |
| 3 | Daemon not running |
| 4 | Timeout (`session wait`) |

### Utilities

```bash
jin session workdir <session-name>    # Print session's working directory path
jin session edit <session-name>       # Open session's working directory in EDITOR
```

The following shell functions are useful:

```bash
# cd to a session's working directory
cc-cd() { cd "$(jin session workdir "$1")"; }

# Select a session with fzf and cd to its working directory
cc-cdf() {
  local session
  session=$(jin session list | tail -n +2 | fzf --height 40% --reverse | awk '{print $1}')
  [[ -n "$session" ]] && cd "$(jin session workdir "$session")"
}

# Select a session with fzf and attach
cc-attach() {
  local session
  session=$(jin session list | tail -n +2 | fzf --height 40% --reverse | awk '{print $1}')
  [[ -n "$session" ]] && jin session attach "$session"
}
```

### Shell Completion

```bash
# bash
source <(jin completion bash)

# zsh
source <(jin completion zsh)

# fish
jin completion fish | source
```

## Configuration

honjin follows the [XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/). Files are split across config / state / runtime directories:

```
$XDG_CONFIG_HOME/honjin/      (default: ~/.config/honjin)
└── config.yaml                # Configuration file

$XDG_STATE_HOME/honjin/       (default: ~/.local/state/honjin)
├── state.yaml                 # State file (last used repository, etc.)
├── sessions/                  # Session data
├── hooks-settings.json        # Generated hooks settings (auto-managed)
├── daemon-debug.log           # Daemon debug log (when JIN_DEBUG=1)
└── hook-debug.log             # Hook debug log (when JIN_DEBUG=1)

$XDG_RUNTIME_DIR/honjin/      (fallback: $TMPDIR/honjin-<uid>)
└── daemon.sock                # Daemon socket
```

### Example configuration (`~/.config/honjin/config.yaml`)

```yaml
# Customize keybindings (defaults are used when omitted)
keybindings:
  # Session list view
  up: ["up", "k"]
  down: ["down", "j"]
  attach: ["enter"]
  new: ["n"]
  kill: ["x"]
  delete: ["d"]
  refresh: ["r"]
  search: ["/"]
  vscode: ["v"]
  notifications: ["!"]
  quit: ["q", "ctrl+c"]
  help: ["?"]
  # Session creation form
  next_field: ["tab"]
  prev_field: ["shift+tab"]
  submit: ["enter"]
  cancel_form: ["esc"]
  # While attached
  detach: ["ctrl+]"]  # Default: ctrl+]
                       # Supported keys: ctrl+^, ctrl+], ctrl+\, ctrl+g
```

### Worktree placement

By default, `jin session new --worktree` creates worktrees under `$XDG_STATE_HOME/honjin/worktrees/{name}` (typically `~/.local/state/honjin/worktrees/`). Override this with `worktree.base_dir` in `config.yaml`:

```yaml
worktree:
  # Group worktrees per repository under a stable location
  base_dir: "${HOME}/ghq/worktrees/{repo}/{name}"
```

Other common layouts:

```yaml
# Sibling directory next to each repo checkout
worktree:
  base_dir: "${HOME}/dev/worktrees/{name}"

# Under a fixed root, ignoring repo name
worktree:
  base_dir: "/mnt/fast/worktrees/{name}"
```

Template variables:

| Placeholder | Expands to |
|-------------|------------|
| `{name}` | Worktree name (e.g. `jin-abcd1234`, or the `--name` you passed) |
| `{repo}` | Basename of the original repository |
| `${VAR}` | Environment variable (`os.ExpandEnv` semantics) |

The expanded path must be absolute. Unknown `{xxx}` placeholders are rejected at session creation.

### Worktree branch naming

Every worktree gets a companion branch. Two `worktree:` settings control how it is named:

```yaml
worktree:
  branch_prefix: "topic/"   # Default: "jin/". Use "" to drop the prefix.
  default_branch: "main"    # Fallback base branch. Default: "" (no fallback).
```

- **`branch_prefix`** — prepended to the auto-derived worktree name to form the branch name. The leading `jin-` on the worktree name is stripped first, so under the default `jin-abcd1234` becomes `jin/abcd1234` (not `jin/jin-abcd1234`). Ignored when you pass `--worktree-branch <name>` to `jin session new`, since that overrides the branch outright.
- **`default_branch`** — used **only** when honjin cannot auto-detect the repository's default branch. Detection reads `refs/remotes/origin/HEAD`; local clones that never had it set (some tarballs, `git clone --no-checkout`, older clones) will hit the fallback. If detection fails and `default_branch` is empty, session creation errors with `cannot detect default branch`.

Worktree creation itself is **offline** — the new branch is cut from your local `origin/<base>` with no network round-trip, so heavy repos aren't taxed on every session. If you want the worktree to start from the freshest remote tip, `git fetch origin <base>` in the source repo before running `jin session new --worktree`, or wire the fetch into the [post-create hook](#worktree-post-create-hook) below.

## TUI Keybindings

### Session list view

| Key | Action |
|-----|--------|
| `↑/k` | Move up |
| `↓/j` | Move down |
| `←/h` | Previous page |
| `→/l` | Next page |
| `/` | Search sessions (name, directory, branch) |
| `Enter` | Attach to session |
| `n` | Create new session |
| `x` | Kill session |
| `d` | Delete session |
| `r` | Refresh list |
| `v` | Open in VS Code |
| `!` | Notification history |
| `?` | Show help |
| `q` | Quit |

### Session creation form

| Key | Action |
|-----|--------|
| `Tab` | Move to next field |
| `Shift+Tab` | Move to previous field |
| `Enter` | Create session |
| `Esc` | Cancel |

While attached, press `Ctrl+]` (default) to detach and return to the TUI.
You can change this in `config.yaml` under `keybindings.detach`.

Supported detach keys:

| Key | Description |
|-----|-------------|
| `ctrl+]` | Default |
| `ctrl+^` | Ctrl+Shift+6 |
| `ctrl+\` | Ctrl+Backslash |
| `ctrl+g` | Ctrl+G |

## Claude Code Hooks

honjin uses Claude Code hooks to detect session state changes. **Hooks are configured automatically** — no manual setup required.

When a session starts, honjin generates `$XDG_STATE_HOME/honjin/hooks-settings.json` (default `~/.local/state/honjin/hooks-settings.json`) and passes it to Claude Code via `claude --settings`. This file wires up the following hooks:

| Hook Event | Role |
|-----------|------|
| `UserPromptSubmit` | User submits a prompt → set session to `thinking` |
| `PostToolUse` | Tool execution ends → set session to `thinking` (recovers from `permission` state) |
| `Stop` | Claude's turn ends → set session to `idle` (send task completion notification) |
| `Notification` | Permission request, etc. → set session to `permission` (send permission request notification) |

## Worktree Post-Create Hook

When you create a session with `jin session new --worktree`, honjin can run a setup script right after the worktree is created — installing dependencies, copying `.env`, initializing submodules — so every new worktree lands ready to use without any manual steps.

### Script location

Place a shell script at `.jin/worktree-post-create.sh` in the **original repository** (not the worktree). It always runs via `bash`, so `chmod +x` is not required. If the file doesn't exist, the hook is silently skipped.

```bash
#!/usr/bin/env bash
set -euo pipefail

# Copy .env from parent repository (git-ignored)
cp "$JIN_REPO_ROOT/.env" "$JIN_WORKTREE_PATH/.env" 2>/dev/null || true

# Install dependencies
pnpm install
```

### Environment variables

| Variable | Description |
|----------|--------------|
| `JIN_WORKTREE_PATH` | Absolute path of the newly created worktree |
| `JIN_WORKTREE_BRANCH` | Branch checked out in the worktree |
| `JIN_WORKTREE_BASE` | Base branch the worktree was created from |
| `JIN_SESSION_ID` | UUID of the session being created |
| `JIN_SESSION_NAME` | Session name, if one was given via `--name` (empty otherwise — the auto-derived name isn't assigned until after the hook runs) |
| `JIN_REPO_ROOT` | Absolute path of the original repository |

### Security: allowlist

Since the script is checked into a repository, honjin never runs it unless the repository has been explicitly trusted (a direnv-style allow model). Trust is tracked by the script's SHA256 — editing the script requires trusting it again.

```bash
jin worktree allow    # Trust the current repository (shows the script, asks for confirmation)
jin worktree revoke   # Revoke trust
jin worktree status   # Show the allow status of the current repository
jin worktree list     # List all trusted repositories
```

If the script exists but isn't trusted (or changed since it was trusted), the hook is skipped with a warning — the worktree is still created and Claude still starts normally. When creating from the TUI, the popup surfaces a three-way prompt (`a`: Allow, `s`: Skip and create anyway, `c`: Cancel) so you can decide without dropping to a shell.

### Skipping the hook

- `jin session new --worktree --no-hook` — skip the hook for this session only
- `worktree.hook_enabled: false` in `~/.config/honjin/config.yaml` — disable the hook for all repositories
- `worktree.hook_timeout: <seconds>` — change the timeout (default: `300`). On expiry the hook's process group is sent `SIGTERM`, given a 5-second grace period, then `SIGKILL` if still alive.

### On failure

If the hook exits non-zero or times out, the worktree and its branch are rolled back and `jin session new` fails with a non-zero exit code. The hook's stdout/stderr are kept at `~/.local/state/honjin/hook-logs/<session-id>.log` for troubleshooting, even after a rollback.

## Debugging

```bash
# Enable debug logging
export JIN_DEBUG=1

# Start daemon
jin daemon start

# View logs
tail -f ~/.local/state/honjin/daemon-debug.log
```

## Requirements

- Go 1.24.5+
- tmux 3.3+
- Claude Code CLI installed

## License

MIT
