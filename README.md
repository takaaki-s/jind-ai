**English** | [日本語](README.ja.md)

# jind-ai

A CLI tool for running and managing multiple agent sessions simultaneously
(Claude Code is the first-class citizen; other agents plug in via
`internal/agent/<kind>/`).

<img height="200" alt="Image" src="https://github.com/user-attachments/assets/9c32b796-991d-470b-8d23-58e10e99c1c4" />

https://github.com/user-attachments/assets/62e9d64a-aa7d-42f8-8edf-03f724fe0ee4

## Supported agents

| Kind | CLI | Notes |
|---|---|---|
| `claude` (default) | [Claude Code](https://claude.com/product/claude-code) 2.x | First-class support. Uses `--session-id` / `--resume` and Claude Code's native hook system for state tracking. |
| `codex` | [OpenAI Codex CLI](https://github.com/openai/codex) 0.144+ | Hooks are injected per-invocation via `-c hooks.X=[...]`; grant trust once through the `/hooks` dialog on first launch (see [docs/gotchas.md](docs/gotchas.md#codex-adapter)). Session name / resume UUID are learned from Codex's `SessionStart` hook payload (no `--session-id` equivalent upstream yet). |

Select a non-default adapter per session:

```bash
jin session new --agent codex --workdir ~/repos/myrepo
```

Or set a persistent default via `default_agent: codex` in `~/.config/jind-ai/config.yaml`.

The TUI create form includes an **agent picker step** whenever more than one adapter is registered — pick the kind per session with ↑↓/j/k + Enter. Initial selection is resolved as `--agent > default_agent > "claude"`. Use `jin ui --agent codex` to preselect Codex for this TUI run only (config is left untouched):

```bash
jin ui --agent codex   # transient default; ends when TUI exits
```

## Features

- **Multi-session management**: Run multiple Claude Code sessions in the background simultaneously
- **tmux-native**: Each session runs in its own tmux pane, so your existing `~/.tmux.conf`, custom keybindings, status bar, and copy-mode setup work as-is
- **Decoupled UI / logic architecture**: All session management, state transitions, and hook handling live in the daemon. The TUI is a thin client that talks to the daemon over a Unix socket and holds no session-management logic. In principle any alternate UI (web, editor extension, ...) can drive the same IPC (see [docs/architecture.md](docs/architecture.md) / [docs/ipc-protocol.md](docs/ipc-protocol.md))
- **TUI**: Interactive terminal UI for listing, monitoring, and operating sessions
- **Attach/Detach**: Quickly switch between sessions (`Ctrl+]` to detach)
- **Real-time status tracking**: Live display of working directory, branch, and latest message
- **Session filter & Paging**: `/` opens a fuzzy-search popup over session name, directory, branch, fleet, and agent kind
- **Desktop notifications**: OS notifications for permission requests and task completion (macOS / Linux)

## Installation

### Download from GitHub Releases

Download the binary for your OS/architecture from the [Releases page](https://github.com/takaaki-s/jind-ai/releases).

```bash
# Example: Linux amd64
curl -Lo jind-ai.tar.gz https://github.com/takaaki-s/jind-ai/releases/latest/download/jind-ai_0.1.0_linux_amd64.tar.gz
tar xzf jind-ai.tar.gz
sudo mv jin /usr/local/bin/
```

### Go install

```bash
go install github.com/takaaki-s/jind-ai/cmd/jin@latest
```

### Build from source

```bash
git clone https://github.com/takaaki-s/jind-ai.git
cd jind-ai
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

jind-ai follows the [XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/). Files are split across config / state / runtime directories:

```
$XDG_CONFIG_HOME/jind-ai/      (default: ~/.config/jind-ai)
└── config.yaml                # Configuration file

$XDG_STATE_HOME/jind-ai/       (default: ~/.local/state/jind-ai)
├── state.yaml                 # State file (last used repository, etc.)
├── sessions/                  # Session data
├── hooks-settings.json        # Generated hooks settings (auto-managed)
├── plugins.lock.yaml          # Installed-plugin ledger (see Plugins below)
├── plugin-logs/               # Per-plugin dispatch/run and build output
├── daemon-debug.log           # Daemon debug log (when JIN_DEBUG=1)
├── hook-debug.log             # Hook debug log (when JIN_DEBUG=1)
└── plugin-debug.log           # Plugin dispatcher debug log (when JIN_DEBUG=1)

$XDG_DATA_HOME/jind-ai/        (default: ~/.local/share/jind-ai)
└── plugins/                   # Installed plugins (see Plugins below)

$XDG_RUNTIME_DIR/jind-ai/      (fallback: $TMPDIR/jind-ai-<uid>)
└── daemon.sock                # Daemon socket
```

### Example configuration (`~/.config/jind-ai/config.yaml`)

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
  search: ["M-f"]         # opens the session filter popup (fuzzy search).
                          # Default M-f (Alt+f). Must be modifier-prefixed —
                          # a bare letter is consumed by the display pane and
                          # never reaches the outer tmux binding.
                          # Use ["/"] to restore the pre-M-f bare-slash key
                          # (breaks agent slash-commands in the display pane).
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
  # Outer tmux (jin-mgr) — action palette trigger, both panes
  action_panel: ["M-p"]  # Default: M-p
                          # action_panel: []           to disable
                          # action_panel: ["M-x"]      to rebind
```

### Worktree placement

By default, `jin session new --worktree` creates worktrees under `$XDG_STATE_HOME/jind-ai/worktrees/{name}` (typically `~/.local/state/jind-ai/worktrees/`). Override this with `worktree.base_dir` in `config.yaml`:

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
- **`default_branch`** — used **only** when jind-ai cannot auto-detect the repository's default branch. Detection reads `refs/remotes/origin/HEAD`; local clones that never had it set (some tarballs, `git clone --no-checkout`, older clones) will hit the fallback. If detection fails and `default_branch` is empty, session creation errors with `cannot detect default branch`.

Worktree creation itself is **offline** — the new branch is cut from your local `origin/<base>` with no network round-trip, so heavy repos aren't taxed on every session. If you want the worktree to start from the freshest remote tip, `git fetch origin <base>` in the source repo before running `jin session new --worktree`, or wire the fetch into the [post-create hook](#worktree-post-create-hook) below.

## TUI Keybindings

### Session list view

| Key | Action |
|-----|--------|
| `↑/k` | Move up |
| `↓/j` | Move down |
| `←/h` | Previous page |
| `→/l` | Next page |
| `M-f` | Open session filter (fuzzy popup) — see [Outer tmux — session filter](#outer-tmux--session-filter) |
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

### Outer tmux — action palette

`M-p` (Alt+p, default) opens the action palette, a searchable popup listing
every built-in TUI action plus installed plugin actions. It's bound at the
outer tmux (`jin-mgr`) root key table, so it fires the same way whether the
session list (left) or an attached agent (right) has focus.

Override or disable it in `config.yaml` (see `keybindings.action_panel`
above):

```yaml
keybindings:
  action_panel: ["M-x"]  # rebind to Alt+x
  # action_panel: []       # disable entirely (no bind-key issued)
```

Keys must include a modifier (`M-`/`C-`) — a bare letter would be consumed as
normal input by the agent in the right pane instead of reaching the outer
tmux binding.

### Outer tmux — session filter

`M-f` (Alt+f, default) opens the session filter, a fuzzy-search popup for
jumping straight to a session: type a few characters and press `Enter` to
attach immediately. It's bound the same way as the action palette above — at
the outer tmux (`jin-mgr`) root key table, so it fires from either pane.
Matched fields are session description, working directory, current working
directory, git branch, fleet, and agent kind (subsequence matching via
[sahilm/fuzzy](https://github.com/sahilm/fuzzy), smart-case, ranked by
score). `Esc` closes the popup without changing anything; `↑`/`↓` or
`Ctrl+P`/`Ctrl+N` move the cursor.

The default changed from `/` to `M-f` because a bare-letter binding at the
outer tmux root also swallows `/` typed in the display pane, breaking agent
slash-commands (Claude Code `/help`, less/vim `/search`, etc.). The action
palette entry ("session filter") also invokes this popup, so you can reach
it without a shortcut at all.

Override or disable the trigger the same way as `action_panel`:

```yaml
keybindings:
  search: ["ctrl+p"]      # rebind to Ctrl+p
  # search: ["/"]         # restore pre-M-f bare-slash (breaks display-pane `/`)
  # search: []            # disable entirely (no bind-key issued)
```

## Claude Code Hooks

jind-ai uses Claude Code hooks to detect session state changes. **Hooks are configured automatically** — no manual setup required.

When a session starts, jind-ai generates `$XDG_STATE_HOME/jind-ai/hooks-settings.json` (default `~/.local/state/jind-ai/hooks-settings.json`) and passes it to Claude Code via `claude --settings`. This file wires up the following hooks:

| Hook Event | Role |
|-----------|------|
| `UserPromptSubmit` | User submits a prompt → set session to `thinking` |
| `PostToolUse` | Tool execution ends → set session to `thinking` (recovers from `permission` state) |
| `Stop` | Claude's turn ends → set session to `idle` (send task completion notification) |
| `Notification` | Permission request, etc. → set session to `permission` (send permission request notification) |

## Worktree Post-Create Hook

When you create a session with `jin session new --worktree`, jind-ai can run a setup script right after the worktree is created — installing dependencies, copying `.env`, initializing submodules — so every new worktree lands ready to use without any manual steps.

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

Since the script is checked into a repository, jind-ai never runs it unless the repository has been explicitly trusted (a direnv-style allow model). Trust is tracked by the script's SHA256 — editing the script requires trusting it again.

```bash
jin worktree allow    # Trust the current repository (shows the script, asks for confirmation)
jin worktree revoke   # Revoke trust
jin worktree status   # Show the allow status of the current repository
jin worktree list     # List all trusted repositories
```

If the script exists but isn't trusted (or changed since it was trusted), the hook is skipped with a warning — the worktree is still created and Claude still starts normally. When creating from the TUI, the popup surfaces a three-way prompt (`a`: Allow, `s`: Skip and create anyway, `c`: Cancel) so you can decide without dropping to a shell.

### Skipping the hook

- `jin session new --worktree --no-hook` — skip the hook for this session only
- `worktree.hook_enabled: false` in `~/.config/jind-ai/config.yaml` — disable the hook for all repositories
- `worktree.hook_timeout: <seconds>` — change the timeout (default: `300`). On expiry the hook's process group is sent `SIGTERM`, given a 5-second grace period, then `SIGKILL` if still alive.

### On failure

If the hook exits non-zero or times out, the worktree and its branch are rolled back and `jin session new` fails with a non-zero exit code. The hook's stdout/stderr are kept at `~/.local/state/jind-ai/hook-logs/<session-id>.log` for troubleshooting, even after a rollback.

## Plugins

jind-ai can run your own shell-executable plugins in reaction to session
status changes, or on demand. A plugin is a directory with a manifest and an
entry-point script; jind-ai never inspects what the script does, only when
it runs and what environment it gets.

### Two ways a plugin runs

- **Event listener** — subscribes to `status_changed` via the manifest's
  `on:` matcher. Good for notifications, logging, CI triggers — anything
  non-interactive. Note: an event fires only when the status actually
  changes; a notification without a status transition (e.g. a repeated stop
  while already idle) does not dispatch.
- **Action** — launched explicitly with `jin plugin run <name> [--session
  <selector>]`. Good for interactive workflows (e.g. a popup-based diff review
  UI). Set `on: []` to make a plugin action-only. Without `--session` the run
  is a **global action**: all session-derived env vars are empty. On every
  action run — global or session-scoped — `JIN_CALLER_TMUX_SOCKET` /
  `JIN_CALLER_TMUX_PANE` identify where the invoking CLI was launched from,
  when it sat inside a tmux client.

Both entry points run the same `run:` command with the same environment;
only the trigger differs.

### Manifest (`jin-plugin.yaml`)

Place this file at the root of the plugin directory:

```yaml
name: notifier
api_version: 1
on: ["status_changed:idle", "status_changed:permission"]
run: ./notify.sh                 # relative to the plugin's own directory
build: go build -o bin/plugin .  # optional; runs once, at install/update only
timeout: 30s                     # optional; default 30s
```

| Field | Required | Description |
|-------|----------|--------------|
| `name` | Yes | `[a-z0-9][a-z0-9-]*`; must match the directory name jind-ai installs it under |
| `api_version` | Yes | A single integer, not a range — see [API compatibility](#api-compatibility) |
| `on` | No | List of `status_changed` or `status_changed:<status>` matchers. Empty or omitted = action-only |
| `run` | Yes | Shell command, run via `bash -c` with the plugin directory as cwd |
| `build` | No | Shell command run once at install/update time (never at dispatch) — see [Language-specific guidance](#language-specific-guidance) |
| `timeout` | No | Duration string (`"30s"`, `"5m"`); default `30s` |

`config.yaml` only enables/disables plugins and tunes dispatch timing (below) — it never duplicates manifest fields.

### What a plugin receives

Environment variables:

| Variable | Description |
|----------|--------------|
| `JIN_EVENT` | `status_changed` or `action` |
| `JIN_SESSION_ID` | Session ID |
| `JIN_STATUS` | Current status |
| `JIN_PREV_STATUS` | Previous status (empty for an `action` run) |
| `JIN_AGENT_KIND` | Adapter kind (`claude`, ...) |
| `JIN_WORKDIR` | Session's working directory |
| `JIN_TMUX_PANE_ID` | tmux pane ID, if known |
| `JIN_NOTIFY_KIND` | Notification kind for this transition: `task-complete`, `error`, `permission`, or empty when the transition triggers no notification |
| `JIN_PLUGIN_API_VERSION` | The `api_version` this plugin declared |
| `JIN_PLUGIN_DEPTH` | Chain depth — see [Constraints](#constraints) |
| `JIN_SOCKET` | Daemon socket path; the `jin` CLI a plugin invokes picks this up automatically |
| `JIN_BIN` | Absolute path of the daemon's own `jin` binary. Prefer `"${JIN_BIN:-jin}"` over a bare `jin` — a `jin` found on PATH may be an older install that lacks newer subcommands |
| `JIN_CALLER_TMUX_SOCKET` | Action runs only: socket path of the tmux server the invoking CLI ran inside (from its `$TMUX`). Unset — not empty — when the caller was outside tmux |
| `JIN_CALLER_TMUX_PANE` | Action runs only: the invoking CLI's pane ID (from its `$TMUX_PANE`). Unset when unknown |

The same data is also written to **stdin as JSON** (same fields, snake_case;
caller tmux context is env-only).

For anything beyond this thin payload, call back into jind-ai:

```bash
jin session info "$JIN_SESSION_ID" --json    # full session details
jin session send "$JIN_SESSION_ID" "..."     # send a prompt
jin session result "$JIN_SESSION_ID" --json  # structured transcript entries
jin session focus "$JIN_SESSION_ID"          # make the running TUI display this session
jin pane popup "$JIN_SESSION_ID" -- <cmd>    # tmux popup over the session's pane
jin pane popup --here -- <cmd>               # tmux popup over the caller's own pane (uses $TMUX, falling back to JIN_CALLER_TMUX_SOCKET)
jin pane split "$JIN_SESSION_ID" -- <cmd>
jin pane capture "$JIN_SESSION_ID"
jin pane send-keys "$JIN_SESSION_ID" <keys>
```

**Compatibility contract**: treat any environment variable, JSON field, or CLI
flag you don't recognize as something to ignore, not an error. jind-ai only
adds to this surface without a version bump; it never removes or renames
within an `api_version`.

### Install / update / remove / list

```bash
# From a git source (github.com/, gitlab.com/, self-hosted, ssh URLs, ...)
jin plugin install github.com/owner/repo          # default branch
jin plugin install github.com/owner/repo@v1.2.0   # pinned to a tag/branch/SHA

# From a local directory, symlinked in place (development)
jin plugin install --link ./my-plugin

jin plugin update <name>
jin plugin remove <name>
jin plugin list          # NAME / API / STATE / SOURCE; --json for scripting
```

A git install/update shows the manifest (`name`, `on`, `run`, `build`) and the
commit SHA it resolved to, and asks for confirmation (`--yes` to skip) before
touching anything; the approved commit SHA is recorded in
`plugins.lock.yaml`, so a later `install`/`update` never silently lands on a
different commit than the one you saw. A `--link`ed plugin skips this —
linking a local path is itself the trust decision, and jind-ai never runs
`build:` for a linked plugin.

### Language-specific guidance

- **Shell / single file** — clone-and-run, no `build:` needed.
- **Node.js / TypeScript** — bundle to `dist/` (esbuild etc.) and commit the
  bundle; resolving dependencies at runtime (bun/deno) works too, but that
  first-dispatch network fetch can fail silently since dispatch is fail-open
  — a pre-built bundle is more predictable.
- **Go / Rust / other compiled languages** — use `build:` to compile on
  install/update so the binary matches the user's platform/arch (and
  `go.sum` / `Cargo.lock` give reproducibility). `build:` runs exactly once
  per install/update as a single declared command; jind-ai does not resolve
  dependencies or detect a toolchain for you — document what's required in
  your plugin's own README. A non-zero exit fails the install/update
  atomically (nothing is left half-installed), with output kept at
  `~/.local/state/jind-ai/plugin-logs/<name>-build.log`. jind-ai injects
  `npm_config_ignore_scripts=true` into the build environment by default (a
  supply-chain guard you can override inside your own `build:` command); the
  build itself runs with your own user privileges — it is not sandboxed.

### Constraints

- **No persistent processes.** jind-ai runs a plugin per event/action and
  tears it down; don't build a long-running daemon into `run:`. If you need
  one, run it yourself (manually, or as a systemd user unit) and keep the
  plugin a thin per-event client to it (e.g. `curl`).
- **Popups don't inherit `JIN_*` env vars.** `jin pane popup` / `jin pane
  split` run their command in a process tmux spawns fresh — pass any data
  the popup needs as arguments on its command line (or as env-assignment
  prefixes in the command string, e.g.
  `jin pane popup "$JIN_SESSION_ID" -- "JIN_BIN=$JIN_BIN inner.sh --id $JIN_SESSION_ID"`),
  not as inherited env vars.
- **Fail-open.** A plugin that errors, times out, or hangs never blocks a
  session's status pipeline — it's logged and the pipeline moves on. Timeout
  defaults to 30s (`timeout:` in the manifest).
- **Loop residual risk.** jind-ai debounces repeated dispatch of the same
  (plugin, session, event) within a short window (default 3s,
  `plugins.debounce` below) and rejects a plugin chaining another plugin run
  beyond one hop (`JIN_PLUGIN_DEPTH`). Neither catches a *slow* ping-pong
  (e.g. a plugin that sends a prompt whose eventual response re-triggers the
  same plugin a few seconds later) — avoiding that is on the plugin author.

### Config (`~/.config/jind-ai/config.yaml`)

```yaml
plugins:
  enabled: true          # default true; false disables all plugin dispatch
  disabled: ["notifier"] # disable individual plugins by name
  build_timeout: 300  # seconds, install/update build step (default 300)
  debounce: 3          # seconds, dispatch debounce window (default 3)
```

### API compatibility

Plugins declare a single `api_version` integer; jind-ai supports a window
`[min, current]` (v1 today: both are `1`). Checked at install/update
(fail-closed — a plugin outside the window is rejected before anything is
written) and again at every dispatch (fail-open — an incompatible installed
plugin is skipped, logged once, and shown as `incompatible` in `jin plugin
list`, with `jin plugin run` pointing you at `jin plugin update <name>`).

### Debugging a plugin

```bash
export JIN_DEBUG=1
tail -f ~/.local/state/jind-ai/plugin-debug.log        # dispatcher decisions
tail -f ~/.local/state/jind-ai/plugin-logs/<name>.log  # a plugin's own stdout/stderr
```

## Debugging

```bash
# Enable debug logging
export JIN_DEBUG=1

# Start daemon
jin daemon start

# View logs
tail -f ~/.local/state/jind-ai/daemon-debug.log
```

## Requirements

- Go 1.24.5+
- tmux 3.3+
- Claude Code CLI installed

## License

MIT
