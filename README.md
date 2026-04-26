**English** | [日本語](README.ja.md)

# ccvalet (claude-code-valet)

A CLI tool for running and managing multiple Claude Code sessions simultaneously.

https://github.com/user-attachments/assets/62e9d64a-aa7d-42f8-8edf-03f724fe0ee4

## Features

- **Multi-session management**: Run multiple Claude Code sessions in the background simultaneously
- **TUI**: Interactive terminal UI for listing, monitoring, and operating sessions
- **Attach/Detach**: Quickly switch between sessions (`Ctrl+]` to detach)
- **Real-time status tracking**: Live display of working directory, branch, and latest message
- **Search & Paging**: Incremental search by session name, directory, or branch
- **Desktop notifications**: OS notifications for permission requests and task completion (macOS / Linux)
- **Remote host support**: Manage remote sessions via SSH / Docker

## Installation

### Download from GitHub Releases

Download the binary for your OS/architecture from the [Releases page](https://github.com/takaaki-s/claude-code-valet/releases).

```bash
# Example: Linux amd64
curl -Lo ccvalet.tar.gz https://github.com/takaaki-s/claude-code-valet/releases/latest/download/ccvalet_0.1.0_linux_amd64.tar.gz
tar xzf ccvalet.tar.gz
sudo mv ccvalet /usr/local/bin/
```

### Go install

```bash
go install github.com/takaaki-s/claude-code-valet/cmd/ccvalet@latest
```

### Build from source

```bash
git clone https://github.com/takaaki-s/claude-code-valet.git
cd claude-code-valet
make build    # Build to bin/ccvalet
make install  # Install to $GOPATH/bin
```

## Quick Start

### 1. Start the daemon

```bash
ccvalet daemon start
```

### 2. Launch the TUI

```bash
ccvalet ui
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
ccvalet daemon start   # Start daemon
ccvalet daemon stop    # Stop daemon
ccvalet daemon status  # Check status
```

### Session management

```bash
# Create session (interactive via TUI - recommended)
ccvalet session new

# Create session (specify working directory)
ccvalet session new --workdir ~/repos/myrepo

# List sessions
ccvalet session list

# List sessions in JSON format (for scripting / LLM integration)
ccvalet session list --json

# Attach to a session
ccvalet session attach <session-name>

# Get session details
ccvalet session info <session-name>

# Send a prompt to a session
ccvalet session send <session-name> "your prompt here"

# Wait for a session to become idle (default timeout: 300s)
ccvalet session wait <session-name>
ccvalet session wait <session-name> --timeout 60

# Get the last assistant message
ccvalet session output <session-name>

# Get the last N conversation pairs
ccvalet session output <session-name> --last 3

# Kill a session
ccvalet session kill <session-name>

# Delete a session
ccvalet session delete <session-name>

# Bulk delete stopped sessions
ccvalet cleanup stopped
ccvalet cleanup stopped --dry-run   # Preview what will be deleted
```

> **Aliases**: `session` can be shortened to `sess` (e.g., `ccvalet sess list`). `list` to `ls`, `delete` to `rm`.

### LLM API (scripting / automation)

The following commands support `--json` for structured output, enabling integration with scripts and other LLM agents.

```bash
# All session commands support --json
ccvalet session list --json
ccvalet session new --workdir ~/repos/myrepo --json
ccvalet session info <session-name> --json
ccvalet session kill <session-name> --json

# Send a prompt and wait for completion
ccvalet session send <session-name> "fix the failing test" --json
ccvalet session wait <session-name> --timeout 120 --json
ccvalet session output <session-name> --json

# Pipeline example: send a prompt, wait, get output
ccvalet session send my-session "refactor main.go"
ccvalet session wait my-session --timeout 300
ccvalet session output my-session --last 1
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
ccvalet session prompt my-session "run go test ./... and report failures"
ccvalet session wait my-session --until idle,permission --timeout 600
ccvalet session result my-session --json | jq '.entries[].blocks[] | select(.kind=="tool_result")'

# Incremental fetch: only entries after a checkpoint.
# --since is strictly exclusive: pass the last entry's timestamp to receive
# only entries that came after it (no duplicates). Claude Code emits timestamps
# with millisecond precision (e.g. "2026-04-09T13:23:10.456Z").
T1=$(ccvalet session result my-session --json | jq -r '.entries[-1].timestamp')
ccvalet session prompt my-session "now also run go vet"
ccvalet session wait my-session --until idle,permission
ccvalet session result my-session --since "$T1" --json

# Filter to a specific tool, or to errors only
ccvalet session result my-session --tool Bash --json
ccvalet session result my-session --errors-only --json
```

`prompt` is an alias for `send`. All flags work with `--host` for remote/peer sessions.

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
ccvalet session workdir <session-name>    # Print session's working directory path
ccvalet session edit <session-name>       # Open session's working directory in EDITOR
```

> **Note**: `workdir` / `edit` only work correctly for local sessions (host type `local`).

The following shell functions are useful:

```bash
# cd to a session's working directory
cc-cd() { cd "$(ccvalet session workdir "$1")"; }

# Select a session with fzf and cd to its working directory
cc-cdf() {
  local session
  session=$(ccvalet session list | tail -n +2 | fzf --height 40% --reverse | awk '{print $1}')
  [[ -n "$session" ]] && cd "$(ccvalet session workdir "$session")"
}

# Select a session with fzf and attach
cc-attach() {
  local session
  session=$(ccvalet session list | tail -n +2 | fzf --height 40% --reverse | awk '{print $1}')
  [[ -n "$session" ]] && ccvalet session attach "$session"
}
```

### Shell Completion

```bash
# bash
source <(ccvalet completion bash)

# zsh
source <(ccvalet completion zsh)

# fish
ccvalet completion fish | source
```

## Configuration

Configuration files and data are stored in `~/.ccvalet/`.

```
~/.ccvalet/
├── config.yaml      # Configuration file
├── state.yaml       # State file (last used repository, etc.)
├── sessions/        # Session data
└── run/
    └── daemon.sock  # Daemon socket
```

### Example configuration (`~/.ccvalet/config.yaml`)

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

ccvalet uses Claude Code hooks to detect session state changes. **Hooks are configured automatically** — no manual setup required.

When a session starts, ccvalet generates `~/.ccvalet/hooks-settings.json` and passes it to Claude Code via `claude --settings`. This file wires up the following hooks:

| Hook Event | Role |
|-----------|------|
| `UserPromptSubmit` | User submits a prompt → set session to `thinking` |
| `PostToolUse` | Tool execution ends → set session to `thinking` (recovers from `permission` state) |
| `Stop` | Claude's turn ends → set session to `idle` (send task completion notification) |
| `Notification` | Permission request, etc. → set session to `permission` (send permission request notification) |

## Remote Hosts (SSH / Docker)

In addition to local sessions, you can manage Claude Code sessions running on remote SSH hosts or Docker containers.

### Architecture

The Master daemon on your local machine communicates with Slave daemons on remote hosts via SSH tunnels (or Docker volume mounts). The Slave runs the same `ccvalet daemon` binary.

Communication is **bidirectional**: the Master establishes a forward tunnel (`-L`) to reach the Slave, and a reverse tunnel (`-R`) so the Slave can reach the Master back. This allows either side to act as an orchestrator. A `visited` array in each request prevents routing loops.

Each daemon has a **host ID** (default: `"local"`). You can set it in `config.yaml` or via the `--host-id` flag:

```yaml
# ~/.ccvalet/config.yaml
host_id: mac   # Identifies this daemon in bidirectional routing
hosts:
  - id: my-server
    type: ssh
    host: my-remote-host
```

### SSH Remote Prerequisites

The SSH tunnel uses Unix socket forwarding (`-L`). Ensure the remote sshd allows it:

```bash
# Check on the remote host
sudo sshd -T | grep allowtcpforwarding
```

If the output is `allowtcpforwarding no`, add to `/etc/ssh/sshd_config`:

```
AllowTcpForwarding local
```

Then restart sshd:

```bash
sudo systemctl restart sshd
```

> **Note:** `AllowStreamLocalForwarding yes` alone is not sufficient — `AllowTcpForwarding` controls Unix socket `-L` forwarding as well, regardless of the OpenSSH version.

### SSH Remote Setup

**1. Install ccvalet and tmux on the remote host (first time only)**

```bash
# Log in to remote host
ssh my-remote-host

# Install ccvalet
go install github.com/takaaki-s/claude-code-valet/cmd/ccvalet@latest

# Install tmux (if not already installed)
sudo apt install -y tmux  # Ubuntu/Debian
```

**2. Add host configuration to config.yaml on your local machine**

```yaml
hosts:
  - id: my-server
    type: ssh
    host: my-remote-host
    ssh_opts:          # SSH connection optimization (recommended)
      - "-o"
      - "ControlMaster=auto"
      - "-o"
      - "ControlPath=~/.ssh/sockets/%r@%h-%p"
      - "-o"
      - "ControlPersist=600"
```

**3. Start Master to auto-connect**

```bash
ccvalet daemon start  # Auto-start Slave + establish tunnel
ccvalet ui            # Manage local + remote sessions in one TUI
```

The Master automatically starts the Slave daemon on the remote host via SSH. An error message is displayed if ccvalet is not installed on the remote host.

#### Specifying the remote ccvalet binary path

By default, `ccvalet` is resolved from the remote shell's `PATH`. If the binary is installed in a non-standard location (e.g., `~/.local/bin`) and is not available in non-interactive SSH sessions, specify the full path explicitly:

```yaml
hosts:
  - id: my-server
    type: ssh
    host: my-remote-host
    ccvalet_path: /home/user/.local/bin/ccvalet  # full path to ccvalet on remote
```

> **Note**: SSH sessions are non-interactive, so `.bashrc` / `.zshrc` are not sourced. If ccvalet is installed via `go install` or to `~/.local/bin` and PATH is only configured in those files, use `ccvalet_path` or add the path to `~/.bash_profile` / `~/.profile` instead.

### Docker Setup

**1. Include ccvalet and tmux in the container**

```dockerfile
# Add to Dockerfile
RUN apt-get update && apt-get install -y tmux
RUN go install github.com/takaaki-s/claude-code-valet/cmd/ccvalet@latest
```

**2. Start the container with a volume mount for socket sharing**

The local socket path is automatically computed as `/tmp/ccvalet-tunnels/{hostID}/daemon.sock` (same convention as SSH). The volume mount maps this directory to the socket directory inside the container.

```bash
# Root user
docker run -v /tmp/ccvalet-tunnels/docker-dev:/root/.ccvalet/run my-image

# Non-root user (app)
docker run -v /tmp/ccvalet-tunnels/docker-dev:/home/app/.ccvalet/run my-image

# Override socket_path
docker run -v /tmp/ccvalet-tunnels/docker-dev:/var/run/ccvalet my-image
```

**3. Add host configuration to config.yaml on your local machine**

```yaml
hosts:
  # Basic setup (default socket path: ~/.ccvalet/run/daemon.sock)
  - id: docker-dev
    type: docker
    container: my-container

  # socket_path override (specify path inside container)
  - id: docker-ci
    type: docker
    container: ci-runner
    socket_path: /var/run/ccvalet/daemon.sock

  # ccvalet_path override (if binary is not in default PATH)
  - id: docker-custom
    type: docker
    container: my-container
    ccvalet_path: /usr/local/bin/ccvalet
```

`socket_path` specifies the socket path inside the container (remote side). Defaults to `~/.ccvalet/run/daemon.sock` when omitted.

`ccvalet_path` specifies the full path to the ccvalet binary inside the container. Defaults to `ccvalet` (resolved from PATH) when omitted.

**4. Start Master**

```bash
ccvalet daemon start  # Auto-start Slave via docker exec
ccvalet ui
```

> **Note**: If you recreate the container (`docker rm`), ccvalet will be lost. Include it in the Dockerfile or persist the binary via volume mount.

## Debugging

```bash
# Enable debug logging
export CCVALET_DEBUG=1

# Start daemon
ccvalet daemon start

# View logs
tail -f ~/.ccvalet/debug.log
```

## Requirements

- Go 1.24.5+
- tmux 3.3+
- Claude Code CLI installed

## License

MIT
