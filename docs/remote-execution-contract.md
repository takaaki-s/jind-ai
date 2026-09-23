# Remote Execution Contract

Status: implementation in progress. Target configuration, stable endpoint
identities, bounded SSH transport, version/capability negotiation, repository
preflight, Task start/retry, structured inspection/sync, and the stdio server
are implemented. Explicit cancellation and remote-owned cleanup remain for the
next slice.

## Purpose

Remote execution must preserve jind-ai's existing separation between a durable
Task, an Execution attempt, and the interactive Session that performs it. A
remote machine is an execution backend, not a second local filesystem. The
controller therefore exchanges identities and bounded summaries with a remote
`jin`; it never treats a remote path, tmux pane, transcript, or daemon socket as
local state.

This document fixes the transport, identity, ownership, retry, and wire
boundaries needed by the first implementation. The executable fixtures under
`test/remotecontract/` protect the framing and examples; the implemented
preflight path reuses the same production framing.

## Scope

The first implementation may:

- connect to an operator-configured SSH target;
- negotiate a protocol version and named capabilities;
- create one remote Task/Execution from an in-memory prompt;
- inspect, cancel, and explicitly clean up that execution;
- reconnect and converge by stable identity and idempotency key; and
- cache only the structured summary defined below.

It must not:

- install or copy `jin`, clone a repository, or synchronize files;
- copy Git/provider credentials or enable SSH agent forwarding;
- scrape a pane, transcript, terminal title, or process output as state;
- forward a remote Unix socket or expose a TCP daemon listener;
- schedule across several hosts, choose a host automatically, or migrate an
  execution between hosts; or
- run local filesystem cleanup against any remote resource.

The operator provisions the remote `jin`, starts its daemon, and registers an
existing repository under a target-local logical repository ID. The remote
daemon uses credentials already present on that host. Missing prerequisites
are preflight failures, not provisioning requests.

## Configuration and CLI boundary

The controller and remote host keep different configuration. The controller
knows how to open SSH and translates a local repository label to an opaque
remote label; it never learns the remote path:

```yaml
remote:
  targets:
    buildbox:
      ssh_host: buildbox
      jin_path: /home/worker/.local/bin/jin
      repositories:
        jind-ai: jind-ai
```

The remote host resolves that label locally and must opt in to serving it:

```yaml
remote:
  serve:
    enabled: true
    repositories:
      jind-ai: /srv/src/jind-ai
```

The controller target revision covers normalized `ssh_host`, `jin_path`, and
the label-to-label repository map. The remote path is covered indirectly by a
repository identity digest returned by preflight; changing that mapping blocks
an existing binding until the operator starts a new Execution.

The currently implemented CLI boundary is:

```text
jin remote preflight <target> --repository <label>
jin remote serve --stdio
jin task new --target <target> --repository <label> <existing prompt flags>
jin task sync <task-selector>
```

The remaining mutation slice will add:

```text
jin task cancel <task-selector> --confirm [--idempotency-key <key>]
jin task cleanup <task-selector> --confirm [--idempotency-key <key>]
```

Target and repository labels use lowercase ASCII letters, digits, `.`, `_`,
and `-`, and begin with a letter or digit. For Task commands,
`--target` and `--repository` are required together and mutually exclusive
with local `--repo`. Existing `--workdir`, `--base`, `--agent`, `--model`,
`--fleet`, and `--no-hook` are interpreted by the remote daemon; `--workdir`
remains repository-relative. `remote serve --stdio` is a machine endpoint and
refuses a TTY. `task info` remains a local cached read; only explicit `sync`
performs network I/O. Cancel and cleanup require a remote-backed Task and print
the stable idempotency key before any ambiguous outcome can occur.

## Chosen transport

The controller starts this non-interactive SSH command with an argument vector,
never through a local shell:

```text
ssh -T -o BatchMode=yes -o ForwardAgent=no -o ClearAllForwardings=yes \
  -o PermitLocalCommand=no -o RequestTTY=no <configured-host> -- \
  <validated-jin-path> remote serve --stdio
```

