# Changelog

goreleaser assembles per-release notes from Conventional Commits history and
attaches them to the corresponding [GitHub Release](https://github.com/takaaki-s/jind-ai/releases).
This file is the curated overview — highlights per release, not a per-commit
log.

## Unreleased

### Fixes

- **Codex sessions no longer report the `permission` status.** Codex raises
  its approval hook when an approval path opens, which is not the same as a
  human being asked — and nothing in the payload jin receives says which it
  was. jind-ai mapped that hook to the `permission` status anyway, so a session
  whose approval resolved without anyone answering kept claiming it was waiting
  on you. The status has no timer behind it, so it stayed until some later hook
  happened to disagree, and `jin session respond` could not clear it: that
  adapter deliberately does not read Codex's dialog.

  The hook now maps to `thinking`, with no notification, and daemon-restart
  recovery rewrites a `permission` left on disk by an earlier version to
  `thinking` as well.

  **This changes what orchestration sees on codex sessions**, and only on
  codex: `jin session wait --until idle,permission` returns only on `idle`, no
  `permission` `JIN_NOTIFY_KIND` is dispatched to plugins, and a codex child
  that really is waiting on an approval reads as `thinking` and stays there —
  no timer moves a `thinking` session — so answer it by attaching to the pane.

- **Daemon-restart recovery no longer discards a hook that arrives while it is
  probing.** Recovery asks the agent adapter to re-derive a stale status, and
  that answer is computed from a snapshot taken before the tmux probes run. It
  was then applied even when a hook had moved the session in the meantime, so a
  turn that ended mid-recovery could be overwritten by the older reading. The
  answer is now withheld whenever the live status has moved since the snapshot,
  which is the rule the no-adapter fallback already followed. Affects every
  agent kind. Claude Code and opencode sessions are
  unchanged.

## 0.11.0

### Fixes

- **A Codex session that died before its first hook can be started again.**
  jind-ai pre-mints a session UUID when it creates the record and marks the
  agent session started at spawn, so a session whose hooks never ran kept that
  pre-minted id with the flag already true. Every start from then on ran
  `codex resume <uuid>` against an id no agent had ever reported, which exits 1
  within a second. Codex 0.149.1 made this the ordinary path rather than an
  edge case: its hook-trust gate means untrusted hooks never run, so the first
  hook never arrives.

  The auto-recovery written for exactly this could not fire. The quick-resume
  failure window was 10s and the pane monitor's poll interval was also 10s, so
  the first observation of a dead pane landed just outside the window —
  measured at 10.034s, twice out of twice. That retry branch had never run in
  production. The window is now defined against the interval, so the two cannot
  drift back into equality.

  A session record now carries whether the agent itself named the id, and the
  Codex adapter requires that before resuming. Records written by earlier
  versions are backfilled by a schema migration and go on resuming as they did.
  A failed resume is retried as a fresh start; an agent you quit inside the pane
  is not, because quitting exits 0 and a failed resume does not. See
  [docs/gotchas.md](docs/gotchas.md#codex-adapter).

- **An opencode session is no longer rewritten by an opencode the agent starts
  for itself.** opencode is the one supported agent told about its hooks
  through the environment rather than the command line, so everything the agent
  runs inherits both the plugin directory and `JIN_SESSION_ID`. A nested
  opencode therefore loaded jind-ai's status plugin and reported its own new
  session against the parent's: measured with the child started from the
  agent's own environment, the parent's recorded agent-session id was replaced
  by the child's in 3 of 3 runs — which is the id a later `opencode --session`
  reopens — and the parent was reported as started, then working, then finished
  for a turn it never ran.
  Claude Code and Codex were unaffected in the same test (0 of 3 in both arms,
  with their own hooks firing throughout).

  The plugin now marks whatever the agent starts and declines to report from
  inside a marked process. Children opencode starts on the agent's behalf — the
  shell tool, the bash tool, the pty API — are all covered; a nested opencode
  launched from an LSP or MCP server is not, because those are started without
  the hook the mark travels on. See
  [docs/gotchas.md](docs/gotchas.md#opencode-adapter).

### Breaking changes

- **`JIN_SESSION_NAME` is no longer passed to `.jin/worktree-post-create.sh`.**
  It was documented as the session name given via `--name`, but no such flag
  exists: the value came from `-d/--description`, and with `-d` omitted — the
  default — the hook received an empty string every time. A human-readable
  label is also not what the hook is for; it runs inside the creation
  transaction, before the agent starts and before the description is settled.
  `JIN_SESSION_ID` remains as the stable key to namespace side effects by.

  A hook that reads the variable under `set -u` now exits non-zero, and a
  failing hook rolls back the worktree and its branch. Drop the reference, or
  guard it as `${JIN_SESSION_NAME:-}`.

  Two documentation errors went with it: the `{name}` placeholder rows named
  the same non-existent `--name` (they mean `--worktree-name`), and
  `JIN_WORKTREE_BASE` carries the bare branch name while the worktree is cut
  from `origin/<name>`.

## 0.10.0

### Features

- **`jin session result` works on Codex sessions.** It reads the agent's own
  rollout log and returns the same `text` / `tool_use` / `tool_result` entries
  a Claude Code session returns, so an orchestrator can inspect what a codex
  child actually did instead of falling back to `git diff`. Two filters are
  weaker there than on Claude Code and the difference matters when you act on
  them: `--errors-only` cannot see a command that merely exited non-zero
  (Codex records no exit code), and `--tool` barely discriminates (Codex
  declares the same tool name for nearly everything it runs). `usage` and
  `thinking` are not available on codex entries. See
  [docs/gotchas.md](docs/gotchas.md#session-result), or `jin docs show gotchas`
  for the orchestration-facing version.

- **`jin session result` works on opencode sessions too.** opencode keeps its
  conversation in a SQLite database, so jind-ai asks opencode for it: the
  command runs `opencode export --pure <session id>` and parses the result.
  Nothing is recorded, no copy is kept, and no SQLite dependency or
  database-schema coupling is taken on. Where the codex reader is weak this one
  is not: `thinking` carries the real reasoning text, entries carry `usage`
  (the sum over the messages opencode splits one turn across — its
  reasoning-token count has no field in jind-ai's shape and is dropped), and
  `--tool` matches real per-tool names. Until now the command answered zero
  entries and exit 0 on an opencode session, which reads exactly like a child
  that ran and said nothing.

  The limits matter more than the capabilities. `opencode` has to be on the
  **daemon's** PATH — sessions start through your login shell, which resolves a
  version manager's shims the daemon may not see — and a read that cannot
  happen is an error, not an empty result; the one case that stays empty and
  exits 0 is a session whose agent has not reported its own id yet, the same
  window codex has. Each read costs 1.45–1.77s whatever the session size (the
  time is opencode's start-up), so `--since` is no cheaper than a full read and
  fewer, larger polls are the way to use it. `--errors-only` is exact for
  `bash` and partial elsewhere: it catches whatever opencode flagged as an
  error, plus a shell command that exited non-zero (opencode records those as
  `completed`, so the flag comes from the exit status recorded alongside it) —
  and no other tool records an exit status at all (0 of 161 calls). Timestamps
  are opencode's own except where its clock disagrees with the order of the
  conversation — parallel tool calls, 13 of 620 blocks measured — where the
  previous value is carried forward, so two entries can share a stamp and
  `--since` can skip one, exactly as on the other two kinds. Revert and undo
  are not tracked, so content an opencode child undid can still appear in the
  result. See
  [docs/gotchas.md](docs/gotchas.md#session-result), or `jin docs show gotchas`
  for the orchestration-facing version.
- **Session rows and `jin session info` show a codex session's last messages.**
  The two message previews — the TUI's second line, the `session list` rows,
  and `last_user_message` / `last_assistant_message` in
  `jin session info --json` — were read with the Claude Code transcript reader
  whatever agent the session ran, so on codex and opencode they were blank
  permanently. They now go through the session's own adapter, like
  `jin session result` already did. Claude Code sessions show exactly what they
  showed before. opencode rows stay blank on purpose: reading an opencode
  conversation means running `opencode export`, which is far too expensive for
  a path the TUI refreshes on a timer. These stay a preview rather than a result: an unreadable
  transcript leaves them empty and the command still succeeds, so an empty
  preview is not evidence a child said nothing — use `jin session result` for
  that. `jin session output` is unchanged and still reads Claude Code only.

### Behaviour change

- **Workspace trust for Claude Code sessions is now written to
  `~/.claude.json`**, the file Claude Code actually reads it from, instead of
  `~/.claude/settings.local.json`, where it was silently ignored. A session
  started in a fresh worktree no longer stops at the trust dialog.
- The old file is neither migrated nor deleted — jind-ai just stops writing
  to it. Installs that ran earlier versions have a large dead `projects` map
  there, which is safe to remove when the file holds nothing else; see
  [docs/gotchas.md](docs/gotchas.md#claude-code-adapter) for the steps.
- Trusting the directory your worktrees are created under, once, stops
  jind-ai writing to `~/.claude.json` for every session below it — Claude
  Code inherits trust from ancestor directories. Sessions started without
  `--worktree` run elsewhere and are not covered by that.

- **`jin plugin validate --run-build` now builds the way an install builds.**
  It had its own copy of the build loop, and that copy ran in whatever
  environment the caller happened to have, under a timeout applied to each
  command separately and sized from the manifest's `timeout:`. An install
  runs the build in a curated environment (`PATH`, `HOME`, `USER`, `SHELL`,
  `LANG`, `TERM`, `LC_*`, plus `npm_config_ignore_scripts=true`) under one
  `plugins.build_timeout` budget for the whole sequence. So the check that
  exists to predict the install could disagree with it in both directions: a
  build leaning on a variable exported in the author's shell — or granted a
  longer budget by a generous `timeout:` — passed validation and failed on
  the user's machine, while a build leaning on `npm_config_ignore_scripts`
  failed validation and installed fine.

  Both commands now go through one implementation, and `--run-build` spends
  the default `build_timeout` on the whole sequence: not the author's own
  configured value (that setting belongs to whoever installs the plugin) and
  not the manifest's `timeout:`, which bounds a dispatch rather than a
  compile. For plugin authors this means `--run-build` is stricter than it
  was: if your build needs `JAVA_HOME`, a registry token, longer than the
  default budget, or anything else outside the allowlist, the install was
  already going to fail without it. It still cannot vouch for the *contents*
  of what the allowlist passes — your `PATH` is your own — so keep naming
  your plugin's requirements in its README. Rule #13 findings now distinguish
  a timeout from a non-zero exit, where a timeout previously surfaced as
  `signal: killed`, and build progress prints as `--- build step i/N: … ---`
  instead of `[build] …`.

  `jin plugin install` / `update` behave as before, save for one message: a
  build step that fails to *start* now names the build log, as every other
  build failure already did.

- **Claude Code's `hooks-settings.json` is rewritten at every session start**
  instead of once per daemon run, so one deleted or hand-edited by mistake is
  back in place at the next start rather than staying broken until the daemon
  is restarted.

### Bug Fixes

- **`jin ui` now decides which daemon its own UI talks to.** The TUI runs in a
  pane of the outer tmux server, so it used to resolve a daemon from that
  server's environment — which holds whatever forked the server, not what the
  `jin ui` invocation meant. The two were measured disagreeing in both
  directions, 3 trials of 3 each: with a stale socket on the server, `jin ui`
  confirmed a daemon was running and the TUI then died against a different,
  dead one (`daemon is not running`, on a UI that had just started); with the
  daemons swapped, `jin ui` refused to start while the daemon the TUI would
  actually have used was up. The popups the TUI opens took the same
  environment. The identity now comes from `jin ui` itself and travels to the
  pane, to every popup, and to the outer tmux session the key bindings read.
  What was measured is a stale `JIN_SOCKET` on the tmux server; the same split
  is reachable wherever the server's `XDG_RUNTIME_DIR` or `TMPDIR` differs from
  the shell's, since the default socket path is derived from those. `JIN_DEBUG`
  travels the same way, which is a behaviour change worth knowing: a `jin ui`
  run without it now turns debug logging off for the popups that outer tmux
  server opens, where before they kept whatever the server was holding.

- **A plugin key binding no longer stops working because of a leftover
  environment.** A tmux server forked by a process that carried
  `JIN_PLUGIN_DEPTH` handed it to every pane, and the daemon then refused each
  `jin plugin run` from those panes as a plugin chaining another — measured
  3 of 3, and invisible, since the binding runs through `run-shell -b` with its
  output discarded. `jin ui` now states the depth as empty on its own tmux
  session, and the daemon records every `jin plugin run` its dispatcher declines
  to start in `plugin-debug.log` when it was started with `JIN_DEBUG=1` (a
  `jin plugin run` that cannot reach a daemon still leaves nothing behind). A
  depth stranded in the inner tmux server, the one the agents' panes live in, is
  the same fault and is not covered yet. A chain started from inside a
  `jin pane popup` is still unbounded by design — see the plugin Constraints in
  the README.

- **A session's agent is now told which daemon to call back to.** Its hooks
  used to find one by inheritance: a tmux pane is handed the tmux *server's*
  environment, and that server outlives any one daemon, so an agent reached
  whichever daemon happened to start the server. When the daemon had started
  it the value was right — by accident. When something else had, the hooks
  reached no daemon at all, and when an earlier daemon on a different socket
  had, they reached that one's address. All three were measured. The failure
  is silent by construction: `jin hook` exits 0 whether or not anyone answered,
  so the only symptom is a live session whose status never moves. Only
  installs that run the daemon on a non-default socket (`JIN_SOCKET`, `jin
  daemon --socket`) were affected.

- **Plugins now receive `JIN_DEBUG`.** A plugin calls back into jind-ai
  through `$JIN_BIN`, and that process decides for itself whether to record
  what it does — so `JIN_DEBUG=1 jin daemon start` used to turn on only
  jind-ai's own half of the exchange, leaving the callbacks silent. The
  variable is omitted, not set to `0`, when debugging is off.

  Both were the same gap: which jin a child should call back into is now
  answered in one place (`internal/jinenv`) rather than at each spawn site,
  which is also why an agent now gets `JIN_BIN` — the daemon's stable binary
  copy, so an agent that shells out to `jin` reaches the version that started
  it rather than whatever is first on `PATH`.

- **A plugin's `$JIN_BIN` now points at that same stable copy** instead of the
  daemon's live executable. The two are not the same file for long: `go build
  -o` over a running binary unlinks it and creates a new one at the same path,
  so a single rebuild while the daemon runs left plugins pointed at a build the
  daemon never was (measured 3/3). Where the IPC shape had changed, a callback
  failed with the protocol-mismatch message; where it had not, it succeeded
  against a binary nobody chose. Deleting the directory the daemon was launched
  from — a worktree, say — left the path gone outright and callbacks exiting
  127 (3/3); `"${JIN_BIN:-jin}"` does not fall back to `PATH` there, because
  `:-` substitutes only when a variable is unset or empty and a dead path is
  neither. Agents were already immune, which is the point: the two spawn sites
  now take the same value rather than each deciding.

- **A pane opened by `jin pane popup` or `jin pane split` is now told which
  jin opened it.** It was told nothing, so it took its environment from the
  tmux server, and what it saw depended on which process had forked that
  server: no `JIN_*` at all when the daemon or a plain shell had (3/3 each),
  an earlier daemon's values when that one had (3/3), and another session's
  `JIN_SESSION_ID` when the server had been forked from inside an agent's
  pane (3/3) — one `jin daemon start` run there arranges that, and from then
  on every session's popups carry the one id. That case is worse than the
  empty one: a stale id is a plausible UUID, so a popup that reads it acts on
  the wrong session and nothing reports an error. Agent panes carried the three
  values they knew on their command line already and were unaffected in all
  four (12/12) — but the same omission reached them through a fourth: with
  debug logging off, nothing was written for `JIN_DEBUG`, so an agent in a
  server forked from an environment that had it on ran with it on (3/3). That
  is closed here too.
  `JIN_SOCKET`, `JIN_BIN`, `JIN_DEBUG` and `JIN_SESSION_ID` now reach every
  pane `jin pane` opens, and the agent's own — popup and split, by selector or
  `--here`, a slot
  restarted with `--if-exists respawn`, and a split with no command — and
  arrive empty when a value is unknown, because a key left out is a key
  inherited. A `--here` pane is given the caller's own values rather than the
  daemon's, so it still works with the daemon down. Plugins no longer thread
  these through a command line by hand.

## 0.9.0

### Features

- **`jin plugin update <name>` now moves an unpinned install to the
  plugin's latest release** instead of re-cloning the locked commit
  (which made `update` a silent no-op on registry installs). Registry
  names resolve to the registry's declared `latest_version`; raw git-URL
  installs pick the highest semver tag from `git ls-remote --tags`,
  falling back to the locked ref when the remote advertises no valid
  tags.
- **Install-time pin captured in the lock**: a bare `install <name>` or
  `install <url>` marks the entry as unpinned (`update` will follow
  latest), while `install <name> -v <ver>` and `install <url>@<ref>`
  mark it pinned (`update` refuses to move it and points at reinstall
  as the way to bump). Old lock files without the field default to
  unpinned, matching the install-time behaviour of a bare install.

### Behaviour change (not a schema break)

- On the first `jin plugin update <name>` after upgrading, unpinned
  installs may move to a newer release — the pre-0.9.0 `update` was
  effectively a no-op that always re-cloned the locked SHA, so this is
  the first time the command actually delivers updates. Users who want
  the old "lock every install to the ref I first saw" behaviour can
  reinstall with a `-v` / `@ref` pin.

## 0.8.1

### Bug Fixes

- **`jin plugin ls-remote` and every other registry-consulting `plugin`
  path work again.** 0.8.0 tightened the plugin manifest's
  `schema_version` from 1 to 2 (`CurrentSchemaVersion = 2`), but the
  registry-document parser was sharing that constant, so every shipped
  0.8.0 binary rejected the live `registry.json` (`schema_version: 1`)
  with `schema_version 1 not supported (this build understands 2)`.
  Registry doc and plugin manifest now have independent version
  constants (`CurrentRegistrySchemaVersion` / `CurrentSchemaVersion`)
  and a regression test parses a literal
  `{"schema_version": 1, "plugins": []}` payload so a future manifest
  bump can never break registry parsing the same way.

## 0.8.0

### Features

- **A plugin can now declare multiple actions.** Manifests support an
  `actions:` list where each entry carries its own `id` / `entrypoint` /
  `on` / `popup` / `label`. The first entry is the implicit default action
  (no explicit flag). Palette rows, keybindings, and `jin plugin run` all
  operate at the action level. Existing v1 manifests (`schema_version: 1`
  with top-level `entrypoint` / `on`) are normalised at parse time into a
  single-action shape, so plugin authors need to do nothing to keep
  working.
- **`jin plugin run <name> [action]`** — an optional second positional
  argument selects the action. Omitted invocations run the default action
  (`actions[0]`) and keep the exact pre-existing output
  (`Started plugin <name> (global)`) so scripts that grep the CLI's
  success message stay working. Shell completion is two-stage: plugin
  names first, then that plugin's action IDs.
- **`JIN_ACTION_ID`** env var — every plugin run receives the ID of the
  action that fired it, so a shared entrypoint script can dispatch on
  `$JIN_ACTION_ID` instead of parsing argv.
- **`jin plugin install` / `update` consent screens are per-action.** v2
  manifests now render every action's `id` / `entrypoint` / `on`. This
  also fixes a v1-era rendering path that read the (now-forbidden)
  top-level `on:` field and left the `Events:` line empty for v2 plugins.
- **`jin plugin validate --run-build` checks every action's entrypoint**
  exists after build, not just the default action's — so multi-binary
  v2 plugins get the full sanity check.
- **`actions[].listener: true`** — marks an action as an event-only
  endpoint. It still fires on matching `on:` events, but is hidden from
  every user-facing surface (palette, help popup, shell completion).
  Lets a plugin split its listener from its user-invoked action without
  the listener cluttering the palette as an entry that does nothing when
  clicked. `jin plugin run <plugin> <action>` still accepts a listener
  ID directly for debugging. A listener with `on: []` is a validation
  error (`R22 RuleListenerRequiresOn`) — a listener with no events has
  no runtime purpose.

### Breaking changes

- **`keybindings.plugins.<name>.keys`** → **`keybindings.plugins.<name>.actions.<id>.keys`**.
  The old shape is detected at load time, a single WARN is logged per
  plugin, and that plugin's binding is dropped (TUI startup is never
  blocked). Rewrite by hand; for a plugin with only a default action,
  `actions.default.keys: [...]` is the shortest translation.

  ```yaml
  # Before (0.7.x — ignored with a WARN under 0.8.0)
  keybindings:
    plugins:
      notifier:
        keys: ["M-n"]

  # After (0.8.0)
  keybindings:
    plugins:
      notifier:
        actions:
          default:
            keys: ["M-n"]
          send-dm:
            keys: ["M-d"]
  ```

- **`plugin.EventDispatcher.RunAction`** signature is now
  `RunAction(name, actionID, ev, depth, actx)`. An empty `actionID` selects
  the default action; an unknown ID returns a synchronous error listing
  the available actions.
- **`daemon.PluginRunRequest`** gains an `Action string` field (empty =
  default action). IPC wire-compat is preserved: pre-0.8.0 clients simply
  omit the field and land on the default action, matching their previous
  behaviour.
- **`plugin.PopupSizeResolver`** signature is now
  `(pluginName, actionID string, m *manifest.PopupConfig) → (w, h string)`.
  The daemon's built-in resolver ignores `actionID` for now — per-action
  popup size in user config is out of scope for this release — but the
  argument is plumbed so a later config schema can widen without another
  breaking signature change.

## 0.7.2

### Bug Fixes

- **`jin plugin install <name>` / `jin plugin update` now clone from
  `github.com`.** Registry entries record `repo` as bare `owner/name`
  (the crawler's GitHub `FullName`), but the resolver was passing that
  string to `git clone` unchanged and hitting an unresolvable host.
  Bare entries are now prefixed with `https://github.com/` before
  clone; entries that already carry a URL scheme (mirrors, `file://`
  fixtures) pass through untouched.

## 0.7.1

### Features

- **`install.source.build` is now optional.** Plugins that ship a directly
  executable entrypoint (shell scripts, prebuilt binaries checked into the
  repo) can omit the `build` block entirely and point `install.source.entrypoint`
  at the script or binary. Only `install.source.entrypoint` remains required
  under `install.source`. Existing manifests that declare a `build` block
  continue to validate unchanged.

## 0.7.0

### Features

- **Plugin registry** — new `jin plugin ls-remote`, `jin plugin install <name>`
  (registry-resolved with SHA pin and a consent screen), and `jin plugin
  validate` commands. See [docs/plugin-registry.md](docs/plugin-registry.md)
  for the discover/install/publish flow and full flag reference.
- **Unified plugin manifest (`jind-ai-plugin.yaml`)** — the runtime dispatcher
  and the registry crawler now read the same file with the same schema. The
  old `jin-plugin.yaml` / `api_version` shape has been removed.
- **`pkg/plugin/manifest`** — the manifest package is now exported so the
  registry crawler and any third-party tool validate manifests bit-for-bit
  identically to jin itself.

### Breaking changes

`0.7.0` is a pre-1.0 minor bump and carries breaking changes to the plugin
system. See [docs/plugin-registry.md#pre-10-break-policy](docs/plugin-registry.md#pre-10-break-policy)
for the policy in full.

- The plugin manifest file is now `jind-ai-plugin.yaml` (was
  `jin-plugin.yaml`); the `api_version` field is gone and `schema_version: 1`
  takes its place. Existing plugins must migrate the file name, add
  `schema_version` / `name` / `version` / `description` / `jin:`, and move
  `run` / `build` under `install.source.{entrypoint,build[]}`.
- The built-in desktop notifier has been removed from the daemon. Install
  [`jind-ai-notifier`](https://github.com/takaaki-s/jind-ai-notifier) — the
  same notifier repackaged as a plugin — to restore the behaviour.
