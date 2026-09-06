package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/session"
)

// The client sets a bound only when it can name one that is certainly longer
// than any legitimate handler run. Where that is unknowable from here — a
// popup's user-controlled lifetime — it passes 0 to sendWithTimeout and defers
// to the bound the handler already owns. A timeout is not a cancellation, so a
// bound we cannot justify only reports "outcome unknown" for work that went on
// to succeed.
const (
	// dialTimeout bounds connecting to the daemon socket. A local unix socket
	// either connects instantly or not at all, so anything slower means the
	// daemon is not accepting; failing fast beats blocking the caller forever.
	dialTimeout = 2 * time.Second

	// requestWriteTimeout bounds writing the request, for every action alike.
	// The bound does not vary because what it guards does not vary: a request is
	// one small JSON value, and the daemon hands each accepted connection to its
	// own goroutine that decodes immediately, so the write never waits on handler
	// work. A write that blocks for seconds means the daemon stopped reading.
	requestWriteTimeout = 5 * time.Second

	// defaultRequestTimeout bounds the wait for a response on every action that
	// does not name its own bound. Only "pane-popup" (user-controlled lifetime)
	// is out of scope; "hook" and "stop" take the tighter bounds below. "new" and
	// "delete" default here because their handlers now acknowledge as soon as the
	// synchronous pre-checks pass and hand the rest to a goroutine (see "Async
	// completion" in docs/ipc-protocol.md).
	//
	// Two handlers have a named cost. "result" runs `opencode export`, bounded by
	// that adapter's exportTimeout, which is chosen to sit inside this one — if
	// either moves they have to keep that order, or the client gives up on a read
	// the daemon is still doing. "send" retries for up to
	// session.sendVerifyBudget, checked between attempts, so its real ceiling is
	// roughly that budget plus one full attempt.
	//
	// At the measured ~33ms per tmux invocation (see "Session send" in
	// docs/gotchas.md) a 30KB prompt lands near 38s. Past roughly 200KB the chunk
	// count pushes the ceiling over 60s and the client would report a timeout
	// while the daemon may yet press Enter; if prompts that large become real,
	// "send" needs its own prompt-derived bound rather than a bigger constant.
	defaultRequestTimeout = 60 * time.Second

	// hookRequestTimeout bounds the agent-facing hook path. The trade cuts both
	// ways: a stalled hook blocks the agent process itself, but an overrun is
	// worse than an ordinary failure, because cmd/jin/cmd/hook.go only logs it
	// and exits 0 — the status update is dropped with nothing shown, and the
	// session looks frozen in the TUI. 10s sits well clear of the handler's real
	// cost while still capping what a wedged daemon can cost the agent.
	//
	// That is the effective bound for claude and codex only. On opencode it stays
	// 3s, because the plugin SIGKILLs the `jin hook` child at HOOK_TIMEOUT_MS
	// first. The asymmetry is intended: the plugin's kill routes through
	// done(false), which re-sends on the next event, whereas claude and codex log
	// and exit 0 and lose the update.
	//
	// The "agent-signal" action is the same path and has no client method yet.
	// One added later must pass this bound explicitly — reaching for send() would
	// inherit defaultRequestTimeout and leave the agent blocked for a minute.
	hookRequestTimeout = 10 * time.Second

	// stopRequestTimeout bounds the stop request. Stopping is the remedy this
	// package points users at when a request times out, so it must stay
	// responsive against exactly the wedged daemon it is meant to clear.
	// handleStop replies before it does any work, so a daemon healthy enough to
	// answer answers quickly; Stop confirms through IsRunning either way.
	stopRequestTimeout = 5 * time.Second

	// stopPollAttempts and stopPollInterval bound how long Stop waits for the
	// daemon to actually go away once the request has been sent — sent, not
	// acknowledged, since the poll runs whether or not an answer came back.
	// handleStop replies before shutting down, so an acknowledgement only means
	// "accepted"; this poll is the only thing that turns it into "stopped".
	stopPollAttempts = 30
	stopPollInterval = 100 * time.Millisecond
)

