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
// than any legitimate handler run. Where the handler's legitimate duration is
// unknowable from here — a popup's user-controlled lifetime, a user-configurable
// hook timeout, an rm -rf over a checkout of unknown size — the client passes 0
// to sendWithTimeout and defers to the bound the handler already owns. A bound
// we cannot justify does not protect anyone: a timeout is not a cancellation,
// so it would only report "outcome unknown" for work that went on to succeed.
const (
	// dialTimeout bounds connecting to the daemon socket. A local unix socket
	// either connects instantly or not at all, so anything slower means the
	// daemon is not accepting; failing fast beats blocking the caller forever.
	dialTimeout = 2 * time.Second

	// requestWriteTimeout bounds writing the request, for every action alike.
	// The bound does not vary because what it guards does not vary: a request
	// is one small JSON value, and the daemon hands each accepted connection
	// to its own goroutine that decodes immediately (server.go handleConnection),
	// so the write never waits on handler work — however long that work may
	// legitimately take. A write that blocks for seconds means the daemon
	// stopped reading altogether, and 5s is already several orders of
	// magnitude past what writing a few hundred bytes into a local socket
	// costs.
	requestWriteTimeout = 5 * time.Second

	// defaultRequestTimeout bounds the wait for a response on every action
	// that does not name its own bound. Out of its scope are "new" and
	// "delete" (each fronts an external process with no bound this side can
	// name) and "pane-popup" (user-controlled lifetime); "hook" and "stop"
	// take the tighter bounds below. What is left is tmux subprocess calls
	// and local file reads, plus one handler with a named cost: "send" waits
	// up to sendVerifyTimeout (5s) for the prompt to appear in the pane.
	// Those handlers also queue behind the manager lock, so 60s is chosen to
	// clear a backlog of them comfortably — hitting it should mean "the
	// daemon is wedged", not "this machine is loaded".
	defaultRequestTimeout = 60 * time.Second

	// hookRequestTimeout bounds the agent-facing hook path. The trade cuts
	// both ways here. A stalled hook blocks the agent process itself, so the
	// path must stay bounded; but an overrun is worse than an ordinary
	// failure, because cmd/jin/cmd/hook.go only logs it and exits 0 — the
	// status update is dropped with nothing shown to the user, and the session
	// looks frozen in the TUI. 10s therefore sits well clear of the handler's
	// real cost (HandleHookEvent upgrades the description inline, reading
	// transcript JSONL under the manager lock) while still capping what a
	// wedged daemon can cost the agent.
	//
	// That makes 10s the effective bound for claude and codex only. On
	// opencode the effective bound stays 3s, because the plugin SIGKILLs the
	// `jin hook` child at HOOK_TIMEOUT_MS (internal/agent/opencode/plugin/jin.ts)
	// before this one can fire. Raising the client bound was deliberately not
	// meant to cover opencode: the plugin's kill routes through done(false),
	// which drops the entry from lastSent so the report is re-sent on the next
	// event, whereas claude and codex just log and exit 0 and lose the update.
	// The asymmetry is intended — opencode can afford the tighter bound
	// because it recovers from overrunning it.
	//
	// This bound covers the "hook" action only because that is the only
	// agent-facing action a Go client sends today. The "agent-signal" action
	// is the same path in every respect that matters here: the server
	// dispatches it to Manager.HandleAgentSignal, which forwards kind "hook"
	// straight into the same HandleHookEvent. It has no client method yet, so
	// if one is added it must pass this bound explicitly — reaching for send()
	// would inherit defaultRequestTimeout and leave the agent blocked for a
	// minute on the wedged daemon this bound exists to cap at ten seconds.
	hookRequestTimeout = 10 * time.Second

	// stopRequestTimeout bounds the stop request. Stopping is the remedy this
	// package points users at when a request times out, so it must stay
	// responsive against exactly the wedged daemon it is meant to clear —
	// waiting defaultRequestTimeout there would make the cure look as broken
	// as the disease. handleStop replies before it does any work, so a daemon
	// healthy enough to answer at all answers quickly; Stop already treats a
	// failed send as non-fatal and confirms through IsRunning instead.
	stopRequestTimeout = 5 * time.Second

	// stopPollAttempts and stopPollInterval bound how long Stop waits for the
	// daemon to actually go away once the request has been sent — sent, not
	// acknowledged, since the poll runs whether or not an answer came back.
	// handleStop replies before shutting down (it cannot answer over the
	// socket it is about to close), so even an acknowledgement only means
	// "accepted"; this poll is the only thing that turns it into "stopped".
	// Their product is far past the listener close and os.Exit that follow,
	// and a test in protocol_test.go pins it rather than leaving the figure
	// quoted here on trust.
	stopPollAttempts = 30
	stopPollInterval = 100 * time.Millisecond
)

