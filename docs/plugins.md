**English** | [日本語](plugins.ja.md)

# Plugins

jind-ai can run your own shell-executable plugins in reaction to session
status changes, or on demand. A plugin is a directory with a manifest and an
entry-point script; jind-ai never inspects what the script does, only when
it runs and what environment it gets.

Community plugins are discoverable through the [plugin registry](plugin-registry.md):
`jin plugin ls-remote` lists them, `jin plugin install <name>` installs one
by registry name with a commit-pinned consent screen.

## Three ways a plugin runs

- **Event listener** — a manifest action subscribes to `status_changed`
  via its `on:` matcher. Good for notifications, logging, CI triggers —
  anything non-interactive. Note: an event fires only when the status
  actually changes; a notification without a status transition (e.g. a
  repeated stop while already idle) does not dispatch. If a plugin
  declares multiple actions, each is matched and debounced independently,
  so the same event can fan out to several actions on the same plugin.
- **Action** — launched explicitly with `jin plugin run <name> [action]
  [--session <selector>]`. Good for interactive workflows (e.g. a
  popup-based diff review UI). Omit `[action]` to run the plugin's default
  action (`actions[0]`); pass an action ID to select a specific one. Set
  an action's `on: []` to make it action-only. Without `--session` the run
  is a **global action**: all session-derived env vars are empty. On every
  action run — global or session-scoped — `JIN_CALLER_TMUX_SOCKET` /
  `JIN_CALLER_TMUX_PANE` identify where the invoking CLI was launched
  from, when it sat inside a tmux client.
- **PR handoff provider** — an action with `handoff: true`, invoked only by
  `jin session pr-handoff <session> <plugin> [action] --confirm`. It receives a
  bounded evidence document and idempotency key, runs synchronously, and
  returns one bounded JSON result. It cannot declare `on`, `listener`, or
  `popup`, and is hidden from generic action surfaces. Use `--dry-run` to run
  every core preflight without invoking it.

All three execute the declared action `entrypoint`; handoff providers use the
stricter stdin/stdout contract below.

## Manifest (`jind-ai-plugin.yaml`)

Place this file at the root of the plugin directory. The same manifest is
read at runtime (dispatcher) and at publish time (registry crawler); one
file, one source of truth.

```yaml
schema_version: 2
name: notifier
version: 0.2.0
description: Desktop notifications for jin sessions
license: MIT
homepage: https://github.com/foo/notifier
jin: ">=0.8.0"
timeout: 30s                                         # per plugin, not per action
install:
  source:
    build:
      - go build -o bin/notifier ./cmd/notifier
actions:
  - id: default                                      # actions[0] is the implicit default
    entrypoint: ./bin/notifier notify
    on: ["status_changed:idle", "status_changed:permission"]
    label: "Desktop notification"
  - id: send-dm                                      # `jin plugin run notifier send-dm`
    entrypoint: ./bin/notifier send-dm
    on: []                                           # action-only (no event subscription)
    label: "Send DM to teammate"
    popup: { width: 60, height: 30 }
```

Existing v1 manifests (`schema_version: 1` with `install.source.entrypoint`
plus top-level `on` / `timeout` / `popup`) keep working: they are normalised
at parse time into a single-action shape, so no author-side migration is
required. Write new manifests as v2.

