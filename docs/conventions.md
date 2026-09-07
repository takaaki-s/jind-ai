# Coding Conventions

## Language & Formatting

- Go 1.26
- Always run `make fmt` (go fmt) before committing
- `make lint` runs golangci-lint; its configuration lives in `.golangci.yml`
- Comments should be in English
- Technical terms, struct/function names remain in English

## Comments

One rule: **write what the code cannot say and a test cannot enforce.**
Everything else is deleted, not shortened.

Drop:

- Restatements of the code (`// EnsureTrustState sets hasTrustDialogAccepted=true`)
- Contracts a test already enforces — the test is the statement
- Rejected alternatives, and why a case that does not exist is absent. These
  answer a reviewer, not the next implementer
- History ("this used to…", "before the refactor…") — that belongs in the commit
  message, where it cannot go stale without anyone noticing
- Another file's reasoning, retold. Link to where it is decided
- The story around a measurement. Keep the number, drop the narrative

Keep:

- An exported symbol's contract, in a line or three: what a caller gets wrong
  without it
- External facts the code cannot show — tmux's behaviour, an agent's on-disk
  format, an OS quirk, a measured threshold
- A "why" that will be reintroduced if removed. One line
- Directives (`//go:*`, `//nolint`)

The same paragraph written twice is the most common finding. State it once, at
the widest scope that covers every case, and link to it from the others.

## Error Handling

- Propagate errors to the caller via return
- Log only at boundaries (daemon server, manager, etc.)
- Wrap with `fmt.Errorf("context: %w", err)`

## Spawning a process

A child process that a context can cancel is wired with
`internal/procgroup.KillOnCancel`, called before `Start`. It puts the child in
its own process group and escalates SIGTERM to a group-wide SIGKILL after
`procgroup.GracePeriod`.

Not a nicety: `exec.CommandContext` signals the leader PID, and `Cmd.WaitDelay`
escalates against that same PID, so neither reaches a grandchild. Everything
jind-ai starts goes on to start something else — a plugin's interpreter, a
worktree hook's bash, `opencode export` — which makes "the leader is gone" and
"the work has stopped" different statements. The same handful of lines was
rederived twice before it was pulled into one place, and the third caller is
what paid for the package.

## Atomic file writes

Publishing a file through a temp sibling and a rename goes through
`internal/atomicfile.Write`, not a fresh create/write/chmod/close/rename. The
same handful of lines was independently rederived three times before it was
pulled into one place.

`Write` takes the temp-name pattern as an argument because callers scan the
directory they write into — state the rule your own scan imposes at the call
site, as `internal/session`'s `tmpSuffixPattern` and the opencode adapter's
`pluginTmpPattern` do.

Two deliberate exceptions, both documented where they live:

- `copyExecutable` (`internal/session/hookbinary.go`) streams through `io.Copy`
  instead of taking a `[]byte`, so a whole executable never lands in memory.
- `writeAtomic` (`pkg/plugin/manifest/registry.go`) keeps its own copy so that
  package stays free of module-internal dependencies.

### Unchecked returns

`.golangci.yml` excludes `Close`, `Flush`, `os.Remove`, `fmt.Fprint*` and
`os.Setenv`/`os.Unsetenv` from errcheck. errcheck matches on message text and
cannot tell the safe cases from the unsafe one, so the rule lives here instead.

**Data you wrote must be durable before the function returns.** Reach that with
a checked `Close` — `internal/atomicfile/atomicfile.go` does
`if err := tmp.Close(); err != nil` before the rename — or a checked `Sync`, as
in `internal/worktreehook/allowlist.go`. A bare `defer f.Close()` on a written
file is fine only once one of those has run, on an error path that discards the
file anyway, or where losing the tail costs nothing (the plugin and hook logs).

Read paths, sockets, and cleanup expected to fail need no check —
`internal/atomicfile/atomicfile.go`'s `defer os.Remove(tmpName)` is a no-op once
the rename succeeds.

## Debug Logging

Use `internal/debug`. Do not duplicate the pattern per package — an earlier
version of this section told you to, and what got duplicated was the decision
about what the flag means and how a line is stamped, which is exactly what has
to agree across files that are read side by side.

```go
var debugLog = debug.NewLogger("daemon-debug.log")   // one per log file
```

- `debug.NewLogger(<file>)` writes to the state dir, and is a no-op when
  debugging is off, when the caller is a test binary, or when the state dir
  cannot be resolved, so callers need no guard of their own.
- `debug.Enabled()` is the answer to "is debugging on" for code deciding what
  *this* process does — routing a child's output to stderr as well as its log,
  for instance. Do not re-read the environment variable.
