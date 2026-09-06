# IPC Protocol

## Transport

- Unix domain socket: `$XDG_RUNTIME_DIR/jind-ai/daemon.sock` (fallback `$TMPDIR/jind-ai-<uid>/daemon.sock`)
- One request / one response per connection (no connection pooling)
- JSON encoding/decoding

### Which daemon a command reaches

`jin daemon` subcommands accept `--socket`; it is not a global flag. Every other
command takes `JIN_SOCKET` when set, and otherwise the path above. Both are
resolved per call (`getSocketPath`, `internal/paths.Socket`), not fixed at
startup.

A tmux server keeps the environment it was forked with, so a `jin` run from one
of its panes resolves against whatever that server holds. When the value has
gone stale — a rotated `$TMPDIR` where `XDG_RUNTIME_DIR` is unset, or an
`XDG_RUNTIME_DIR` from an earlier login — the run reaches whatever daemon lives
under the older path, measured 3 of 3, and does so without complaint, because a
daemon really is listening there. This reaches only tmux servers started outside
jind-ai. The agent panes and the TUI pane are handed `JIN_SOCKET` explicitly
(measured 3 of 3 in the same setup), and popups inherit it from the client that
opened them.

`jin ui` publishes the identity onto the outer tmux session on **every** start,
along with the key bindings that read it at fire time; neither write is
conditional. What is conditional is the TUI pane: one already running on a
different daemon is respawned rather than left behind, so a live UI and the keys
around it do not end up on two daemons. `identityMoved` in `cmd/jin/cmd/tui.go`
carries the reasoning and the measurements.

Other entries an earlier `jin ui` left on that session — `JIN_CURRENT_SESSION`,
`JIN_CURSOR_SESSION` — still name the previous daemon's sessions until something
overwrites them. They are UI state rather than routing, so nothing resolves a
daemon through them.

## Timeouts

`Client` bounds every exchange so a daemon that accepts a connection and then
stops responding cannot hang the caller forever. The bounds are tiered, not
per-action:

| Bound | Value | Applies to |
|---|---|---|
| dial | 2s | every request |
| request write | 5s | every request |
| response wait (default) | 60s | every action without its own entry below |
| response wait: `hook` | 10s | the agent-facing path — a stalled hook blocks the agent process itself |
| response wait: `stop` | 5s | the remedy for a wedged daemon, so it must not inherit the wedged-daemon bound |
| response wait: `pane-popup` | none | the handler runs `tmux display-popup -E`, which blocks for the popup's user-controlled lifetime |

**The client bounds an exchange only when it can name a duration that is
certainly longer than any legitimate handler run.** `pane-popup` has no such
value — its handler blocks for however long the user leaves the popup open —
so it defers to the bound the handler already owns. Guessing on its behalf
would not protect anyone: since a timeout is not a cancellation (below), a
bound that fires early just reports an unknown outcome for work that goes on
to succeed.

Dial and write stay bounded for every action, `pane-popup` included. The write
bound is a single constant rather than a per-action one because what it guards
does not vary: a request is one small JSON value, and the daemon decodes each
accepted connection on its own goroutine, so writing never waits on handler
work. A blocked write means the daemon stopped reading, and it is reported that
way rather than as a failure to respond.

The 60s response wait is deliberately generous. With `pane-popup` out of its
scope — and `hook` and `stop` on bounds of their own — what it covers is tmux
subprocess calls and local file reads, plus one handler with a named cost:
`send` waits for the prompt to appear in the pane before giving up, and that
wait is not the flat 5s this said for a long time — `sendVerifyBudget` scales
with the prompt, from roughly 5s for a short one to 22.6s at 8KB. Which makes
this bound the ceiling on that one: raising the send budget past what 60s
absorbs would only move the failure here.

