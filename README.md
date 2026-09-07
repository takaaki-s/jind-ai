**English** | [日本語](README.ja.md)

# jind-ai

**Ten agent sessions. One screen.**

- Never miss the agent that's waiting on you
- A git worktree of its own for every session
- Reboot the machine, pick up the conversation
- Agents can drive agents

Runs on tmux, so your config carries over — and over SSH you come back to the
same screen.

![Eight agent sessions in one list. The cursor moves down it and the pane below follows, describing each session without switching to it; the one waiting on a permission prompt is marked, and its prompt is open on the right.](https://github.com/takaaki-s/jind-ai/releases/download/v0.10.0/demo.gif)

> **For AI agents.** jin's reference material for agents is not in this file — it
> is compiled into the binary, so it always matches the version installed.
> `jin docs list` names the topics, `jin docs show <name>` reads one, and
> neither needs the daemon. With only the repository in front of you, the same
> documents are [`internal/agentdocs/docs/`](internal/agentdocs/docs/); start
> with [orchestration](internal/agentdocs/docs/orchestration.md).

## Installation

### Download from GitHub Releases

Download the binary for your OS/architecture from the [Releases page](https://github.com/takaaki-s/jind-ai/releases).

```bash
# Example: Linux amd64
curl -Lo jind-ai.tar.gz https://github.com/takaaki-s/jind-ai/releases/latest/download/jind-ai_0.7.0_linux_amd64.tar.gz
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

## What you can do

**Know which one needs you.** Status is reported by the agent itself — thinking,
idle, or stopped waiting for your permission — rather than guessed by reading
the screen.

**Keep agents out of each other's way.** `--worktree` cuts a git worktree and a
branch as the session starts, so agents running in parallel never share a
working tree.

**Come back to it.** A session outlives the daemon, the agent, and a reboot.
Reopening one resumes the conversation where it stopped.

**Get around quickly.** `Ctrl+]` detaches; `M-f` opens a fuzzy search over every
session's name, directory, branch, fleet and agent kind.

**Mix agents.** Claude Code, Codex and opencode run side by side, chosen per
session.

**Drive it from a script — or from another agent.** Create, send, wait and read
the result back, all with `--json`.

**Extend it.** Plugins run on a status change or on demand; desktop
notifications are one you can install today.

All the logic lives in the daemon and the TUI is a thin client over a Unix
socket, so another front end can drive the same IPC — see
[architecture](docs/architecture.md) and [IPC protocol](docs/ipc-protocol.md).

How to do each of these is further down: [CLI commands](#cli-commands),
[configuration](#configuration), [TUI keybindings](#tui-keybindings),
[plugins](#plugins) — and, for writing one, [docs/plugins.md](docs/plugins.md).

## Project status

jind-ai is a personal project, built and maintained by one developer for their
own daily use and released in the hope that it is useful to others. Please use
it with these expectations:

- **No support guarantee.** Issues and pull requests may go unanswered.
- **Pre-1.0.** Breaking changes to configuration, IPC, and plugin manifests can
  land in any release.
- **Deliberately narrow scope.** See [CONTRIBUTING.md](CONTRIBUTING.md) before
  opening an issue or pull request.

Bug reports with reproduction steps are the most welcome kind of contribution.

## Supported agents

| Kind | CLI | Notes |
|---|---|---|
| `claude` (default) | [Claude Code](https://claude.com/product/claude-code) 2.x | First-class support. Uses `--session-id` / `--resume` and Claude Code's native hook system for state tracking. |
| `codex` | [OpenAI Codex CLI](https://github.com/openai/codex) 0.144+ | Hooks are injected per-invocation via `-c hooks.X=[...]`; grant trust once through the `/hooks` dialog on first launch (see [docs/gotchas.md](docs/gotchas.md#codex-adapter)). Session name / resume UUID are learned from Codex's `SessionStart` hook payload (no `--session-id` equivalent upstream yet). |
| `opencode` | [opencode](https://github.com/sst/opencode) 1.17+ | **Experimental.** Status is reported by a TypeScript plugin embedded in the `jin` binary and materialised under jind-ai state; opencode is pointed at it via `OPENCODE_CONFIG_DIR`, which is additive and leaves `~/.config/opencode` untouched (see [docs/gotchas.md](docs/gotchas.md#opencode-adapter)). No external bun install is required. The resume id is learned from the plugin's `SessionStart` (no `--session-id` equivalent upstream). `jin session result` runs `opencode export --pure <session id>` and parses that — opencode keeps its conversation in SQLite, jind-ai records nothing of its own, and `opencode` therefore has to be on the daemon's PATH. |

Claude Code is the first-class citizen; other agents plug in as adapters under
`internal/agent/<kind>/`.

Select a non-default adapter per session:

```bash
jin session new --agent codex --workdir ~/repos/myrepo
```

Or set a persistent default via `default_agent: codex` in `~/.config/jind-ai/config.yaml`.

The TUI create form includes an **agent picker step** whenever more than one adapter is registered — pick the kind per session with ↑↓/j/k + Enter. Initial selection is resolved as `--agent > default_agent > "claude"`. Use `jin ui --agent codex` to preselect Codex for this TUI run only (config is left untouched):

```bash
jin ui --agent codex   # transient default; ends when TUI exits
```

### Picking a model

`--model` selects the model for one session:

```bash
jin session new --model opus                    # Claude Code
jin session new --agent opencode --model anthropic/claude-opus-4-5
```

The value is handed to the agent's own CLI untouched, so it is spelled the way
that CLI spells it — an alias or full name for Claude Code, `provider/model`
for opencode. jind-ai does not check it against a model list, and the agents do
not agree on what to do with a name they do not know: Claude Code starts and
warns inside the pane, opencode starts and says nothing at all. **Either way a
typo produces a session jind-ai reports as running** — and on opencode nothing
in the pane will tell you, so check the spelling rather than the session.

The choice sticks to the session and is replayed whenever it is resumed,
including after a daemon restart. It can only be set at creation time — there
is no config default and no TUI picker for it.

## Quick Start

### 1. Set up

```bash
jin init
```

Writes a default `config.yaml`, then offers to install the **jin skill** for
whichever agents it finds on your `PATH` (`claude`, `codex`, `opencode`). The
skill is a short document that teaches an agent to drive jin by reading
`jin docs`.

Installing it is opt-in: the full text and every destination are printed
before you are asked, the prompt defaults to no, existing files are never
replaced without `--force`, and nothing is written when stdin is not a
terminal. Use `--dry-run` to see what it would do, `--no-skill` to skip it, or
`--skill-dir` to choose the location yourself.

### 2. Start the daemon

```bash
jin daemon start
```

### 3. Launch the TUI

```bash
jin ui
```

### 4. Create and attach to a session

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
| `idle` | ○ | `Stop` hook, or a 30s no-hook fallback | Waiting for input |
| `stopped` | ■ | Process death detection | Stopped |

**A `codex` session never reaches `permission`.** Codex raises its approval
hook when an approval path opens, which is not the same as a human being asked,
and nothing in the payload jin receives says which it was — so that adapter
maps it to `thinking`. A codex session genuinely waiting on an approval reads
as `thinking`, `jin session respond` cannot answer it, and no timer moves it:
attach to the pane. Claude Code and opencode are unaffected.

## CLI Commands

### Daemon management

```bash
jin daemon start   # Start daemon
jin daemon stop    # Stop daemon
jin daemon status  # Check status
```

**Which daemon a command reaches.** `JIN_SOCKET` when set, otherwise the default
socket path — `$XDG_RUNTIME_DIR/jind-ai/daemon.sock`, or
`$TMPDIR/jind-ai-<uid>/daemon.sock` where `XDG_RUNTIME_DIR` is unset, which is
the usual case on macOS. A tmux server started outside jind-ai keeps the
environment it was forked with, so a `jin` run from one of its panes can reach
an older daemon without saying so — set `JIN_SOCKET` or restart that server.
Panes jind-ai opens are unaffected. Details in
[docs/ipc-protocol.md](docs/ipc-protocol.md#which-daemon-a-command-reaches).

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

# Send a prompt to a session (idle sessions only)
jin session send <session-name> "your prompt here"

# Answer a session blocked on a prompt (Claude Code sessions)
jin session respond <session-name> --option 1
jin session respond <session-name> --text "use bun"

# Wait for a session to become idle (default timeout: 300s)
jin session wait <session-name>
jin session wait <session-name> --timeout 600

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

### Agent-facing documentation

jin ships the reference material its own agents need, compiled into the binary
so it always matches the version you are running.

```bash
jin docs list             # available topics, with a summary of each
jin docs show <name>      # read one
```

Both work without the daemon. Topics cover the delegation loop, selector
resolution, exit codes, and the traps an orchestrating agent hits. Agents that
installed the skill via `jin init` are pointed here automatically, and every
session jin starts is told these commands exist.

### Driving jin from a script or another agent

Every session command takes `--json`, so a script — or another agent — can
create sessions, send prompts, wait for them to settle, and read back what the
child actually did.

```bash
jin session new --workdir ~/repos/myrepo --json
jin session send my-session "run go test ./... and report failures" --wait-running
jin session wait my-session --until idle,permission --timeout 600
jin session result my-session --json      # tool calls and results, from the agent's own log
```

`--wait-running` is the part that is easy to omit and expensive to get wrong:
without it `send` reports delivery only, so a prompt that reached the input box
but was never submitted still exits 0 and the following `wait` returns at once.

The full loop — including what to do when a child blocks on a permission
prompt, how to fetch results incrementally, which agent kinds support what, and
every exit code with its remedy — ships with the binary and is browsable here:

| Topic | Read it |
|---|---|
| The delegation loop, end to end | [`jin docs show orchestration`](internal/agentdocs/docs/orchestration.md) |
| Exit codes and what to do about each | [`jin docs show exit-codes`](internal/agentdocs/docs/exit-codes.md) |
| Selecting a session | [`jin docs show selectors`](internal/agentdocs/docs/selectors.md) |
| Behaviour that will surprise you | [`jin docs show gotchas`](internal/agentdocs/docs/gotchas.md) |

`jin init` can install a skill that teaches an agent to read these itself.

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
  search: ["M-f"]         # opens the switch-session picker (fuzzy search).
                          # Default M-f (Alt+f). Must be modifier-prefixed —
                          # a bare letter is consumed by the display pane and
                          # never reaches the outer tmux binding.
                          # Use ["/"] to restore the pre-M-f bare-slash key
                          # (breaks agent slash-commands in the display pane).
  vscode: ["v"]
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
  # Outer tmux (jin-mgr) — per-plugin, per-action triggers (both panes)
  # No default — user opts in per (plugin, action). Fires
  # `jin plugin run <name> <action>` via tmux `run-shell -b` (background,
  # no output to the active pane), so opening a popup is the action's own
  # responsibility (matches the `jin pane popup --here` model). Uninstalled
  # plugins are silently skipped with one log line. Key collisions with
  # core outer-tmux bindings are warned only; tmux last-write-wins.
  # Outer-tmux keys accept both tmux notation (`M-n`, `C-f`) and the "+"
  # style (`alt+n`, `ctrl+f`); they are normalized to tmux form at load.
  # Note: the 0.7.x `plugins.<name>.keys` shape is rejected from 0.8.0
  # onward — one WARN is logged at startup and the binding is dropped.
  # Rewrite it as `plugins.<name>.actions.<id>.keys` (shown below).
  plugins:
    # notifier:
    #   actions:
    #     default: { keys: ["M-n"] }              # one keystroke → default action
    #     send-dm: { keys: ["M-d", "C-M-d"] }     # multiple keys / a different action
    # worktree-cleanup:
    #   actions:
    #     default: { keys: ["M-w"] }
    # journal:
    #   actions:
    #     default: { keys: ["ctrl+f"] }           # "+" style also accepted

# Optional: popup sizes (percent, int 1-100). Every entry is optional;
# omitted popups keep their default (create/session_filter/action = 70-80).
# See docs/tui-guide.md#popup-sizes for the full table and delivery paths.
# Note: the `session_filter` key sizes the switch-session picker (see the
# "Outer tmux — switch session" section above) — kept for backward compat.
popups:
  create:         { width: 80, height: 80 }
  session_filter: { width: 70, height: 70 }  # switch-session picker
  help:           { width: 60, height: 60 }
  action:         { width: 70, height: 70 }
  confirm:        { width: 50, height: 50 }  # Kill/Delete confirmation (default: 48x10 cells)
  plugin_default: { width: 70, height: 70 }
  plugins:                                # per-plugin overrides
    # my-notifier:  { width: 40, height: 20 }
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
| `{name}` | Worktree name (e.g. `jin-abcd1234`, or the `--worktree-name` you passed) |
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
| `M-f` | Open switch-session picker (fuzzy popup) — see [Outer tmux — switch session](#outer-tmux--switch-session) |
| `Enter` | Attach to session |
| `n` | Create new session |
| `x` | Kill session |
| `d` | Delete session |
| `r` | Refresh list |
| `v` | Open in VS Code |
| `?` | Show help |
| `q` | Quit |

The session list also takes mouse input. Switching sessions takes two clicks: the first moves the cursor onto that session, so the detail pane below previews it without switching, and a second click switches (same as `Enter`) — a mis-aimed tap costs you a look at the wrong session rather than a switch to it. Use the wheel to scroll the list without moving the cursor, and hold `Shift` while the list has the mouse to drag-select text in that pane with your terminal.

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

### Outer tmux — switch session

`M-f` (Alt+f, default) opens the switch-session picker, a fuzzy-search popup
for jumping straight to a session: type a few characters and press `Enter` to
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
palette entry ("switch session") also invokes this popup, so you can reach
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
| `Stop` | Claude's turn ends → set session to `idle` (dispatches a task-complete `JIN_NOTIFY_KIND` to plugins) |
| `Notification` | Permission request, etc. → set session to `permission` (dispatches a permission `JIN_NOTIFY_KIND` to plugins) |

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
| `JIN_WORKTREE_BASE` | Base branch name — the worktree is cut from `origin/<this>` |
| `JIN_SESSION_ID` | UUID of the session being created |
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

Plugins run on a status change or on demand — a desktop notification when a
session needs you, a message posted somewhere, anything you can execute. They
are ordinary programs jind-ai spawns with the session's context in the
environment; nothing is compiled in, and the built-in notifier was removed in
favour of one.

```bash
jin plugin ls-remote                       # what the registry offers
jin plugin install jind-ai-notifier        # install one
jin plugin list                            # what is installed
jin plugin run <name>                      # run one on demand
```

An install shows you the manifest and the resolved commit and asks before
touching anything.

- **[Writing and installing plugins](docs/plugins.md)** — manifest format, what
  a plugin receives, language-specific guidance, constraints, compatibility
- **[Publishing to the registry](docs/plugin-registry.md)** — how a plugin gets
  listed

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

- Go 1.26+
- tmux 3.5+ (3.3a cannot re-attach to a session; 3.6a and 3.7a each have a display bug — see docs/gotchas.md)
- Claude Code CLI installed

## License

MIT