- Putting the flag into a *child's* environment is a different question and has
  a different answer: `internal/jinenv`, which carries it alongside the socket
  and the binary path. A child needs all three to call back into the jin that
  started it. `daemon.NewServer` reads the flag once, there, and hands the
  resulting `jinenv.Identity` to every spawner — do not read it again at a spawn
  site. Each site once answered separately, and no site answered completely.

  Two exceptions, and it is their shape that matters. The first: `--here`
  (`callerPaneEnv` in `cmd/jin/cmd/pane.go`) never reaches the daemon, so there
  is no one to hand it an identity. It forwards its own process's — a plugin's,
  or an agent pane's shell's — rather than deriving anything, which is the same
  rule stated from the other side: pass on what you were given. Making it ask
  the daemon would cost `--here` the property that it works while the daemon is
  down. A spawn site may read the environment only when nothing upstream of it
  could have been asked.

  The second: `jin ui` (`uiIdentity` in `cmd/jin/cmd/tui.go`) passes the same
  test from the other direction — it is not downstream of anyone. It is
  the process that picks which daemon this UI manages, so there is no one above
  it to ask — asking the daemon which socket it is on would be asking the answer
  to supply itself. Everything it starts (the TUI's pane, every popup, the outer
  tmux session the key bindings read) is handed that identity rather than
  deriving one, because those all read the tmux *server's* environment when left
  alone, and that was measured naming a different daemon than this invocation
  validated, 3 trials of 3. Only the socket is resolved there: `BinPath` and
  `Debug` are forwarded from this process's environment, following the first
  exception's rule. `BinPath` is the one that could have been derived —
  `os.Executable()` is right there — and must not be.
- `debug.Untrusted` / `debug.UntrustedBytes` bound and quote any value the
  local process did not choose, so one payload cannot forge entries or fill the
  file.

The line format lives in `NewLogger` and nowhere else. It carries a full date
on purpose: these files are appended to and never rotated, so one accumulates
days of a long-lived daemon, and a clock-only stamp cannot order two lines
across a midnight. `internal/debug`'s tests pin it.

A test binary gets a no-op, whatever `JIN_DEBUG` says, and there is nothing to
opt into or remember. This is where the reason lives; the code says it in one
line and points here.

It is not tidiness. The loggers above are package-level variables, so each
resolves its path when its package is initialized — before `TestMain`, and long
before any `t.Setenv` — which means a test cannot redirect one for itself even
if it tries. A suite run with the flag on therefore appended to the same file
the daemon writes: measured on a real one, 955 of its 1121 status-transition
lines were fixtures, carrying session names straight out of the suite. Read
without excluding them, one transition appeared 245 times and looked endemic;
production had produced it once. Nothing failed while that was true. The file is
a diagnostic, so misreading it is the whole damage.

Isolating `$XDG_STATE_HOME` per package was the alternative, and it fails twice
over: it is the "remember to call this from `TestMain`" contract that
`testutil.IsolateFromRealDaemon` already carries and that most packages needing
it do not call, and it does nothing at all for anyone who exports
`XDG_STATE_HOME` pointing at their real state directory. Deciding in the one
constructor leaves nothing to remember.

The consequence to know about is that `JIN_DEBUG=1 go test` prints nothing to
these files. That capability is what was doing the harm, so it was not worth
keeping — but if a future change needs it back, give it a destination of its own
rather than letting the ambient one through.

The guard covers the process it runs in and nothing else. `Enabled()` is
deliberately still true under the flag, so a test that starts a real jin as a
child starts one that logs normally: isolate that child's `XDG_STATE_HOME`
yourself.

## Configuration Access

- Always access settings through `config.Manager`
- Do not call `viper` directly (outside the config package)
- `config.Manager` and `config.StateManager` are separate instances

## Concurrency

- `sync.RWMutex` field name is `mu`
- Lock ordering: session.Manager.mu is the central lock; auxiliary locks
  (`tmuxInitMu`, `paneSlotMu`) are always acquired BEFORE `mu` and never
  while holding it
  - `session.Store.saveMu` is below all of them: `Store` never calls back into
    `Manager`, so it is a leaf. It IS reached under `mu` — the two saves in
    `startSessionTmux` keep the lock — which is safe only while that stays
    true
  - Exception: `claude.trustMu` is taken under `mu`, because the adapter's
    `Setup()` is called from `startSessionTmux` and cannot see the lock. It is
    safe only because it is a leaf — nothing beneath it re-enters
    session.Manager. Anything else added there must keep that property
  - No adapter carries state from its `Setup` to its `SpawnCommand`
    (`session.SpawnOptions.StateDir` says why), so none of them needs a lock
    for that hand-off — the three `setupMu`s that used to guard it are gone.
    Adapters do still hold locks for their own reasons, and two of them are
    reached under `mu`, because `startSessionTmux` runs under
    `StartBackground`'s `mu.Lock()` and calls both `Setup` and `SpawnCommand`:
    `claude.trustMu` (via `Setup`) and the `Locator.mu` guarding Codex's
    rollout-path cache (via `SpawnCommand` on a resume). Both are therefore
    covered by the exception above, and both keep the property it requires:
    nothing beneath them re-enters session.Manager. A lock appearing between
    `Setup` and `SpawnCommand` specifically is the thing to question: it means
    something is being remembered that the spawn was already handed
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