// dialDaemon is the package's one door to the socket. It is a var so that
// tests can record the dial timeout and wrap the returned conn to observe which
// deadlines the client set — "no read deadline at all" is not something waiting
// can demonstrate. Swapping a package-level var means those tests stay serial.
//
// Deliberately not the interface seam the repo reaches for elsewhere: Client
// carries no other injected dependency, so a constructor parameter would add an
// injection point whose only user is the test binary.
var dialDaemon = net.DialTimeout

// Client is the daemon client
type Client struct {
	socketPath string
}

// NewClient creates a new daemon client
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

// IsRunning checks if the daemon is running.
//
// A true result only means the socket accepted the connection — a wedged
// daemon still accepts, so this is not a liveness check.
func (c *Client) IsRunning() bool {
	conn, err := dialDaemon("unix", c.socketPath, dialTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (c *Client) send(req Request) (*Response, error) {
	return c.sendWithTimeout(req, defaultRequestTimeout)
}

// sendWithTimeout performs one request/response exchange. The timeout bounds
// the wait for a response only, and a timeout of 0 waives that bound; dial and
// write keep their own fixed bounds either way. Each deadline is set once,
// before writing, because every response is a single JSON value read in one
// Decode.
func (c *Client) sendWithTimeout(req Request, timeout time.Duration) (*Response, error) {
	conn, err := dialDaemon("unix", c.socketPath, dialTimeout)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, fmt.Errorf(
				"daemon is not accepting connections (timed out after %s) — try: jin daemon restart",
				dialTimeout,
			)
		}
		// The docs pointer is here and nowhere else among the exit paths: a
		// stopped daemon is the failure an orchestrating agent hits first, and
		// the context injected into every child session points at `jin docs`,
		// so this is where the two have to meet.
		return nil, fmt.Errorf("daemon not running. Start with: jin daemon start (details: jin docs show gotchas)")
	}
	defer conn.Close()

	_ = conn.SetWriteDeadline(time.Now().Add(requestWriteTimeout))
	if timeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
	}

	req.ProtocolVersion = ProtocolVersion
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(req); err != nil {
		return nil, wrapDeadline(err, req.Action, fmt.Sprintf(
			"daemon stopped reading the request within %s", requestWriteTimeout,
		))
	}

	decoder := json.NewDecoder(conn)
	var resp Response
	if err := decoder.Decode(&resp); err != nil {
		// "within 0s" is built here but never reaches the caller: timeout == 0
		// skipped the read deadline above, so Decode has none to blow and
		// wrapDeadline returns the error unwrapped. Put a bound on the read path
		// unconditionally and this string starts escaping.
		return nil, wrapDeadline(err, req.Action, fmt.Sprintf(
			"daemon did not respond within %s", timeout,
		))
	}

	if resp.ProtocolVersion != ProtocolVersion {
		// Old daemon (pre-versioning) sends no protocol_version and it
		// deserializes to 0 — treat that the same as any explicit mismatch.
		// The whole point of the check is to fail loudly here instead of
		// letting individual endpoints error with confusing symptoms like
		// "unexpected end of JSON input".
		return nil, fmt.Errorf(
			"daemon protocol version %d does not match client %d — run 'jin daemon restart' after updating jin",
			resp.ProtocolVersion, ProtocolVersion,
		)
	}

	return &resp, nil
}

// wrapDeadline turns a deadline overrun into a message that distinguishes "the
// daemon is stuck" from "the daemon is gone". stalled says which half of the
// exchange ran out of time and after how long; an overrun while writing means
// the daemon stopped reading, not that it failed to answer.
//
// The protocol has no cancel channel, so giving up here does not stop the
// daemon: a mutating action such as new or delete may well have completed.
// Those get an unknown-outcome wording rather than a failure, so callers are
// not nudged into blindly repeating them. That holds on the write side too —
// Encode issues the value and its newline as one Write, and the daemon's
// json.Decoder is satisfied by the closing brace, so a write that timed out may
// nonetheless have delivered a complete request. Read-only actions get the
// plain message: spending the warning where nothing is at stake costs its
// credibility.
func wrapDeadline(err error, action, stalled string) error {
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	if readOnlyActions[action] {
		return fmt.Errorf("%s — try: jin daemon restart (%w)", stalled, os.ErrDeadlineExceeded)
	}
	return fmt.Errorf(
		"%s — the request may still be running there, so its outcome is unknown; check the current state before repeating it, or run: jin daemon restart (%w)",
		stalled, os.ErrDeadlineExceeded,
	)
}

