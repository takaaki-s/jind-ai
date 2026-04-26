# IPC Protocol

## Transport

- Unix domain socket: `~/.ccvalet/run/daemon.sock`
- One request / one response per connection (no connection pooling)
- JSON encoding/decoding

## Message Format

```go
// Request (client → server)
type Request struct {
    Action  string          `json:"action"`
    Data    json.RawMessage `json:"data,omitempty"`
    Visited []string        `json:"visited,omitempty"` // Host IDs already visited (loop prevention)
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
| `list-hosts` | (none) | List hosts |
| `hook` | `HookRequest` | Claude Code hook event |
| `notification-history` | (none) | Get notification history |
| `result` | `ResultRequest` | Fetch structured transcript entries (orchestration) |

## Request Types

```go
type NewRequest struct {
    Name        string `json:"name"`
    WorkDir     string `json:"work_dir"`
    Start       bool   `json:"start"`
    HostID      string `json:"host_id,omitempty"`
    SSHAuthSock string `json:"ssh_auth_sock,omitempty"`
}

type IDRequest struct {
    ID     string `json:"id"`
    HostID string `json:"host_id,omitempty"`
}

type HookRequest struct {
    SessionID        string `json:"session_id"`
    CcvaletSessionID string `json:"ccvalet_session_id,omitempty"`
    HookEventName    string `json:"hook_event_name"`
    NotificationType string `json:"notification_type,omitempty"`
    CWD              string `json:"cwd,omitempty"`
    StopReason       string `json:"stop_reason,omitempty"`
}

type SendRequest struct {
    ID     string `json:"id"`
    Prompt string `json:"prompt"`
    HostID string `json:"host_id,omitempty"`
}

// ResultRequest fetches structured transcript entries (text/thinking/tool_use/
// tool_result) for orchestration. It supports incremental reads (Since), output
// truncation (Last), and tool/error filtering.
type ResultRequest struct {
    ID     string `json:"id"`
    HostID string `json:"host_id,omitempty"`
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

// ResultResponse returns the filtered entry list along with session/host metadata.
// Truncated=true indicates that Last truncation was applied.
type ResultResponse struct {
    SessionID       string             `json:"session_id"`
    HostID          string             `json:"host_id"`
    ClaudeSessionID string             `json:"claude_session_id,omitempty"`
    Entries         []transcript.Entry `json:"entries"`
    Truncated       bool               `json:"truncated,omitempty"`
}
```

## Bidirectional Routing

Daemons use visited-based routing to support bidirectional communication between hosts.

When `host_id` is not "local" and not the current daemon's own host ID, the request is forwarded to the target host. Before forwarding, the current daemon's `hostID` is appended to `req.Visited`. If the target `hostID` is already in `Visited`, a "routing loop detected" error is returned.

For aggregation actions (`list`, `notification-history`), the daemon queries all reachable hosts (configured remotes + peers registered via reverse tunnel), skipping any host already in `Visited`.

### Peer Registration

A peer is a daemon connected via SSH reverse tunnel (`-R`). When the master starts a slave, it passes `--peer-socket` and `--peer-id` flags. The slave uses these to register the master as a peer, enabling bidirectional communication.

### Host ID

Each daemon has a `hostID` (flag `--host-id` > config `host_id` > default `"local"`). This ID is used in `Visited` arrays and for tagging sessions/notifications.

## Adding a New Action

1. Add a case to the `handleRequest()` switch in `server.go`
2. Define a Request type if needed
3. Implement a `handle{Action}()` method
4. Add a corresponding method in `client.go`
5. Add a CLI command in `cmd/ccvalet/cmd/` (→ docs/adding-commands.md)