`<configured-host>` is an SSH config alias. ProxyJump, identity-file, host-key,
and certificate policy remain owned by OpenSSH and the operator's SSH config.
The `jin` path is either the literal `jin` or an absolute path containing only
ASCII letters, digits, `/`, `.`, `_`, and `-`; it cannot carry arguments or
shell metacharacters. The remote account's non-interactive shell must keep
stdout quiet. Any bytes before the first valid frame are a protocol failure.

One SSH channel is a reusable, full-duplex connection with at most one request
in flight. This deliberately avoids multiplexing semantics in the first MVP.
A second short-lived channel may carry cancel or inspection if the primary
channel is blocked. Closing a channel cancels only the RPC currently using that
channel; it does not imply cancellation of an accepted Execution.

Alternatives were rejected for the MVP:

| Candidate | Decision |
| --- | --- |
| SSH stdio | Selected: authentication, encryption, host verification, and process lifetime already have one owner; no remote listener is added |
| Forward the daemon's Unix socket | Rejected: assumes a remote path and daemon lifecycle, and makes local IPC versioning an accidental public network protocol |
| Forward a TCP listener | Rejected: adds listener configuration and exposure without improving the one-controller use case |
| Copy a helper over SSH | Rejected: silently turns execution into binary provisioning and changes the trust boundary |

## Framing and negotiation

Each message is UTF-8 JSON preceded by one unsigned 32-bit big-endian byte
length. A frame larger than 1 MiB is rejected before allocation. There is no
compression. SSH stdout contains frames only; diagnostics use stderr and the
controller reads at most 32 KiB before truncating. Raw stderr is used only to
classify transport failures; it is neither displayed nor persisted in Task
state.

Every request uses this envelope:

```json
{
  "protocol": "jind-ai.remote",
  "protocol_version": 1,
  "request_id": "req-018f...",
  "controller_id": "ctl-018f...",
  "operation": "handshake",
  "payload": {}
}
```

Every response echoes `request_id` and `operation`, and contains exactly one of
an object `payload` or an `error`:

```json
{
  "protocol": "jind-ai.remote",
  "protocol_version": 1,
  "request_id": "req-018f...",
  "operation": "handshake",
  "status": "error",
  "error": {
    "code": "unsupported_version",
    "message": "no mutually supported protocol version",
    "retryable": false
  }
}
```

The first request on every connection is `handshake`. It advertises an
inclusive version range and required capability names. The server selects the
highest common version, returns its identities and limits, and fails closed if
a required capability is absent. Unknown optional capabilities are ignored.
Version 1 capability names are:

- `repository.preflight.v1`
- `execution.start.v1`
- `execution.inspect.v1`
- `execution.cancel.v1`
- `execution.cleanup.v1`
- `summary.structured.v1`

The examples in `test/remotecontract/testdata/` are the canonical version 1
payloads. Adding an optional field does not require a version bump. Removing or
reinterpreting a field, changing framing, or making a field required does.

## Identity

Four identities are intentionally separate:

| Identity | Created and owned by | Meaning |
| --- | --- | --- |
| target ID + target revision | local configuration | A stable alias plus SHA-256 of normalized connection-affecting config. Editing host, user, port, SSH alias, `jin` path, or repository map creates a new revision |
| controller ID | local jind-ai state | Persistent random UUID for this controller installation; sent on every request |
| server instance ID | remote jind-ai state | Persistent random UUID for that remote state directory; a different value blocks automatic reattachment |
| server boot ID | remote daemon process | Random UUID per daemon start; a change triggers re-inspection, not an identity failure |

OpenSSH, not jind-ai, authenticates the host. jind-ai does not parse banners or
claim a hostname is cryptographic identity. Host-key acceptance must already be
established in the operator's known-hosts policy; the implementation must not
set `StrictHostKeyChecking=no` or create a trust-on-first-use prompt in a daemon.

An Execution receives a controller execution ID before network I/O. The remote
side atomically binds `(controller_id, controller_execution_id)` and its
idempotency key to one remote Task ID and one remote Execution ID. Reusing one
of those values with different request evidence is `conflict`, never a second
execution.

Repositories are named by a configured logical `repository_id`. The remote
side resolves it to its own path and owns any worktree, branch, tmux pane, and
session it creates. Neither a repository path nor a worktree path crosses the
wire. IDs returned for those resources are opaque outside the remote server.