Those handlers queue behind the manager lock, so 60s is sized to clear a
backlog of them; hitting it should mean "the daemon is wedged", not "this
machine is loaded". `new` and `delete` also live under this default: see
[Async completion](#async-completion) for why their handlers no longer need
an exception.

The table lists the bounds a Go client sets today, so `agent-signal` has no row:
the daemon dispatches it, but no client method sends it. It nonetheless lands on
the same agent-facing path as `hook`, so a client method added for it belongs on
the `hook` bound rather than the default — `hookRequestTimeout` in `client.go`
traces the path and says so.

**A timeout is not a cancellation.** The protocol has no cancel channel, so a
client that gives up does not stop the daemon — a mutating action may still
complete. Error messages for those actions
therefore report an unknown outcome rather than a failure, and point at
`jin daemon restart` instead of encouraging a blind retry; read-only actions
(`readOnlyActions` in `server.go`) get a plain timeout message instead, so the
warning keeps its weight where it matters. Any new action that mutates state
inherits this property by default; make it idempotent, or expect callers to
check state after a timeout.

`stop` is the one action that does not point at `jin daemon restart` when it
fails, for the obvious reason: restart stops through `Client.Stop()` itself,
so it would answer a stop that failed with the same stop. A daemon deaf to the
request needs a signal instead, and `Stop()` says so — but only once its
shutdown poll has run out. The predicate for "stop failed" lives entirely in
that poll's result, not in how the send attempt went: if the daemon is still
accepting connections once the poll gives up, `Stop()` reports the pkill
remedy regardless of whether the send blew a deadline, timed out dialing (an
error `sendWithTimeout` never wraps in `os.ErrDeadlineExceeded`, so a
type-based check would miss it), or even succeeded outright. A send that
fails but the poll finds the daemon gone anyway is still success — the poll,
not the send, is what `Stop()` trusts.

## Message Format

```go
// Request (client → server)
type Request struct {
    ProtocolVersion int             `json:"protocol_version,omitempty"`
    Action          string          `json:"action"`
    Data            json.RawMessage `json:"data,omitempty"`
}

// Response (server → client)
type Response struct {
    ProtocolVersion int             `json:"protocol_version,omitempty"`
    Success         bool            `json:"success"`
    Data            json.RawMessage `json:"data,omitempty"`
    Error           string          `json:"error,omitempty"`
}
```

`ProtocolVersion` is stamped on every request by `Client.send` and on every
response by `Server.handleConnection`. Either end rejects a message whose
version does not match its own — a pre-versioning peer sends 0, which counts
as a mismatch. Bump `daemon.ProtocolVersion` (`internal/daemon/protocol.go`)
whenever a wire message's shape changes; docs-only or refactor patches leave
it alone.

## Actions

| Action | Data Type | Description |
|--------|-----------|-------------|
| `new` | `NewRequest` | Create session (async; poll via `get`) |
| `list` | (none) | List all sessions (with last-message enrichment) |
| `get` | `IDRequest` | Get a single session (with last-message enrichment) |
| `send` | `SendRequest` | Send a prompt to a session (alias `prompt` on the CLI) |
| `respond` | `RespondRequest` | Answer a prompt an agent is blocked on; returns `RespondResponse` |
| `start` | `IDRequest` | Start session |
| `kill` | `IDRequest` | Kill session |
| `delete` | `DeleteRequest` | Delete session (async; poll via `get`, optionally with worktree) |
| `stop` | (none) | Stop daemon |
| `hook` | `HookRequest` | Claude Code hook event |
| `result` | `ResultRequest` | Fetch structured transcript entries from the session's own agent adapter (orchestration; fails for a kind with no reader) |
| `set-description` | `SetDescriptionRequest` | Update session description (empty resets to auto-generated) |
| `attention-seen` | `IDRequest` | Acknowledge a session's completion receipt; returns the postcondition `session.Info`. Idempotent, changes no process status |
| `agent-signal` | `AgentSignalRequest` | Deliver an out-of-band status signal from an agent adapter (currently only `kind="hook"` is wired) |
| `pane-popup` | `PanePopupRequest` | Open a tmux popup over a session's pane, running a command |
| `pane-split` | `PaneSplitRequest` | Split a session's pane, optionally running a command in the new pane (→ `PaneSplitResponse`) |
| `pane-close` | `PaneCloseRequest` | Close a named-slot pane created by `pane-split` with `name` |
| `pane-capture` | `PaneCaptureRequest` | Capture the visible contents of a session's pane |
| `pane-send-keys` | `PaneSendKeysRequest` | Send keys to a session's pane (literal text or tmux key names) |
| `plugin-run` | `PluginRunRequest` | Run a plugin on demand for a session (bypasses matcher/debounce; async) |

**Last-message enrichment** fills `Info.last_user_message` and
`Info.last_assistant_message` by reading the conversation through the session's
own agent adapter (`Manager.AttachLastMessages`). Unlike `result`, it never
fails the response: an adapter with no reader, a read error, and an agent that
has said nothing all leave the two fields empty and `success: true`. Clients
that need to distinguish those must use `result`.

## Async completion

`new`, `delete` and `plugin-run` accept the request, return an acknowledgement,
and continue their real work in a goroutine. The daemon uses this pattern
whenever the underlying I/O has no bound the client can name (git subprocesses,
`rm -rf` over an unknown-size checkout, plugin scripts) so the response wait
stays covered by the default 60s tier.

- `new` returns immediately with a session `Info` at `Status=creating`. The
  client polls `get` and observes the transition to `running` / `idle` on
  success or `stopped` + `error_message` on failure. Non-fatal warnings
  (e.g. post-create hook detected but not allowed) surface through
  `Info.creation_warning`, which persists until the session is deleted.
- `delete` flips the session to `Status=deleting`, then removes the worktree
  and drops the record. The client sees the record disappear on success or
  `Status=stopped` + `error_message` on failure.
- `plugin-run` writes its outcome to the plugin log; the response only
  confirms the run was dispatched.

The **synchronous pre-checks** for these actions still fail on the response:
`new` refuses an unknown agent kind; `delete` refuses a missing session, a
non-worktree target when `remove_worktree` was requested, and a dirty
worktree when `remove_worktree` was requested without `force_remove_worktree`.
Everything past those checks runs in the goroutine.

## Request Types

```go
type NewRequest struct {
    Description string `json:"description"`
    WorkDir     string `json:"work_dir"`
    Start       bool   `json:"start"`
    Fleet       string `json:"fleet"`                     // Fleet name for session grouping
    AgentKind   string `json:"agent_kind,omitempty"`      // Adapter kind ("claude" etc.); daemon defaults from config's default_agent when empty
    Model       string `json:"model,omitempty"`           // Agent model, spelled as that agent's CLI spells it; passed through unvalidated, persisted, and replayed on every resume

    Worktree       bool   `json:"worktree,omitempty"`        // Create a git worktree for this session
    WorktreeName   string `json:"worktree_name,omitempty"`   // Override auto-generated worktree name
    WorktreeBranch string `json:"worktree_branch,omitempty"` // Override auto-generated branch name
    WorktreeBase   string `json:"worktree_base,omitempty"`   // Override auto-detected base branch
    NoHook         bool   `json:"no_hook,omitempty"`         // Skip .jin/worktree-post-create.sh hook
}

// AgentSignalRequest carries a generic status signal from any agent adapter's
// out-of-band notifier. Manager routes the Payload through the registered
// agent's StatusSource.Interpret.
type AgentSignalRequest struct {
    JinSessionID string            `json:"jin_session_id"`
    Kind         string            `json:"kind"`              // "hook" (currently the only wired kind)
    Payload      map[string]string `json:"payload,omitempty"` // adapter-defined bag; for "hook": event, notification_type, cwd, stop_reason, agent_session_id
}

// SetDescriptionRequest updates a session's description. An empty Description
// unlocks the session and regenerates the Layer A baseline; a non-empty value
// locks the description against Layer C auto-upgrade.
type SetDescriptionRequest struct {
    ID          string `json:"id"`
    Description string `json:"description"` // no omitempty: empty string means "unlock"
}

type SetDescriptionResponse struct {
    Session session.Info `json:"session"`
}

type IDRequest struct {
    ID string `json:"id"`
}

type HookRequest struct {
    SessionID        string `json:"session_id"`
    JinSessionID     string `json:"jin_session_id,omitempty"`
    HookEventName    string `json:"hook_event_name"`
    NotificationType string `json:"notification_type,omitempty"`
    CWD              string `json:"cwd,omitempty"`
    StopReason       string `json:"stop_reason,omitempty"`
}

type SendRequest struct {
    ID     string `json:"id"`
    Prompt string `json:"prompt"`
}

// Exactly one of Option and Text carries the answer; the handler rejects a
// request that sets both or neither. Option is the choice's on-screen number,
// bounded to 1-9 because an answer is delivered as a single keystroke.
type RespondRequest struct {
    ID     string `json:"id"`
    Option int    `json:"option,omitempty"`
    Text   string `json:"text,omitempty"`
}

// Data on a successful "respond". Kind names the sort of prompt that was
// answered ("tool-permission" or "question"), so a caller learns what it
// agreed to without capturing the pane.
type RespondResponse struct {
    Kind string `json:"kind"`
}

// ResultRequest fetches structured transcript entries (text/thinking/tool_use/
// tool_result) for orchestration. It supports incremental reads (Since), output
// truncation (Last), and tool/error filtering.
//
// The entries come from the adapter that owns the session, resolved by its
// agent kind, not from a fixed reader. The handler used to call the Claude
// Code reader for every kind, so a session whose adapter cannot be read
// answered `entries: []` with `Success=true` — indistinguishable from a child
// that ran and produced nothing. It now returns Success=false when the kind is
// unknown to the registry, when its adapter has no transcript reader, or when
// the reader itself fails. Every shipped kind has a reader, so the second case
// is reserved for an adapter whose reader is not written yet; the third is
// live — the opencode reader shells out to `opencode export`, which fails when
// `opencode` is not on the daemon's PATH or the export cannot be parsed.
// `AgentSessionID == ""` is not one of those cases: it still returns
// `entries: []` with Success=true, because an agent that has not started is
// not a read failure.
type ResultRequest struct {
    ID string `json:"id"`
    // Since: ISO8601. Only entries with Timestamp strictly greater than Since are returned;
    // an entry whose Timestamp equals Since is excluded. This lets a caller pass the
    // timestamp of the last entry it has already seen to receive only what came after,
    // without duplicates. String comparison is used; every adapter is required to emit
    // lexicographically sortable RFC3339 timestamps with millisecond precision (e.g.
    // "2026-04-09T13:23:10.456Z") — see session.TranscriptSource for the full contract.
    // A timestamp is not a unique key, so an entry sharing the boundary timestamp is
    // dropped rather than repeated; see docs/gotchas.md "Session result". That applies
    // to every kind — measured 1 of 112 adjacent pairs across 14 codex rollouts,
    // 42 of 51,681 across 242 Claude Code transcripts, 12 of 478 entries across 34
    // opencode sessions. On opencode, Since is also no cheaper than a full read: the
    // reader runs `opencode export` (1.45-1.77s whatever the session size) and filters
    // afterwards.
    Since      string `json:"since,omitempty"`
    Last       int    `json:"last,omitempty"`         // Truncate to last N entries (0 = no truncation)
    Tool       string `json:"tool,omitempty"`         // Filter by tool name (matches tool_use and its tool_result)
    ErrorsOnly bool   `json:"errors_only,omitempty"`  // Keep only entries with at least one tool_result.is_error=true
}

// ResultResponse returns the filtered entry list along with session metadata.
// Truncated=true indicates that Last truncation was applied.
//
// Entries is always a JSON array, never null — a reader that found nothing
// returns nil and the no-filter path passes nil through, so ResultResponse
// carries its own MarshalJSON that re-empties it. On the type rather than at
// the handler because the CLI re-encodes this struct after decoding it, so
// both marshals have to be covered. Callers script this field with
// `jq '.entries[] | select(.type=="system")'` to tell "the child said nothing"
// from "the conversation was lost", and jq fails on null.
type ResultResponse struct {
    SessionID      string             `json:"session_id"`
    AgentSessionID string             `json:"agent_session_id,omitempty"` // adapter-side session id (Claude Code UUID etc.)
    Entries        []transcript.Entry `json:"entries"`
    Truncated      bool               `json:"truncated,omitempty"`
}

// PanePopupRequest is the request payload for the "pane-popup" action. It opens
// a tmux popup anchored to the session's pane, running Cmd in the session's
// working directory.
//
// The popup is told which jin to call back into and which session its work
// belongs to: JIN_SOCKET, JIN_BIN, JIN_DEBUG and JIN_SESSION_ID reach it as one
// tmux -e each, all four every time and empty when a value is unknown.
// jinenv.Identity.TmuxEnviron renders them and says why omitting a key is not
// the same as leaving it unset. It writes JIN_PLUGIN_DEPTH empty alongside
// them, which is not part of the identity — its doc has why a popup is told it
// continues no plugin's chain.
type PanePopupRequest struct {
    ID     string `json:"id"`
    Cmd    string `json:"cmd"`
    Title  string `json:"title,omitempty"`  // tmux 3.3+
    Width  string `json:"width,omitempty"`  // e.g. "80%"
    Height string `json:"height,omitempty"` // e.g. "80%"
}

// PaneSplitRequest splits the session's pane. Cmd is optional; an empty split
// just opens a shell in the new pane. Name enables the idempotent named-slot
// path; IfExists picks the policy when the named pane already exists
// (noop/respawn/error, empty = noop).
//
// The new pane is told the same variables PanePopupRequest names, whether
// it runs Cmd or a bare shell, and a slot restarted under IfExists=respawn is
// told them again.
//
// Breaking change: Horizontal/Percent (bool/int) are gone, replaced by
// Direction/Size below. The CLI and daemon are built together, so upgrading
// requires restarting the daemon (`jin daemon stop` then relaunch) — an old
// daemon does not understand the new fields.
type PaneSplitRequest struct {
    ID        string `json:"id"`
    Cmd       string `json:"cmd,omitempty"`
    Direction string `json:"direction,omitempty"` // down (default), up, left, right
    Size      string `json:"size,omitempty"`      // "30%" or "15"
    Full      bool   `json:"full,omitempty"`      // span the full window width/height
    NoFocus   bool   `json:"no_focus,omitempty"`  // keep focus on the current pane
    Name      string `json:"name,omitempty"`      // named-slot identifier (see FindPaneByName)
    IfExists  string `json:"if_exists,omitempty"` // noop (default), respawn or error
}

// PaneSplitResponse is the response payload for the "pane-split" action.
type PaneSplitResponse struct {
    PaneID string `json:"pane_id"`
}

// PaneCloseRequest closes the named-slot pane created by a "pane-split" call
// with the same Name in the same session.
type PaneCloseRequest struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

// PaneCaptureRequest captures the visible contents of the session's pane.
type PaneCaptureRequest struct {
    ID   string `json:"id"`
    ANSI bool   `json:"ansi,omitempty"` // include ANSI escape sequences
}

// PaneCaptureResponse is the response payload for the "pane-capture" action.
type PaneCaptureResponse struct {
    Content string `json:"content"`
}

// PaneSendKeysRequest sends keys to the session's pane. When Literal is true
// the keys are typed verbatim; otherwise they are interpreted as tmux key
// names (e.g. "Enter", "C-c").
type PaneSendKeysRequest struct {
    ID      string `json:"id"`
    Keys    string `json:"keys"`
    Literal bool   `json:"literal,omitempty"`
}

// PluginRunRequest runs one plugin on demand, bypassing matcher and debounce:
// against a session's current snapshot when SessionID is set, or as a global
// action (all session fields empty) when it is not. Depth carries the caller
// CLI's JIN_PLUGIN_DEPTH so the dispatcher can reject a plugin that tries to
// chain another plugin run. CallerTmuxSocket/CallerTmuxPane carry the invoking
// CLI's tmux context ($TMUX socket path / $TMUX_PANE), surfaced to the plugin
// as JIN_CALLER_TMUX_SOCKET/JIN_CALLER_TMUX_PANE. Success means the run was
// accepted; it executes async.
type PluginRunRequest struct {
    Plugin           string `json:"plugin"`
    SessionID        string `json:"session_id,omitempty"`
    Depth            int    `json:"depth,omitempty"`
    CallerTmuxSocket string `json:"caller_tmux_socket,omitempty"`
    CallerTmuxPane   string `json:"caller_tmux_pane,omitempty"`
}
```

`respond` was added without a ProtocolVersion bump, which is the rule above
applied rather than an exception to it: an action that never existed cannot
break an old client, because an old client never calls it. Adding a field to
`SendRequest` instead — the alternative considered — would have been a change
to an existing endpoint's Data shape, and would have needed one.

v3 is the other half of the same rule. `attention-seen` alone would not have
needed a bump, but the same change put an `attention` object on `session.Info`,
which `new` (embedded in `NewResponse`), `list`, `get` and `set-description`
all return — a change to existing endpoints' Data shape.

`attention-seen` is deliberately **not** in `readOnlyActions`: it writes a
session file, so a client that times out on it must be told the outcome is
unknown. `Manager.MarkSeen` is idempotent, so the retry that wording invites is
safe.

## Adding a New Action

1. Add a case to the `handleRequest()` switch in `server.go`
2. Define a Request type if needed
3. Implement a `handle{Action}()` method
4. Add a corresponding method in `client.go`
5. Add a CLI command in `cmd/jin/cmd/` (→ docs/adding-commands.md)