// NewOptions contains options for creating a new session
type NewOptions struct {
	Description string
	WorkDir     string
	Start       bool
	Fleet       string // Fleet name for session grouping
	AgentKind   string // Adapter identifier; daemon defaults from config when empty
	Model       string // Agent model in the CLI's own spelling; empty = the agent's own default

	Worktree       bool   // Create a git worktree for this session
	WorktreeName   string // Override auto-generated worktree name
	WorktreeBranch string // Override auto-generated branch name
	WorktreeBase   string // Override auto-detected base branch
	NoHook         bool   // Skip .jin/worktree-post-create.sh hook
}

// New creates a new session. Any non-fatal creation warning is discarded;
// callers that want to surface it should use NewWithOptions instead.
func (c *Client) New(description, workDir string, start bool) (*session.Info, error) {
	info, _, err := c.NewWithOptions(NewOptions{
		Description: description,
		WorkDir:     workDir,
		Start:       start,
	})
	return info, err
}

// NewWithOptions creates a new session with full options. The second return
// value is a non-fatal warning message (empty when there is nothing to
// surface) — see NewResponse. It is only attached to the create response,
// never to subsequent Get/List calls.
func (c *Client) NewWithOptions(opts NewOptions) (*session.Info, string, error) {
	// NewRequest and NewOptions share a field layout by design (see server.go).
	// The conversion keeps them in lockstep without an error-prone field-by-field
	// copy; NewRequest's JSON tags apply on Marshal regardless.
	data, _ := json.Marshal(NewRequest(opts))

	// defaultRequestTimeout applies here (via send): handleNew only registers the
	// session record and returns a StatusCreating reservation before this call
	// gets its response. Callers that need to know when provisioning actually
	// finishes poll Get for the Status transition off StatusCreating.
	resp, err := c.send(Request{Action: "new", Data: data})
	if err != nil {
		return nil, "", err
	}
	if !resp.Success {
		return nil, "", errors.New(resp.Error)
	}

	var out NewResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return nil, "", err
	}
	info := out.Info
	return &info, out.Warning, nil
}

// Get retrieves a single session by ID
func (c *Client) Get(id string) (*session.Info, error) {
	data, _ := json.Marshal(IDRequest{ID: id})

	resp, err := c.send(Request{Action: "get", Data: data})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, errors.New(resp.Error)
	}

	var info session.Info
	if err := json.Unmarshal(resp.Data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ErrRespondNotCleared reports that a prompt was still on screen after its
// answer was sent. The CLI maps it to the timeout exit code, which the README
// and the embedded exit-codes doc both promise for this case.
var ErrRespondNotCleared = errors.New("the prompt did not clear")

// Respond answers a prompt the session's agent is blocked on and returns the
// sort of prompt that was answered. Exactly one of option and text carries
// the answer; the daemon rejects a call that sets both or neither.
func (c *Client) Respond(id string, option int, text string) (string, error) {
	data, _ := json.Marshal(RespondRequest{ID: id, Option: option, Text: text})
	resp, err := c.send(Request{Action: "respond", Data: data})
	if err != nil {
		return "", err
	}
	if !resp.Success {
		// Restore the classification the daemon tagged on, and hand the caller
		// a message without it. The prefix exists only to survive the wire.
		if msg, ok := strings.CutPrefix(resp.Error, RespondNotClearedPrefix); ok {
			return "", fmt.Errorf("%w: %s", ErrRespondNotCleared, msg)
		}
		return "", errors.New(resp.Error)
	}
	var out RespondResponse
	// A daemon too old to send the payload still answered the call, and the
	// answer landing is what the caller needs. Report the empty kind rather
	// than turning a successful answer into an error.
	if len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			return "", err
		}
	}
	return out.Kind, nil
}

