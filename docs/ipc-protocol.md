# IPC Protocol

## Transport

- Unix domain socket: `$XDG_RUNTIME_DIR/jind-ai/daemon.sock` (fallback `$TMPDIR/jind-ai-<uid>/daemon.sock`)
- One request / one response per connection (no connection pooling)
- JSON encoding/decoding

## Timeouts

`Client` bounds every exchange so a daemon that accepts a connection and then
stops responding cannot hang the caller forever. The bounds are tiered, not
per-action:

| Bound | Value | Applies to |
|---|---|---|
| dial | 2s | every request |
| write | 5s | every request |
| response wait | 60s | every action without its own entry below |
| `hook` | 10s | the agent-facing path — a stalled hook blocks the agent process itself |
| `stop` | 5s | the remedy for a wedged daemon, so it must not inherit the wedged-daemon bound |
| `new` | none | the handler chains unbounded git subprocesses and then the post-create hook; only the hook is capped (`worktree.hook_timeout`, default 300s) |
| `delete` | none | the handler runs `git worktree remove` synchronously, and neither side bounds it — how long an `rm -rf` of a checkout takes is a property of the user's disk |
| `pane-popup` | none | the handler runs `tmux display-popup -E`, which blocks for the popup's user-controlled lifetime |

**The client bounds an exchange only when it can name a duration that is
certainly longer than any legitimate handler run.** `new`, `delete` and
`pane-popup` have no such value — one is capped by a user-editable config key
the client cannot see, one by an external process over a checkout of unknown
size, the last by when the user closes a popup — so they defer to the bound the
handler already owns. Guessing on their behalf would not protect anyone: since
a timeout is not a cancellation (below), a bound that fires early just reports
an unknown outcome for work that goes on to succeed.

Dial and write stay bounded for every action, including those three. The write
bound is a single constant rather than a per-action one because what it guards
does not vary: a request is one small JSON value, and the daemon decodes each
accepted connection on its own goroutine, so writing never waits on handler
work. A blocked write means the daemon stopped reading, and it is reported that
way rather than as a failure to respond.

The 60s response wait is deliberately generous. With `new`, `delete` and
`pane-popup` out of its scope — and `hook` and `stop` on bounds of their own —
what it covers is tmux subprocess calls and local file reads, plus one handler
with a named cost: `send` waits up to 5s for the prompt to appear in the pane
before giving up. Those handlers queue behind the manager lock, so
60s is sized to clear a backlog of them; hitting it should mean "the daemon is
wedged", not "this machine is loaded".

**A timeout is not a cancellation.** The protocol has no cancel channel, so a
client that gives up does not stop the daemon — a mutating action such as
`new` or `delete` may still complete. Error messages for those actions
therefore report an unknown outcome rather than a failure, and point at
`jin daemon restart` instead of encouraging a blind retry; read-only actions
(`readOnlyActions` in `server.go`) get a plain timeout message instead, so the
warning keeps its weight where it matters. Any new action that mutates state
inherits this property by default; make it idempotent, or expect callers to
check state after a timeout.

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
| `new` | `NewRequest` | Create session |
| `list` | (none) | List all sessions |
| `get` | `IDRequest` | Get a single session (with last-message enrichment) |
| `send` | `SendRequest` | Send a prompt to a session (alias `prompt` on the CLI) |
| `start` | `IDRequest` | Start session |
| `kill` | `IDRequest` | Kill session |
| `delete` | `DeleteRequest` | Delete session (optionally with worktree) |
| `stop` | (none) | Stop daemon |
| `hook` | `HookRequest` | Claude Code hook event |
| `result` | `ResultRequest` | Fetch structured transcript entries (orchestration) |
| `set-description` | `SetDescriptionRequest` | Update session description (empty resets to auto-generated) |
| `agent-signal` | `AgentSignalRequest` | Deliver an out-of-band status signal from an agent adapter (currently only `kind="hook"` is wired) |
| `pane-popup` | `PanePopupRequest` | Open a tmux popup over a session's pane, running a command |
| `pane-split` | `PaneSplitRequest` | Split a session's pane, optionally running a command in the new pane (→ `PaneSplitResponse`) |
| `pane-close` | `PaneCloseRequest` | Close a named-slot pane created by `pane-split` with `name` |
| `pane-capture` | `PaneCaptureRequest` | Capture the visible contents of a session's pane |
| `pane-send-keys` | `PaneSendKeysRequest` | Send keys to a session's pane (literal text or tmux key names) |
| `plugin-run` | `PluginRunRequest` | Run a plugin on demand for a session (bypasses matcher/debounce; async) |

## Request Types

```go
type NewRequest struct {
    Description string `json:"description"`
    WorkDir     string `json:"work_dir"`
    Start       bool   `json:"start"`
    Fleet       string `json:"fleet"`                     // Fleet name for session grouping
    AgentKind   string `json:"agent_kind,omitempty"`      // Adapter kind ("claude" etc.); daemon defaults from config's default_agent when empty

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

// ResultRequest fetches structured transcript entries (text/thinking/tool_use/
// tool_result) for orchestration. It supports incremental reads (Since), output
// truncation (Last), and tool/error filtering.
type ResultRequest struct {
    ID string `json:"id"`
    // Since: ISO8601. Only entries with Timestamp strictly greater than Since are returned;
    // an entry whose Timestamp equals Since is excluded. This lets a caller pass the
    // timestamp of the last entry it has already seen to receive only what came after,
    // without duplicates. String comparison is used (Claude Code emits lexicographically
    // sortable RFC3339 timestamps with millisecond precision, e.g. "2026-04-09T13:23:10.456Z").
    Since      string `json:"since,omitempty"`
    Last       int    `json:"last,omitempty"`         // Truncate to last N entries (0 = no truncation)
    Tool       string `json:"tool,omitempty"`         // Filter by tool name (matches tool_use and its tool_result)
    ErrorsOnly bool   `json:"errors_only,omitempty"`  // Keep only entries with at least one tool_result.is_error=true
}

// ResultResponse returns the filtered entry list along with session metadata.
// Truncated=true indicates that Last truncation was applied.
type ResultResponse struct {
    SessionID      string             `json:"session_id"`
    AgentSessionID string             `json:"agent_session_id,omitempty"` // adapter-side session id (Claude Code UUID etc.)
    Entries        []transcript.Entry `json:"entries"`
    Truncated      bool               `json:"truncated,omitempty"`
}

// PanePopupRequest opens a tmux popup anchored to the session's pane, running
// Cmd in the session's working directory. The popup process does not inherit
// JIN_* environment variables.
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

## Adding a New Action

1. Add a case to the `handleRequest()` switch in `server.go`
2. Define a Request type if needed
3. Implement a `handle{Action}()` method
4. Add a corresponding method in `client.go`
5. Add a CLI command in `cmd/jin/cmd/` (→ docs/adding-commands.md)