// dialDaemon is the package's one door to the socket. It is a var so that
// tests can record the dial timeout and wrap the returned conn to observe
// which deadlines the client set — "no read deadline at all" is not something
// waiting can demonstrate. Swapping a package-level var means the tests that
// do so must stay serial; nothing in this package calls t.Parallel().
//
// This is deliberately not the interface seam the repo reaches for elsewhere
// (tmux.Runner, per docs/conventions.md). Client carries no other injected
// dependency and every caller builds one through NewClient(path) alone, so a
// constructor parameter or field would add an injection point whose only user
// is the test binary — widening the type's surface to say what this var
// already says. Revisit if Client ever grows a second seam, or if a test in
// here needs t.Parallel(); until then the var is the smaller thing.
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
// write keep their own fixed bounds either way.
//
// Each deadline is set once, before writing, because every response is a single
// JSON value read in one Decode — there is no streaming path that would need
// the deadline extended mid-exchange.
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
		return nil, fmt.Errorf("daemon not running. Start with: jin daemon")
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
		// wrapDeadline returns the error unwrapped, message discarded. That is
		// a consequence of the branch above, not a property of this call — put
		// a bound on the read path unconditionally and this string starts
		// escaping, claiming the daemon had zero seconds to answer.
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

// wrapDeadline turns a deadline overrun into a message that distinguishes
// "the daemon is stuck" from "the daemon is gone". stalled says which half of
// the exchange ran out of time and after how long; the two halves fail for
// different reasons and must not be described alike — an overrun while writing
// means the daemon stopped reading, not that it failed to answer.
//
// The protocol has no cancel channel, so giving up here does not stop the
// daemon: a mutating action such as new or delete may well have completed.
// Those get an unknown-outcome wording rather than a failure, so callers are
// not nudged into blindly repeating them. That holds on the write side too:
// the daemon decodes with a json.Decoder, which is satisfied by the closing
// brace, so a write that timed out may nonetheless have delivered a complete
// request. Encode issues the value and its newline as one Write, so the cut
// can land anywhere in it — including past the closing brace. Which is why the
// wording has to stay cautious: where it landed is not observable from here.
// Read-only actions get the plain message — telling someone
// to go check state after a failed `list` spends the warning's credibility
// where nothing is at stake.
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

	// No read bound: CreateWithOptions runs a chain of unbounded steps —
	// git prune, then `git worktree add`, then the post-create hook where
	// `npm ci` and friends live. Only the last of those is capped (by the
	// user's worktree.hook_timeout, 300s by default and freely raised), so
	// there is no duration to name here that would not eventually fire on a
	// session that was in fact being created successfully.
	resp, err := c.sendWithTimeout(Request{Action: "new", Data: data}, 0)
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
	// No read bound, for the same reason as NewWithOptions: Manager.Delete runs
	// `git worktree remove` synchronously and that has no timeout on either
	// side. Removing a worktree is an rm -rf of a whole checkout — node_modules
	// included — so how long it legitimately takes is a property of the user's
	// disk, not something this side can name. It runs outside the manager lock
	// (manager.go), so a slow delete does not hold up other actions either.
	resp, err := c.sendWithTimeout(Request{Action: "delete", Data: data}, 0)
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

// Stop stops the daemon and waits for it to actually exit.
//
// A protocol-mismatched daemon still executes the stop action — its handler
// runs before we notice the client-side mismatch on the response — so we
// swallow the send error when a subsequent IsRunning() poll confirms the
// daemon did shut down. There is one caller — stopDaemonIfRunning in
// cmd/jin/cmd/daemon.go — and both `jin daemon stop` and `jin daemon restart`
// reach it through there, so both get this behavior without re-implementing
// the poll. Anything added later inherits it the same way; the error wording
// below assumes only that the caller wanted the daemon stopped.
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
	// Past the poll the daemon is still accepting, and the remedy the rest of
	// this package points at is no help: `jin daemon restart` stops through
	// this very function, so naming it here would answer a failed stop with
	// the same stop. Send the user outside the socket instead — a daemon that
	// ignored the request needs a signal, not another request.
	if errors.Is(sendErr, os.ErrDeadlineExceeded) {
		// pkill is offered as an example rather than the instruction: the
		// pattern also matches a daemon started on another --socket, which the
		// user may not want to take down.
		//
		// The two callers named above want opposite things once the kill is
		// done — restart is left without the daemon it was going to start
		// again, while stop got what it asked for — so the start half is
		// offered conditionally. Telling every reader to start one back up
		// would be this same message's mistake aimed at the other caller.
		return fmt.Errorf(
			"daemon did not respond to stop within %s and is still accepting connections — kill it manually (e.g. pkill -f 'jin daemon'); if you were restarting, start the new one with: jin daemon start (%w)",
			stopRequestTimeout, os.ErrDeadlineExceeded,
		)
	}
	return sendErr
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