// Send sends a prompt to a session
func (c *Client) Send(id, prompt string) error {
	data, _ := json.Marshal(SendRequest{ID: id, Prompt: prompt})
	resp, err := c.send(Request{Action: "send", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// Result fetches structured transcript entries for a session, with optional
// since/last/tool/errors-only filters. Used by orchestration tools to inspect
// what a session actually did (tool_use / tool_result), not just the assistant text.
func (c *Client) Result(req ResultRequest) (*ResultResponse, error) {
	data, _ := json.Marshal(req)
	resp, err := c.send(Request{Action: "result", Data: data})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, errors.New(resp.Error)
	}
	var out ResultResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// List lists all sessions
func (c *Client) List() ([]session.Info, error) {
	resp, err := c.send(Request{Action: "list"})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, errors.New(resp.Error)
	}

	var sessions []session.Info
	if err := json.Unmarshal(resp.Data, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// Start starts a session
func (c *Client) Start(id string) error {
	data, _ := json.Marshal(IDRequest{ID: id})
	resp, err := c.send(Request{Action: "start", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// Kill kills a session
func (c *Client) Kill(id string) error {
	data, _ := json.Marshal(IDRequest{ID: id})
	resp, err := c.send(Request{Action: "kill", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// Delete deletes a session. If removeWorktree is true, the session's git worktree
// will also be removed. If the worktree has uncommitted changes and forceRemoveWorktree
// is false, an error is returned.
func (c *Client) Delete(id string, removeWorktree, forceRemoveWorktree bool) error {
	data, _ := json.Marshal(DeleteRequest{ID: id, RemoveWorktree: removeWorktree, ForceRemoveWorktree: forceRemoveWorktree})
	// defaultRequestTimeout applies here too: handleDelete only runs the
	// synchronous pre-checks before this call gets its response. The worktree
	// removal — an rm -rf of a whole checkout — and the tmux teardown run in a
	// goroutine afterwards, so the session moves to StatusDeleting and disappears
	// from Get/List once that finishes.
	resp, err := c.send(Request{Action: "delete", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		if strings.Contains(resp.Error, session.ErrWorktreeDirty.Error()) {
			return session.ErrWorktreeDirty
		}
		if strings.Contains(resp.Error, session.ErrNotWorktree.Error()) {
			return session.ErrNotWorktree
		}
		return errors.New(resp.Error)
	}
	return nil
}

// SetDescription updates a session's description. An empty description unlocks
// the session and regenerates the Layer A baseline; a non-empty description
// locks it (Layer B manual override).
func (c *Client) SetDescription(id, description string) error {
	data, _ := json.Marshal(SetDescriptionRequest{ID: id, Description: description})
	resp, err := c.send(Request{Action: "set-description", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// MarkSeen acknowledges a session's completion receipt and returns the
// resulting Info. See session.Manager.MarkSeen.
func (c *Client) MarkSeen(id string) (*session.Info, error) {
	data, _ := json.Marshal(IDRequest{ID: id})
	resp, err := c.send(Request{Action: "attention-seen", Data: data})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, errors.New(resp.Error)
	}

	var info session.Info
	if err := json.Unmarshal(resp.Data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Stop stops the daemon and waits for it to actually exit.
//
// A protocol-mismatched daemon still executes the stop action — its handler
// runs before the client-side mismatch is noticed on the response — so the send
// error is swallowed when a subsequent IsRunning() poll confirms the daemon did
// shut down.
func (c *Client) Stop() error {
	return c.stop(stopPollAttempts, stopPollInterval)
}

// stop is Stop with the shutdown poll spelled out, so a test can reach the
// exhausted-poll path without sitting through three real seconds. Only Stop
// calls it, and only with the constants above.
func (c *Client) stop(attempts int, interval time.Duration) error {
	_, sendErr := c.sendWithTimeout(Request{Action: "stop"}, stopRequestTimeout)
	for range attempts {
		if !c.IsRunning() {
			return nil
		}
		time.Sleep(interval)
	}
	// Past the poll the daemon is still accepting connections, which is grounds
	// enough for the remedy below regardless of what sendErr was: a blown
	// deadline, a dial timeout, or even nil because the request went through and
	// the daemon simply has not exited yet. The predicate lives here rather than
	// in sendErr's type so every "still running" outcome gets the same answer.
	//
	// `jin daemon restart` stops through this very function, so naming it here
	// would answer a failed stop with the same stop. A daemon that ignored the
	// request needs a signal, not another request; pkill is offered as an example
	// rather than the instruction, since the pattern also matches a daemon on
	// another --socket. The start half is offered conditionally because restart
	// and stop want opposite things once the kill is done.
	msg := fmt.Sprintf(
		"daemon is still accepting connections %s after the stop request — kill it manually (e.g. pkill -f 'jin daemon'); if you were restarting, start the new one with: jin daemon start",
		time.Duration(attempts)*interval,
	)
	if errors.Is(sendErr, os.ErrDeadlineExceeded) {
		return fmt.Errorf("%s (%w)", msg, os.ErrDeadlineExceeded)
	}
	return errors.New(msg)
}

// SendHook sends a Claude Code hook event to the daemon
func (c *Client) SendHook(req HookRequest) error {
	data, _ := json.Marshal(req)
	resp, err := c.sendWithTimeout(Request{Action: "hook", Data: data}, hookRequestTimeout)
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// DirHistory retrieves directory usage history
func (c *Client) DirHistory(maxEntries int) ([]config.DirHistoryEntry, error) {
	data, _ := json.Marshal(struct {
		MaxEntries int `json:"max_entries"`
	}{MaxEntries: maxEntries})

	resp, err := c.send(Request{Action: "dir-history", Data: data})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, errors.New(resp.Error)
	}

	var entries []config.DirHistoryEntry
	if err := json.Unmarshal(resp.Data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// RemoveDirHistory removes a directory history entry
func (c *Client) RemoveDirHistory(path string) error {
	data, _ := json.Marshal(struct {
		Path string `json:"path"`
	}{Path: path})

	resp, err := c.send(Request{Action: "remove-dir-history", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// PanePopup opens a tmux popup running cmd for the session, anchored to its
// pane and started in the session's working directory.
func (c *Client) PanePopup(id, cmd, title, width, height string) error {
	data, _ := json.Marshal(PanePopupRequest{ID: id, Cmd: cmd, Title: title, Width: width, Height: height})
	// No read deadline: the handler runs `tmux display-popup -E`, which blocks
	// for the popup's entire lifetime. That lifetime ends when the user closes
	// the popup, so there is no bound we could pick that would not eventually
	// kill a legitimately open popup.
	resp, err := c.sendWithTimeout(Request{Action: "pane-popup", Data: data}, 0)
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// PaneSplit splits the session's pane per req and returns the new pane's ID —
// or, for a named slot that already exists, the reused pane's ID. An empty
// req.Cmd just opens a shell in the new pane.
func (c *Client) PaneSplit(req PaneSplitRequest) (string, error) {
	data, _ := json.Marshal(req)
	resp, err := c.send(Request{Action: "pane-split", Data: data})
	if err != nil {
		return "", err
	}
	if !resp.Success {
		return "", errors.New(resp.Error)
	}
	var out PaneSplitResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return "", err
	}
	return out.PaneID, nil
}

// PaneClose kills the named-slot pane created by PaneSplit with a name.
func (c *Client) PaneClose(id, name string) error {
	data, _ := json.Marshal(PaneCloseRequest{ID: id, Name: name})
	resp, err := c.send(Request{Action: "pane-close", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// PaneCapture returns the visible contents of the session's pane.
func (c *Client) PaneCapture(id string, ansi bool) (string, error) {
	data, _ := json.Marshal(PaneCaptureRequest{ID: id, ANSI: ansi})
	resp, err := c.send(Request{Action: "pane-capture", Data: data})
	if err != nil {
		return "", err
	}
	if !resp.Success {
		return "", errors.New(resp.Error)
	}
	var out PaneCaptureResponse
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return "", err
	}
	return out.Content, nil
}

// PaneSendKeys sends keys to the session's pane. When literal is true the keys
// are typed verbatim; otherwise they are interpreted as tmux key names.
func (c *Client) PaneSendKeys(id, keys string, literal bool) error {
	data, _ := json.Marshal(PaneSendKeysRequest{ID: id, Keys: keys, Literal: literal})
	resp, err := c.send(Request{Action: "pane-send-keys", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// PluginRun runs a plugin on demand, bypassing matcher and debounce: against a
// session's current snapshot when req.SessionID is set, or as a global action
// when it is empty. It returns once the run is accepted; the plugin executes
// asynchronously on the daemon.
func (c *Client) PluginRun(req PluginRunRequest) error {
	data, _ := json.Marshal(req)
	resp, err := c.send(Request{Action: "plugin-run", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}