## Operations

After `handshake`, version 1 permits five operations:

### `repository.preflight`

Preflight is read-only. It confirms that the requested repository label is
enabled, resolves to a Git repository, and returns its default branch,
available agent kinds, and an opaque repository identity digest. The digest is
derived from the server instance, repository label, and canonical remote git
common directory, but the path itself never crosses the wire. `execution.start`
echoes the expected digest, and the server revalidates it immediately before
reservation.

### `execution.start`

The request contains bounded Task source metadata, requested base, relative
work directory, agent kind, model, fleet, hook choice, repository ID and
expected identity, idempotency key, and a prompt `{sha256, bytes, body}`.
The body is live request data: both sides validate its digest and byte count,
but neither Task record stores it. The remote server durably reserves identities
before provisioning a worktree or agent session and returns the same identities
for an identical retry.

Success means only that the remote reservation is durable. Progress is reported
through the existing Execution phases (`reserved` through `submitted`, plus
`failed` or `interrupted`) and the structured Session summary. It does not mean
the agent finished its turn.

### `execution.inspect`

Inspection is read-only and addresses both the controller execution ID and the
remote execution ID. The pair prevents accidental attachment to an ID from a
different controller. The response contains a monotonic summary sequence and
no process output.

### `execution.cancel`

Cancellation has its own idempotency key. The remote server persists a cancel
receipt before signalling its locally owned Session. Repeating the same key
returns that receipt. A different key cannot reinterpret an unknown result as a
new cancellation. Closing SSH is never cancellation.

### `execution.cleanup`

Cleanup is explicit, idempotent, and accepted only after the remote server can
prove the Session inactive. The remote side removes only resources recorded in
its own ownership journal. Local code never expands a returned value into a
path or invokes local git/tmux cleanup for a remote Execution. Provider
resources and remote refs remain outside cleanup, matching local cleanup rules.

## Controller state and disconnect recovery

The later MVP extends an Execution with `backend: local|remote` and a remote
link containing the target ID/revision, server instance ID, controller
execution ID, remote Task/Execution IDs when known, last summary sequence, and
sync state. It must not overload `SessionID` with a remote ID or put a remote
path into `Run.Repo`.

Sync state is independent of the remote Execution phase:

```text
dispatching ── accepted receipt ──> bound ── transport loss ──> unreachable
     │                                  ^                         │
     └── transport loss ────────────────┴── same-key reconcile ──┘

any state ── target revision/server identity mismatch ──> blocked
```

`unreachable` means only that current remote state is unknown. It never changes
the cached remote phase to failed, interrupted, cancelled, or complete.

| Disconnect point | Durable local fact | Required retry behaviour |
| --- | --- | --- |
| before local reservation | none | a normal new request is safe |
| after local reservation, before write | controller execution ID + idempotency key | reconnect and send the identical start |
| during request write/read | same as above; remote acceptance is unknown | reconnect and send the identical start; remote atomic reservation deduplicates it |
| after accepted response, before local bind save | dispatching record still has the same key | identical start returns the original remote IDs |
| after local bind | both identity pairs | handshake, verify server instance, then inspect |
| during cancel or cleanup | persisted operation key; outcome unknown | repeat that operation with the same key and reconcile its receipt |
| remote daemon restart | stable server instance, new boot ID | inspect; remote transient phases may report `interrupted` |
| server instance or target revision changed | old binding only | enter `blocked`; never adopt or recreate automatically |
| bound execution returns not-found | binding proves it once existed | enter `blocked`; absence is remote data loss, not permission to duplicate |

No automatic retry may repeat prompt submission when the remote journal says
`submitting`, `submitted`, or `interrupted` from submitting. This is the same
uncertain-delivery rule used by local `jin task new`.

Cancel and cleanup responses carry a receipt with the operation idempotency key
and `succeeded`, `failed`, or `unknown`. Envelope status `ok` means the RPC was
understood; it does not rewrite an `unknown` operation receipt into success.

## Structured summary

The remote server is authoritative for its Task/Execution/Session. The
controller may cache only this bounded projection:

- summary sequence and observation time;
- remote Execution phase, failed phase, and bounded safe error/guidance codes;
- opaque remote Session ID, status, completion attention counters;
- aggregate review facts (commits, branch, fingerprint, counts); and
- aggregate reported-check status bound to that fingerprint.

The projection deliberately excludes prompts, messages, transcripts, patches,
pane text, terminal escape sequences, environment variables, argv, absolute
paths, process lists, plugin/provider responses, and credentials. A future
request for richer evidence needs a new named capability and bounded schema; it
must not fall back to screen scraping.

Summary sequence is monotonic per remote Execution. The controller ignores a
lower sequence after reconnect. Equal sequence with different JSON is a
protocol violation. Timestamps are display data and never order updates.

## Security and failure boundaries

- SSH host authentication and user authentication remain OpenSSH concerns.
- Batch mode is mandatory. The daemon cannot display password, key-passphrase,
  host-key, or MFA prompts.
- Agent forwarding, local/remote/dynamic forwarding, PTY allocation, and local
  SSH commands are disabled on the command line.
- The protocol carries no SSH key, Git credential, provider token, cookie,
  environment snapshot, or credential path. Remote Git/provider access uses
  remote-host configuration.
- Prompt bodies cross the authenticated channel only for `execution.start` and
  are never logged or persisted by the transport. Logs may include digest and
  byte count.
- Frame, prompt, identifier, list, diagnostic, and timeout bounds are checked
  before use. A malformed or oversized response closes the channel and leaves
  the operation outcome unknown.
- Error messages are bounded and safe for display. Durable state records stable
  error codes rather than raw SSH stderr or remote process output.
- Server instance mismatch, capability loss, protocol mismatch, and repository
  mapping mismatch all fail closed before mutation.

## Measurement harness

`make test-remote-contract` starts an isolated localhost `sshd` on a random
loopback port with temporary host/client keys. It measures and emits JSON for:

1. first SSH command startup;
2. a second independent connection (the reconnect control);
3. stdout/stderr separation and preservation of a remote non-zero exit; and
4. bounded cancellation of a remote command when the local context is closed.

The harness uses no user SSH keys, agent, known-hosts file, or running jind-ai
daemon. Default `make test` validates the wire fixtures and skips the socket
measurement. The opt-in target may be unavailable in containers that cannot
listen on loopback or start `sshd`; that is an environment limitation, not a
reason to weaken production SSH policy.

The harness answers transport questions only. It does not claim that a shell
banner is safe: production framing rejects any stdout contamination. It also
does not benchmark a real network; timeout defaults must be configurable and
tested with deterministic delayed peers.

A reference run on 2026-09-22 with OpenSSH 9.6p1 measured 215 ms for the first
command, 217 ms for an independent reconnect, and 215 ms for a command that
preserved remote exit status 42 with clean stdout/stderr separation. Cancelling
a 30-second remote command converged locally in 502 ms under a 500 ms context
deadline. These numbers are evidence for the state model, not production
defaults or performance guarantees.

## Implementation slices and acceptance fixtures

The MVP is limited to:

1. target configuration with immutable revision calculation and logical
   repository mappings;
2. persistent controller/server identities;
3. a bounded SSH process runner with the exact safety options above;
4. version 1 framing, handshake, capability checks, and the five versioned
   operations;
5. remote-link persistence and the sync/retry table above;
6. a remote stdio server that delegates mutations to its local daemon; and
7. CLI preflight plus explicit start, inspect, cancel, and cleanup.

Items 1-3, start/inspect and structured-summary portions of item 4, the durable
remote link and retry/sync behavior in item 5, and their stdio/CLI paths in
items 6-7 are implemented. The production server advertises repository
preflight, execution start/inspect, and structured summary capabilities.
Cancel/cleanup capabilities are intentionally not advertised until their
receipt journals and ownership checks are implemented.

Its tests must reuse the JSON examples and add deterministic peers for: partial
frame reads, oversized lengths, malformed JSON, stderr floods, timeout before
write, disconnect after request, disconnect after remote reservation, duplicate
start, conflicting duplicate, stale summary, identity mismatch, missing
capability, daemon restart, unknown cancel, and unknown cleanup. At least one
end-to-end test must use an isolated real `sshd`; tmux pane output must never be
an assertion source.
