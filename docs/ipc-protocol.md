# IPC Protocol

## Transport

- Unix domain socket: `$XDG_RUNTIME_DIR/jindaiko/daemon.sock` (fallback `$TMPDIR/jindaiko-<uid>/daemon.sock`)
- One request / one response per connection (no connection pooling)
- JSON encoding/decoding

## Message Format

```go
// Request (client → server)
type Request struct {
    Action string          `json:"action"`
    Data   json.RawMessage `json:"data,omitempty"`
}

// Response (server → client)
type Response struct {
    Success bool            `json:"success"`
    Data    json.RawMessage `json:"data,omitempty"`
    Error   string          `json:"error,omitempty"`
}
```

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
| `notification-history` | (none) | Get notification history |
| `result` | `ResultRequest` | Fetch structured transcript entries (orchestration) |
| `set-description` | `SetDescriptionRequest` | Update session description (empty resets to auto-generated) |
| `agent-signal` | `AgentSignalRequest` | Deliver an out-of-band status signal from an agent adapter (currently only `kind="hook"` is wired) |

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
```

## Adding a New Action

1. Add a case to the `handleRequest()` switch in `server.go`
2. Define a Request type if needed
3. Implement a `handle{Action}()` method
4. Add a corresponding method in `client.go`
5. Add a CLI command in `cmd/jin/cmd/` (→ docs/adding-commands.md)