| Field | Required | Description |
|-------|----------|--------------|
| `schema_version` | Yes | Manifest generation. `1` or `2`. v1 is auto-normalised to a single-action v2 shape at parse time |
| `name` | Yes | `[a-z][a-z0-9-]{1,63}`; unique in the registry; must match the directory name jind-ai installs it under |
| `version` | Yes | Plugin's own semver (`X.Y.Z`); pre-release/build metadata allowed |
| `description` | Yes | One-liner shown in `jin plugin ls-remote` search results |
| `license` / `homepage` | No | Optional metadata carried into the registry entry |
| `jin` | Yes | Semver constraint on the jin binary (`">=0.8.0"`, `"^0.8"`, `">=0.8 <0.10"`). Checked at install and at every dispatch |
| `install.source.build` | No | Ordered list of build commands (each element runs in its own `bash -c`, no piping across elements) — see [Language-specific guidance](#language-specific-guidance). Omit for plugins that ship a directly-executable entrypoint (shell scripts, prebuilt binaries in the repo) |
| `install.source.entrypoint` | v1 only | Default entrypoint the dispatcher executes. Forbidden in v2 (validation error) — v2 declares entrypoints per action instead |
| `install.release_asset.pattern` | Conditional | Alternative to `install.source`. Downloads a prebuilt asset from the latest GitHub Release. Placeholders: `{os}`, `{arch}` |
| `actions[]` | v2 only | List of actions the plugin exposes. `actions[0]` is the implicit default. Each element carries `id` / `entrypoint` / `on` / `label` / `popup` (fields below) |
| `actions[].id` | Yes | `[a-z][a-z0-9-]{0,31}` (32 chars max — deliberately narrower than `name`, since ids are also CLI arg tokens); unique within the plugin. Explicit IDs are strongly recommended — palette, keybindings, and `jin plugin run` all reference actions by ID |
| `actions[].entrypoint` | Yes | Path (relative to the plugin dir) executed for this action. Every action needs its own — v2 has no manifest-level fallback |
| `actions[].on` | No | Per-action `status_changed` matchers, same syntax as v1's top-level `on`. Empty or omitted = action-only. Matching / debouncing is independent per action |
| `actions[].label` | No | Human-readable label shown in the palette and help popup. When empty, the palette displays `<plugin>:<action-id>` (or just `<plugin>` when the default action's ID is `default`) |
| `timeout` (top-level) | No | How long any of this plugin's actions may run; default `30s`. There is no per-action override — the dispatcher applies this one value to every action |
| `actions[].popup.width` / `.height` | No | Per-action manifest hint for `jin pane popup --here` size (1–100, percent of terminal) |
| `actions[].listener` | No | Marks the action as an event-only endpoint. It still fires on matching `on:` events, but is hidden from every user-facing surface (palette, help popup, shell completion). Direct invocation via `jin plugin run <plugin> <action>` remains available for debugging. Requires a non-empty `on:` — a listener with no events has no runtime purpose |
| `actions[].handoff` | No | Marks a synchronous structured PR-handoff provider. It is hidden from ordinary action surfaces and can run only through `jin session pr-handoff`. Requires empty `on` and forbids `listener`/`popup` |
| `on` / `popup` (top-level) | v1 only | Legacy v1 fields; forbidden in v2 (validation error). Put them under `actions[]` instead. Top-level `timeout` is **not** in this group — it stays valid in v2 (row above) |

`install.source` and `install.release_asset` are mutually exclusive.

`config.yaml` only enables/disables plugins and tunes dispatch timing (below) — it never duplicates manifest fields.

**Listener actions** are the common pattern for a plugin that both reacts to
events and exposes a UI. Split the two concerns so only the UI part appears
in the palette:

```yaml
actions:
  - id: list                        # user-facing: appears in the palette
    entrypoint: ./notifier.sh list
    label: "Show pending sessions"
  - id: listen                      # event listener: hidden from the palette
    entrypoint: ./notifier.sh listen
    on: ["status_changed"]
    listener: true                  # requires a non-empty on:
```

## What a plugin receives

Environment variables:

| Variable | Description |
|----------|--------------|
| `JIN_EVENT` | `status_changed`, `action`, or `pr_handoff` |
| `JIN_ACTION_ID` | ID of the manifest action that fired this run (`default` for v1 manifests / v2 default actions synthesised as `default`). A shared entrypoint script can dispatch on this instead of parsing argv |
| `JIN_SESSION_ID` | Session ID |
| `JIN_STATUS` | Current status |
| `JIN_PREV_STATUS` | Previous status (empty for an `action` run) |
| `JIN_AGENT_KIND` | Adapter kind (`claude`, ...) |
| `JIN_WORKDIR` | Session's working directory |
| `JIN_TMUX_PANE_ID` | tmux pane ID, if known |
| `JIN_NOTIFY_KIND` | Notification kind for this transition: `task-complete`, `error`, `permission`, or empty when the transition triggers no notification |
| `JIN_PLUGIN_DEPTH` | Chain depth — see [Constraints](#constraints) |
| `JIN_HANDOFF_KEY` | PR handoff only: stable idempotency key, also present in stdin JSON |
| `JIN_SOCKET` | Daemon socket path; the `jin` CLI a plugin invokes picks this up automatically |
| `JIN_BIN` | Absolute path of a `jin` matching the running daemon — a copy jind-ai keeps under its state directory, so it stays valid even if the binary the daemon was launched from is rebuilt or removed. Prefer `"${JIN_BIN:-jin}"` over a bare `jin` — a `jin` found on PATH may be an older install that lacks newer subcommands |
| `JIN_DEBUG` | `1` when the daemon is running with debug logging on, so a `jin` the plugin calls back into records what it does too. Omitted — not set to `0` — otherwise |
| `JIN_CALLER_TMUX_SOCKET` | Action runs only: socket path of the tmux server the invoking CLI ran inside (from its `$TMUX`). Unset — not empty — when the caller was outside tmux |
| `JIN_CALLER_TMUX_PANE` | Action runs only: the invoking CLI's pane ID (from its `$TMUX_PANE`). Unset when unknown |

The same data is also written to **stdin as JSON** (same fields, snake_case;
caller tmux context is env-only).

### PR handoff provider contract

A handoff action receives a schema-versioned document containing the session
ID, repository name when known, base/head commit IDs, branch, workspace
fingerprint, bounded diff counts, the review time, and an optional current
reported-check result. It never contains paths, filenames, patch text, prompts,
transcripts, environment values, or credentials.

Return exactly one JSON object on stdout:

```json
{
  "status": "succeeded",
  "provider": "github",
  "id": "123",
  "url": "https://github.com/acme/app/pull/123"
}
```

`status` is `succeeded` or `failed`; a success requires `id` or `url`.
URLs must be HTTP(S) without credentials, query, or fragment. stdout is capped
at 32 KiB and each persisted field has a smaller bound. Diagnostics belong on
stderr. Any timeout, non-zero exit, malformed/oversized result, or lost daemon
response is an unknown external outcome: providers must implement the supplied
idempotency key so receiving it again queries or safely converges on the same
PR rather than creating another one.

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

`jin pane split` takes a "named slot": `--name` makes the split idempotent —
a repeated call with the same name reuses the existing pane instead of
stacking a new one each time an event fires — and `--no-focus` keeps focus on
the session's own pane while it does. This is the pattern for an
event-driven plugin that opens a side pane (a monitor, a log tail, ...)
without spawning a new one on every invocation:

```bash
jin pane split "$JIN_SESSION_ID" --name monitor --no-focus --direction right --size 30% -- <cmd>
jin pane split "$JIN_SESSION_ID" --name monitor --no-focus --if-exists respawn --direction right --size 30% -- <cmd>  # restart it instead of leaving the old process running
jin pane close "$JIN_SESSION_ID" --name monitor         # tear it down
jin pane split --here --name monitor --no-focus -- <cmd> # same, over the caller's own pane instead of a session's
```

`--if-exists` defaults to `noop` (reuse the pane as-is); `error` fails
instead of reusing, for callers that want to detect the slot is already
taken. The daemon serializes named-slot calls; `--here` runs without that
arbitration, so concurrent calls for the same slot name are not guaranteed
idempotent.

**What a pane opened by `jin pane popup` / `jin pane split` receives**:
`JIN_SOCKET`, `JIN_BIN`, `JIN_DEBUG` and `JIN_SESSION_ID` — the same jin you
were told to call back into, and the session the pane's work belongs to. Call
back from inside a popup or a split pane exactly as you would from the plugin
itself, `"${JIN_BIN:-jin}"` included. Every pane gets them: by selector or
`--here`, a slot restarted with `--if-exists respawn`, and a split with no
command at all. A value jind-ai does not know arrives empty rather than
omitted, because a key left out of a pane's environment is one tmux fills in
from its server. That is also why `JIN_DEBUG` is empty here when debug logging
is off rather than absent as it is above; it is never `0` in either place.

A pane also gets `JIN_PLUGIN_DEPTH` empty. That is not part of the identity —
it says the pane continues no plugin's chain, so a depth some process left in
the tmux server cannot be read as this pane's caller and silently refuse every
`jin plugin run` you make from here. See [Constraints](#constraints) for what
the depth does bound.

**Compatibility contract**: treat any environment variable, JSON field, or CLI
flag you don't recognize as something to ignore, not an error. jind-ai only
adds to this surface within a `schema_version`; breaking removals happen
across a `schema_version` bump (or, pre-1.0, across a minor jin release).

## Install / update / remove / list

```bash
# From the plugin registry (see plugin-registry.md)
jin plugin ls-remote                              # list plugins in the registry
jin plugin install jind-ai-notifier               # latest release; `plugin update` follows the plugin
jin plugin install jind-ai-notifier -v 0.2.0      # pin a specific version; `plugin update` will not move it
jin plugin install jind-ai-notifier --force       # override an unsatisfied jin compat range

# From a git source (github.com/, gitlab.com/, self-hosted, ssh URLs, ...)
jin plugin install github.com/owner/repo          # default branch; `plugin update` follows highest semver tag
jin plugin install github.com/owner/repo@v1.2.0   # pinned to a tag/branch/SHA; `plugin update` will not move it

# From a local directory, symlinked in place (development)
jin plugin install --link ./my-plugin

jin plugin update <name>
jin plugin remove <name>
jin plugin list          # NAME / VERSION / STATE / SOURCE; --json for scripting

# Validate a manifest — same checks the registry crawler runs
jin plugin validate                               # defaults to .
jin plugin validate --github-actions              # emit ::error / ::warning annotations
```

A git install/update shows the manifest (`name`, `version`, `on`,
`entrypoint`, `build`) and the commit SHA it resolved to, and asks for
confirmation (`--yes` to skip) before
touching anything; the approved commit SHA is recorded in
`plugins.lock.yaml`, so a later `install`/`update` never silently lands on a
different commit than the one you saw. A `--link`ed plugin skips this —
linking a local path is itself the trust decision, and jind-ai never runs
`build:` for a linked plugin.

**`jin plugin update <name>` resolves the plugin's latest release** for
an unpinned install: registry names resolve to the registry's declared
`latest_version`, and raw git-URL installs pick the highest semver tag
from `git ls-remote --tags` (falling back to the locked ref when the
remote advertises no semver tags). A pinned install — one that used
`-v <ver>` on the registry path or `@<ref>` on the git-URL path — is a
no-op with a message pointing at reinstall as the way to move it. This
mirrors install: the "give me latest" and "hold this exact ref" intents
are decided once at install time and honoured by every later update.

## Language-specific guidance

- **Shell / single file** — commit the script, point `entrypoint` at it,
  and omit `install.source.build`; add a `chmod +x` step only if the
  script is generated or the exec bit isn't preserved in git.
- **Node.js / TypeScript** — bundle to `dist/` (esbuild etc.) as one build
  step; resolving dependencies at runtime (bun/deno) works too, but that
  first-dispatch network fetch can fail silently since dispatch is fail-open
  — a pre-built bundle is more predictable.
- **Go / Rust / other compiled languages** — declare a build sequence under
  `install.source.build`; each element runs as its own process (no shell
  piping between elements) so the binary matches the user's platform/arch
  (and `go.sum` / `Cargo.lock` give reproducibility). Builds run once per
  install/update; jind-ai does not resolve dependencies or detect a
  toolchain for you — document what's required in your plugin's own README.
  A non-zero exit fails the install/update atomically (nothing is left
  half-installed), with output kept at
  `~/.local/state/jind-ai/plugin-logs/<name>-build.log`. The whole sequence
  shares one budget (`plugins.build_timeout`, default 300s), not one budget
  per step.

  **The build environment is an allowlist.** A build step receives `PATH`,
  `HOME`, `USER`, `SHELL`, `LANG`, `TERM` and `LC_*` from the process that
  started it, plus `npm_config_ignore_scripts=true` (a supply-chain guard you
  can override inside your own build step) — nothing else. If your build needs
  `JAVA_HOME`, `CARGO_HOME`, a registry token or any other variable, derive it
  inside the build step; do not assume it is inherited. `jin plugin validate
  --run-build` applies the same filter and the same default budget, and it
  reads neither your own `plugins.build_timeout` (that setting belongs to
  whoever installs your plugin) nor your manifest's `timeout:` (that one
  bounds a dispatch, not a compile) — so a build needing more than an install
  grants fails at your desk instead of theirs. It cannot vouch for what does
  get through: your `PATH` is still yours, so a toolchain only you have
  installed still builds fine locally. Name what your plugin requires in its
  own README. The build runs with your own user privileges — it is not
  sandboxed.

## Constraints

- **No persistent processes.** jind-ai runs a plugin per event/action and
  tears it down; don't build a long-running daemon into `entrypoint`. If you
  need one, run it yourself (manually, or as a systemd user unit) and keep
  the plugin a thin per-event client to it (e.g. `curl`).
- **Fail-open.** A plugin that errors, times out, or hangs never blocks a
  session's status pipeline — it's logged and the pipeline moves on. Timeout
  defaults to 30s (`timeout:` in the manifest).
- **Loop residual risk.** jind-ai debounces repeated dispatch of the same
  (plugin, session, event) within a short window (default 3s,
  `plugins.debounce` below) and rejects a plugin chaining another plugin run
  beyond one hop (`JIN_PLUGIN_DEPTH`). Neither catches a *slow* ping-pong
  (e.g. a plugin that sends a prompt whose eventual response re-triggers the
  same plugin a few seconds later) — avoiding that is on the plugin author.
  Neither reaches a run started from inside a `jin pane popup` / `jin pane
  split` child either: the depth travels in the plugin's own environment, and a
  pane is given `JIN_PLUGIN_DEPTH` empty rather than the depth of whoever
  opened it, so such a run begins at depth 1 again, and the debounce window
  covers status dispatch rather than `jin plugin run`. Treat a chain you start
  from a popup as unbounded and stop it yourself.

## Config (`~/.config/jind-ai/config.yaml`)

```yaml
plugins:
  enabled: true          # default true; false disables all plugin dispatch
  disabled: ["notifier"] # disable individual plugins by name
  build_timeout: 300  # seconds for an install/update's whole build sequence (default 300)
  debounce: 3          # seconds, dispatch debounce window (default 3)
```

## Compatibility

Plugins declare a semver constraint on the jin binary via `jin:` (e.g.
`">=0.7.0"`, `"^0.7"`). Checked at install/update (fail-closed — a plugin
outside the range is rejected before anything is written) and again at
every dispatch (fail-open — an incompatible installed plugin is skipped,
logged once, and shown as `incompatible` in `jin plugin list`, with `jin
plugin run` pointing you at `jin plugin update <name>`). Development builds
(`jin --version` reporting `dev` or an unstamped value) satisfy every
constraint so local plugin work is unblocked.

The `schema_version` field is orthogonal to `jin`: it identifies the
manifest generation. jind-ai supports schema versions in a window
`[min, current]`, currently both `1`, and older schemas will be honoured up
to two generations back once we start bumping.

## Debugging a plugin

```bash
export JIN_DEBUG=1
tail -f ~/.local/state/jind-ai/plugin-debug.log        # dispatcher decisions
tail -f ~/.local/state/jind-ai/plugin-logs/<name>.log  # a plugin's own stdout/stderr
```

The flag reaches the plugin as `JIN_DEBUG=1`, so a `jin` the plugin calls back
into logs as well — `hook-debug.log` for `jin hook`, `daemon-debug.log` for the
daemon side of anything else it asks for.

---

Part of the [jind-ai](../README.md) documentation.
