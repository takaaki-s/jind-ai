package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/debug"
	"github.com/takaaki-s/jind-ai/internal/jinenv"
	"github.com/takaaki-s/jind-ai/internal/paths"
	"github.com/takaaki-s/jind-ai/internal/plugin"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/tmux"
	"github.com/takaaki-s/jind-ai/internal/transcript"
	"github.com/takaaki-s/jind-ai/internal/worktreehook"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// agentResolverAdapter wraps agent.Lookup so it satisfies session.AgentResolver
// without leaking the registry package into internal/session. This is the
// single point of contact between the two packages — session never imports
// internal/agent directly.
type agentResolverAdapter struct{}

func (agentResolverAdapter) Resolve(kind string) (session.Agent, error) {
	return agent.Lookup(kind)
}

var debugLog = debug.NewLogger("daemon-debug.log")

// Server is the daemon server
type Server struct {
	socketPath string
	manager    *session.Manager
	configMgr  *config.Manager
	stateMgr   *config.StateManager
	pluginDisp *plugin.EventDispatcher
	createMu   sync.Mutex // Mutual exclusion for session creation

	// Shutdown state, written by Stop and read by Start's accept loop from
	// another goroutine. lifecycleMu guards both fields.
	//
	// stopping is an explicit sentinel rather than a `listener == nil` check:
	// conflating "the field was cleared" with "shutdown was intended" is what
	// forced the accept loop to read a field Stop was concurrently writing. It
	// also lets Stop publish the intent before closing the listener, so the loop
	// can never observe a close-induced Accept error while the sentinel still
	// says "unexpected" and spin on the dead listener.
	lifecycleMu sync.Mutex
	listener    net.Listener
	stopping    bool
	stopOnce    sync.Once
}

// Message types
//
// ProtocolVersion travels on every request and response so a version mismatch
// between the CLI and a running daemon fails loudly with an actionable message,
// instead of surfacing as endpoint-specific JSON parse errors.
type Request struct {
	ProtocolVersion int             `json:"protocol_version,omitempty"`
	Action          string          `json:"action"`
	Data            json.RawMessage `json:"data,omitempty"`
}

type Response struct {
	ProtocolVersion int             `json:"protocol_version,omitempty"`
	Success         bool            `json:"success"`
	Data            json.RawMessage `json:"data,omitempty"`
	Error           string          `json:"error,omitempty"`
}

// NewServer creates a new daemon server.
//
// sessionsDir holds per-session JSON files; configDir holds config.yaml;
// stateDir holds state.yaml plus generated artifacts (hooks-settings.json).
// XDG-compliant defaults are resolved by the caller via internal/paths.
func NewServer(socketPath, sessionsDir, configDir, stateDir string) (*Server, error) {
	configMgr, err := config.NewManager(configDir)
	if err != nil {
		return nil, err
	}

	stateMgr, err := config.NewStateManager(stateDir)
	if err != nil {
		return nil, err
	}

	// Which jin every child this daemon spawns is told to call back into.
	// Assembled once, here, because this is the only place that knows all three
	// answers, and answering separately per spawn site is what once let them
	// disagree. The binary is a copy taken now, so the path baked into a session's
	// hooks survives the launch binary being rebuilt or deleted.
	identity := jinenv.Identity{
		SocketPath: socketPath,
		BinPath:    session.EstablishHookBinary(stateDir),
		Debug:      debug.Enabled(),
	}

	mgr, err := session.NewManager(sessionsDir, stateDir, identity, configMgr)
	if err != nil {
		return nil, err
	}

	// Wire the agent resolver so startSessionTmux / HandleHookEvent can
	// dispatch to the adapter that owns each session's kind. Layer C
	// description enhancers now live behind Agent.Description() — no
	// separate wiring is needed here.
	mgr.SetAgentResolver(agentResolverAdapter{})

	// Set up tmux client whenever the tmux binary is available. Recovery
	// decides per-session whether its inner tmux session is still alive —
	// gating on a pre-existing outer session would skip recovery entirely
	// (inner sessions are standalone "sess-*" sessions, and has-session
	// queries never spawn a tmux server).
	if tc, err := tmux.NewClient(); err == nil {
		mgr.SetTmuxClient(tc)
		mgr.RecoverTmuxSessions()
		// Report the socket the client actually bound to, not the built-in
		// default: under JIN_TMUX_SOCKET these differ, and this line is the
		// primary evidence that an isolated run is talking to its own server.
		debugLog("tmux client initialized (socket: %s)", tc.GetSocketName())
	}

	hookRunner, err := worktreehook.NewRunner(stateDir)
	if err != nil {
		return nil, fmt.Errorf("initializing worktree hook runner: %w", err)
	}
	mgr.SetHookRunner(hookRunner)

	pluginCfg := configMgr.GetPluginsConfig()
	pluginReg := plugin.NewRegistry(paths.Plugins(), stateDir, pluginCfg)
	pluginDisp := plugin.NewDispatcher(pluginReg, paths.Plugins(), stateDir, identity,
		time.Duration(pluginCfg.Debounce)*time.Second,
		// actionID is accepted but unused for now: user config keys popup size
		// by plugin name only, so every action shares the plugin-level setting.
		func(pluginName, _ string, m *manifest.PopupConfig) (string, string) {
			var cfgManifest *config.PopupSizeConfig
			if m != nil {
				cfgManifest = &config.PopupSizeConfig{Width: m.Width, Height: m.Height}
			}
			return configMgr.GetPluginPopupSize(pluginName, cfgManifest)
		})
	mgr.SetPluginDispatcher(pluginDisp)

	return &Server{
		socketPath: socketPath,
		manager:    mgr,
		configMgr:  configMgr,
		stateMgr:   stateMgr,
		pluginDisp: pluginDisp,
	}, nil
}

// Start starts the daemon server
func (s *Server) Start() error {
	// Remove existing socket
	os.Remove(s.socketPath)

	// Ensure directory exists with user-only permissions (XDG_RUNTIME_DIR rules
	// apply, and the TMPDIR fallback shares a multi-user space).
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0700); err != nil {
		return err
	}

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return err
	}
	// Publishing the listener and re-checking the sentinel happen under one lock,
	// which closes the window between net.Listen returning and the field being
	// visible to Stop. A Stop landing in that window would find a nil listener,
	// close nothing, and leave the accept loop below blocked forever on a
	// listener nobody owns. Exactly one side wins.
	s.lifecycleMu.Lock()
	if s.stopping {
		s.lifecycleMu.Unlock()
		listener.Close()
		// Stop's own os.Remove ran before net.Listen recreated the socket
		// file, so clean up the leftover here.
		os.Remove(s.socketPath)
		return nil
	}
	s.listener = listener
	s.lifecycleMu.Unlock()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		s.Stop()
		os.Exit(0)
	}()

	log.Printf("Daemon listening on %s", s.socketPath)

	for {
		// listener is the local variable on purpose: Accept blocks, so
		// holding lifecycleMu across it would deadlock Stop.
		conn, err := listener.Accept()
		if err != nil {
			if s.stopRequested() {
				return nil // Server stopped
			}
			log.Printf("Accept error: %v", err)
			continue
		}
		go s.handleConnection(conn)
	}
}

// stopRequested reports whether Stop has been entered, telling the accept
// loop whether an Accept error is our own shutdown or a genuine fault.
func (s *Server) stopRequested() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.stopping
}

// Stop stops the daemon server.
//
// Safe to call concurrently and more than once: the signal handler goroutine,
// handleStop's goroutine, and an explicit call can all fire at the same time.
// sync.Once rather than an early return on a flag, so later callers block until
// the first has finished closing the listener and removing the socket — a
// caller that follows Stop with os.Exit cannot exit before the cleanup lands.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		// Publish the intent before closing, so the accept loop is guaranteed
		// to see stopping=true on the Accept error that the Close below wakes.
		s.lifecycleMu.Lock()
		s.stopping = true
		listener := s.listener
		s.listener = nil
		s.lifecycleMu.Unlock()

		if listener != nil {
			listener.Close()
		}
		os.Remove(s.socketPath)
	})
}

func (s *Server) handleConnection(conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	var req Request
	if err := decoder.Decode(&req); err != nil {
		if err != io.EOF {
			log.Printf("Decode error: %v", err)
		}
		return
	}

	var resp Response
	if req.ProtocolVersion != ProtocolVersion {
		// Refuse the request before dispatching so no side effect happens
		// against an incompatible caller. A pre-versioning CLI sends 0 here;
		// treat that identically to any other mismatch.
		resp = Response{Success: false, Error: fmt.Sprintf(
			"client protocol version %d does not match daemon %d — reinstall jin to match the running daemon (or stop the daemon with SIGTERM and restart it after updating)",
			req.ProtocolVersion, ProtocolVersion,
		)}
	} else {
		resp = s.handleRequest(&req)
	}
	resp.ProtocolVersion = ProtocolVersion
	_ = encoder.Encode(resp)
}

func (s *Server) handleRequest(req *Request) Response {
	switch req.Action {
	case "new":
		return s.handleNew(req.Data)
	case "list":
		return s.handleList()
	case "get":
		return s.handleGet(req.Data)
	case "send":
		return s.handleSend(req.Data)
	case "start":
		return s.handleStart(req.Data)
	case "kill":
		return s.handleKill(req.Data)
	case "delete":
		return s.handleDelete(req.Data)
	case "stop":
		return s.handleStop()
	case "hook":
		return s.handleHook(req.Data)
	case "dir-history":
		return s.handleDirHistory(req.Data)
	case "remove-dir-history":
		return s.handleRemoveDirHistory(req.Data)
	case "result":
		return s.handleResult(req.Data)
	case "set-description":
		return s.handleSetDescription(req.Data)
	case "attention-seen":
		return s.handleAttentionSeen(req.Data)
	case "agent-signal":
		return s.handleAgentSignal(req.Data)
	case "pane-popup":
		return s.handlePanePopup(req.Data)
	case "pane-split":
		return s.handlePaneSplit(req.Data)
	case "pane-close":
		return s.handlePaneClose(req.Data)
	case "pane-capture":
		return s.handlePaneCapture(req.Data)
	case "pane-send-keys":
		return s.handlePaneSendKeys(req.Data)
	case "respond":
		return s.handleRespond(req.Data)
	case "plugin-run":
		return s.handlePluginRun(req.Data)
	default:
		return Response{Success: false, Error: fmt.Sprintf("unknown action: %s", req.Action)}
	}
}

// readOnlyActions names the actions above whose handlers only read state. The
// client uses it to decide whether a timeout needs the "your request may have
// gone through anyway" warning (see wrapDeadline); it lives here so the two
// stay in sync as the switch grows.
//
// Membership is opt-in on purpose: an action forgotten here gets the cautious
// wording, which is wrong but harmless. The reverse mistake would be silent.
var readOnlyActions = map[string]bool{
	"list":         true,
	"get":          true,
	"dir-history":  true,
	"pane-capture": true,
	"result":       true,
}

// AgentSignalRequest carries a generic status signal from any agent adapter's
// out-of-band notifier (a hook binary for Claude Code, a pane-output tailer
// for adapters without hooks). Manager routes the Payload through the
// registered agent's StatusSource.Interpret.
type AgentSignalRequest struct {
	JinSessionID string            `json:"jin_session_id"`
	Kind         string            `json:"kind"` // "hook" | "pane-output" | ...
	Payload      map[string]string `json:"payload,omitempty"`
}

func (s *Server) handleAgentSignal(data json.RawMessage) Response {
	var req AgentSignalRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.JinSessionID == "" {
		return Response{Success: false, Error: "jin_session_id is required"}
	}
	if req.Kind == "" {
		return Response{Success: false, Error: "kind is required"}
	}
	s.manager.HandleAgentSignal(req.JinSessionID, req.Kind, req.Payload)
	return Response{Success: true}
}

// HookRequest represents a Claude Code hook event
type HookRequest struct {
	SessionID        string `json:"session_id"`
	JinSessionID     string `json:"jin_session_id,omitempty"`
	HookEventName    string `json:"hook_event_name"`
	NotificationType string `json:"notification_type,omitempty"`
	CWD              string `json:"cwd,omitempty"`
	StopReason       string `json:"stop_reason,omitempty"`
}

func (s *Server) handleHook(data json.RawMessage) Response {
	var req HookRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	s.manager.HandleHookEvent(req.SessionID, req.JinSessionID, req.HookEventName, req.NotificationType, req.CWD, req.StopReason)
	return Response{Success: true}
}

type NewRequest struct {
	Description string `json:"description"`
	WorkDir     string `json:"work_dir"`
	Start       bool   `json:"start"`
	Fleet       string `json:"fleet"`                // Fleet name for session grouping
	AgentKind   string `json:"agent_kind,omitempty"` // Adapter identifier; daemon defaults to config default_agent when empty
	Model       string `json:"model,omitempty"`      // Agent model in the CLI's own spelling; passed through unvalidated (see handleNew)

	Worktree       bool   `json:"worktree,omitempty"`        // Create a git worktree for this session
	WorktreeName   string `json:"worktree_name,omitempty"`   // Override auto-generated worktree name
	WorktreeBranch string `json:"worktree_branch,omitempty"` // Override auto-generated branch name
	WorktreeBase   string `json:"worktree_base,omitempty"`   // Override auto-detected base branch
	NoHook         bool   `json:"no_hook,omitempty"`         // Skip .jin/worktree-post-create.sh hook
}

// NewResponse is the payload for the "new" action. Warning is a non-fatal
// message emitted at creation (e.g. hook skipped because the repo is not
// allowlisted); it is scoped to this single response and is not attached to
// the persisted Session, so subsequent Get/List do not repeat it.
type NewResponse struct {
	session.Info
	Warning string `json:"warning,omitempty"`
}

func (s *Server) handleNew(data json.RawMessage) Response {
	var req NewRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Backfill the agent kind from user config, then validate against the
	// registry. Validation up front means the CLI gets a clear "unknown
	// kind" error before any tmux / worktree side effects run.
	if req.AgentKind == "" {
		req.AgentKind = s.configMgr.GetDefaultAgent()
	}
	if _, err := agent.Lookup(req.AgentKind); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Model gets no equivalent gate: there is no registry to check it against,
	// and each agent's catalogue moves faster than a jind-ai release, so an
	// allowlist would reject valid new models.
	//
	// Nor does an agent necessarily reject one, and they do not behave alike:
	// Claude Code starts and warns inside the pane (3/3 measured), which is what
	// its modelOverrides setting is for; opencode starts and says nothing at all
	// (3/3). Either way a typo produces a session jind-ai reports as running.
	// That is the cost of the pass-through, and it is paid knowingly rather than
	// papered over with a list that would go stale.
	opts := session.CreateOptions{
		Description:    req.Description,
		WorkDir:        req.WorkDir,
		Fleet:          req.Fleet,
		AgentKind:      req.AgentKind,
		Model:          req.Model,
		Worktree:       req.Worktree,
		WorktreeName:   req.WorktreeName,
		WorktreeBranch: req.WorktreeBranch,
		WorktreeBase:   req.WorktreeBase,
		NoHook:         req.NoHook,
	}

	// Take createMu around the *entire* async lifetime, not just the
	// synchronous prefix: concurrent `new` requests must still be serialized
	// so their worktree provisioning cannot interleave and (worse) so their
	// findAvailableWorktreeName probes see each other's committed state. The
	// goroutine below is what unlocks it.
	s.createMu.Lock()

	sess, info, err := s.manager.ReserveCreation(opts)
	if err != nil {
		s.createMu.Unlock()
		return Response{Success: false, Error: err.Error()}
	}
	sessID := info.ID

	// Dispatch the provisioning + start to a goroutine and return the
	// StatusCreating reservation to the caller immediately. The client
	// polls `get` to observe the transition to running/idle/stopped, and
	// any failure is surfaced through Session.ErrorMessage rather than the
	// (already-departed) request/response.
	start := req.Start
	workDir := req.WorkDir
	go func() {
		defer s.createMu.Unlock()

		warning, provErr := s.manager.ProvisionAsync(sess, opts)
		if provErr != nil {
			s.manager.MarkCreationFailed(sessID, provErr)
			return
		}
		if warning != "" {
			s.manager.SetCreationWarning(sessID, warning)
		}

		_ = s.stateMgr.RecordDirUsage(workDir)

		if start {
			// StartBackground moves Status off Creating through the
			// adapter's own path. On failure we mark the session and
			// leave the record so the client can see what happened.
			if err := s.manager.StartBackground(sessID); err != nil {
				s.manager.MarkCreationFailed(sessID, err)
				return
			}
		} else {
			// No start requested — provisioning is done but no agent is
			// running. Move Status off Creating so `get` shows "ready to
			// start" instead of a stuck creating state.
			s.manager.SetStatus(sessID, session.StatusStopped)
		}
	}()

	// Warning is intentionally empty here: any warning discovered during
	// async provisioning is written to Session.CreationWarning by the
	// goroutine above and observable through `get`. NewResponse.Warning is
	// kept for the (currently empty) set of synchronous warnings.
	respData, _ := json.Marshal(NewResponse{Info: info})
	return Response{Success: true, Data: respData}
}

func (s *Server) handleList() Response {
	sessions := s.manager.List()
	data, _ := json.Marshal(sessions)
	return Response{Success: true, Data: data}
}

func (s *Server) handleGet(data json.RawMessage) Response {
	var req IDRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// GetInfo (not Get) so we snapshot under the manager's read lock; the
	// async provisioning / delete goroutines can mutate the record while a
	// client polls `get`, and reading through Get's aliased pointer would
	// race with those writers.
	info, ok := s.manager.GetInfo(req.ID)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("session not found: %s", req.ID)}
	}

	// Enrich with transcript data, through the adapter that owns the session —
	// see Manager.AttachLastMessages for why the previews were blank on every
	// non-Claude kind before.
	s.manager.AttachLastMessages(&info)

	respData, _ := json.Marshal(info)
	return Response{Success: true, Data: respData}
}

// SendRequest is the request payload for the "send" action
type SendRequest struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
}

func (s *Server) handleSend(data json.RawMessage) Response {
	var req SendRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Reject prompts SendPrompt could not verify. Its verify path searches the
	// captured pane for the prompt's tail, and a prompt that normalizes to
	// nothing leaves no needle — sendVerifyOK then accepts it trivially, so one
	// let through here would send an unverified Enter to the TUI. Deferring to
	// session.PromptVerifiable keeps the two rules from drifting apart.
	if req.Prompt == "" {
		return Response{Success: false, Error: "prompt is required"}
	}
	if !session.PromptVerifiable(req.Prompt) {
		return Response{Success: false, Error: "prompt has no verifiable content " +
			"(only whitespace or box-drawing characters)"}
	}
	// Success here means the keystrokes reached the input area and Enter was
	// pressed — not that a turn began on them. SendPrompt closes any completion
	// overlay first and re-checks the prompt survived, but nothing downstream
	// confirms the agent picked the prompt up. Callers that need that fact poll
	// for it (`send --wait-running`).
	if err := s.manager.SendPrompt(req.ID, req.Prompt); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
}

// RespondRequest is the request payload for the "respond" action: an answer
// to a prompt an agent is blocked on. Exactly one of Option and Text carries
// the answer.
type RespondRequest struct {
	ID string `json:"id"`
	// Option is a choice's on-screen number, 1-based.
	Option int `json:"option,omitempty"`
	// Text is free text, for a prompt that offers a free-text entry.
	Text string `json:"text,omitempty"`
}

// RespondResponse reports which sort of prompt was answered. The caller
// cannot see the pane, and asking it to capture one to find out would push it
// onto the exact evidence jin's own docs warn against — a capture keeps
// finished menus on screen.
type RespondResponse struct {
	Kind string `json:"kind"`
}

// RespondNotClearedPrefix tags the one "respond" failure the CLI maps to the
// timeout exit code: the prompt was still on screen after the answer went out.
//
// A prefix rather than a substring match on the message, which would couple an
// exit code the docs promise to wording nobody would think to keep stable.
// Callers strip it before display.
const RespondNotClearedPrefix = "not-cleared: "

// tagRespondError flattens a RespondToBlock failure into the wire's one error
// string, marking the single case the CLI turns into a distinct exit code.
// Split out from the handler because the handler is not reachable in a test
// without a live tmux pane, and a classifier nothing exercises silently stops
// classifying.
func tagRespondError(err error) string {
	if errors.Is(err, session.ErrBlockNotCleared) {
		return RespondNotClearedPrefix + err.Error()
	}
	return err.Error()
}

func (s *Server) handleRespond(data json.RawMessage) Response {
	var req RespondRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.ID == "" {
		return Response{Success: false, Error: "id is required"}
	}
	// Validated here as well as in the CLI because this endpoint is reachable
	// without it — plugins shell out to jin, but nothing stops a caller
	// speaking the protocol directly.
	hasOption, hasText := req.Option != 0, req.Text != ""
	switch {
	case hasOption && hasText:
		return Response{Success: false, Error: "pass either an option or text, not both"}
	case !hasOption && !hasText:
		return Response{Success: false, Error: "an answer is required: pass an option or text"}
	}
	if hasOption && (req.Option < 1 || req.Option > session.MaxAnswerOption) {
		return Response{Success: false, Error: fmt.Sprintf(
			"option must be between 1 and %d (an answer is one keystroke)", session.MaxAnswerOption)}
	}
	// Reject text the verify step could not search the pane for, by the same
	// rule and for the same reason as "send" — RespondToBlock confirms free
	// text rendered before it presses the key that submits it, and text that
	// normalizes to nothing would satisfy that check without evidence.
	if hasText && len(req.Text) > session.MaxAnswerTextBytes {
		return Response{Success: false, Error: fmt.Sprintf(
			"text is %d bytes; answers over %d cannot be verified because the agent folds a "+
				"write that large into a placeholder, hiding the text jin checks for",
			len(req.Text), session.MaxAnswerTextBytes)}
	}
	if hasText && !session.PromptVerifiable(req.Text) {
		return Response{Success: false, Error: "text has no verifiable content " +
			"(only whitespace or box-drawing characters)"}
	}

	kind, err := s.manager.RespondToBlock(req.ID, session.BlockAnswer{Option: req.Option, Text: req.Text})
	if err != nil {
		// The protocol carries errors as strings, so the one distinction the
		// CLI has to make — "the prompt never cleared", which becomes the
		// timeout exit code — is tagged here while the typed error still
		// exists. Matching the message text on the far side would make a
		// reworded sentence silently change an exit code.
		return Response{Success: false, Error: tagRespondError(err)}
	}
	// Unlike "send", success here means the prompt left the pane — not merely
	// that keys were delivered. That is why this action has no equivalent of
	// send's --wait-running: there is no gap between "sent" and "taken" left
	// for a caller to poll.
	payload, err := json.Marshal(RespondResponse{Kind: string(kind)})
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true, Data: payload}
}

// ResultRequest is the request payload for the "result" action.
// Used by orchestrators to fetch structured transcript entries (text/thinking/
// tool_use/tool_result) from a session, with optional incremental and filter modes.
type ResultRequest struct {
	ID         string `json:"id"`
	Since      string `json:"since,omitempty"`       // ISO8601; only entries with Timestamp > Since are returned
	Last       int    `json:"last,omitempty"`        // Truncate to last N entries (after filtering); 0 = no truncation
	Tool       string `json:"tool,omitempty"`        // Keep entries that contain a tool_use or tool_result for this tool name
	ErrorsOnly bool   `json:"errors_only,omitempty"` // Keep entries that contain at least one tool_result with is_error=true
}

// ResultResponse is the response payload for the "result" action.
type ResultResponse struct {
	SessionID      string             `json:"session_id"`
	AgentSessionID string             `json:"agent_session_id,omitempty"`
	Entries        []transcript.Entry `json:"entries"`
	Truncated      bool               `json:"truncated,omitempty"` // true if Last truncation was applied
}

// MarshalJSON renders Entries as an array even when it is nil.
//
// The invariant belongs on the type rather than at a call site because this
// value is marshalled twice on the way to a user: once by the daemon, and again
// by the CLI, which re-encodes the struct it decoded. A nil slice encodes as
// `null`, and the agent-facing docs tell an orchestrator to separate "the child
// said nothing" from "the conversation was lost" with
// `jq '.entries[] | select(.type=="system")'` — which dies with "Cannot iterate
// over null" at precisely that moment.
func (r ResultResponse) MarshalJSON() ([]byte, error) {
	// The alias drops the method set, so this does not recurse.
	type wire ResultResponse
	if r.Entries == nil {
		r.Entries = []transcript.Entry{}
	}
	return json.Marshal(wire(r))
}

func (s *Server) handleResult(data json.RawMessage) Response {
	var req ResultRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	if req.Last < 0 {
		return Response{Success: false, Error: "last must be >= 0"}
	}

	sess, ok := s.manager.Get(req.ID)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("session not found: %s", req.ID)}
	}
	info := sess.ToInfo()

	resp := ResultResponse{SessionID: info.ID, AgentSessionID: info.AgentSessionID}

	// No agent session ID yet means the agent has not been launched, which is
	// a state every session starts in rather than a failure. Empty + success
	// is the right answer here and stays the right answer below only when a
	// reader actually looked.
	if info.AgentSessionID == "" {
		respData, _ := json.Marshal(resp)
		return Response{Success: true, Data: respData}
	}

	workDir := info.CurrentWorkDir
	if workDir == "" {
		workDir = info.WorkDir
	}

	// Ask the session's own adapter for its transcript. This used to call the
	// Claude Code reader unconditionally, so a Codex or opencode session got
	// zero entries and success — the same answer as a child agent that ran
	// and produced nothing. An orchestrator has no way to tell those apart,
	// so it reads "no output" and moves on.
	ag, err := agent.Lookup(info.AgentKind)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	src := ag.Transcript()
	if src == nil {
		return Response{Success: false, Error: fmt.Sprintf(
			"cannot read a transcript for agent kind %q: this adapter has no transcript reader", info.AgentKind)}
	}
	entries, err := src.ReadEntries(workDir, info.AgentSessionID, req.Since)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	filtered := filterResultEntries(entries, req.Tool, req.ErrorsOnly)
	if req.Last > 0 && len(filtered) > req.Last {
		// Copy the tail rather than resliced it. A reslice keeps the whole
		// backing array reachable until the response is marshalled, and on a
		// Codex session an entry can hold a tool output measured in megabytes —
		// so `--last 1` would hold the entire transcript in memory to return
		// one entry.
		filtered = append([]transcript.Entry(nil), filtered[len(filtered)-req.Last:]...)
		resp.Truncated = true
	}
	resp.Entries = filtered

	respData, _ := json.Marshal(resp)
	return Response{Success: true, Data: respData}
}

// filterResultEntries keeps entries that contain at least one block matching
// the given tool name and/or error filter. An empty tool with errorsOnly=false
// returns the input as-is. For a tool_result entry, name matching requires
// having seen the corresponding tool_use earlier in the input (by tool_use_id).
//
// It reads the shared block vocabulary and nothing agent-specific, which is why
// it stays here rather than moving into the adapters: a per-adapter filter would
// let --tool and --errors-only mean different things per agent kind.
//
// The vocabulary is what is short. IsError=false means "the agent said it
// succeeded" on Claude Code and "jind-ai could not tell" on Codex, which records
// no failure flag at all. Do not resolve that by branching on kind here; it is
// fixed by giving the block an error state that can say "undetermined", which
// changes the wire format and so is its own change.
func filterResultEntries(entries []transcript.Entry, tool string, errorsOnly bool) []transcript.Entry {
	if tool == "" && !errorsOnly {
		return entries
	}
	// Build tool_use_id -> name map by scanning forward.
	useNameByID := map[string]string{}
	if tool != "" {
		for _, e := range entries {
			for _, b := range e.Blocks {
				if b.Kind == "tool_use" && b.ToolUseID != "" {
					useNameByID[b.ToolUseID] = b.ToolName
				}
			}
		}
	}
	out := make([]transcript.Entry, 0, len(entries))
	for _, e := range entries {
		if entryMatches(e, tool, errorsOnly, useNameByID) {
			out = append(out, e)
		}
	}
	return out
}

func entryMatches(e transcript.Entry, tool string, errorsOnly bool, useNameByID map[string]string) bool {
	for _, b := range e.Blocks {
		switch b.Kind {
		case "tool_use":
			if errorsOnly {
				continue
			}
			if tool == "" || b.ToolName == tool {
				return true
			}
		case "tool_result":
			if errorsOnly && !b.IsError {
				continue
			}
			if tool == "" {
				return true
			}
			if useNameByID[b.ToolUseID] == tool {
				return true
			}
		}
	}
	return false
}

// SetDescriptionRequest is the request payload for the "set-description" action.
// Description intentionally has no omitempty tag: an empty string is a valid,
// meaningful request (unlock + regenerate the Layer A baseline), distinct from
// an absent field.
type SetDescriptionRequest struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// SetDescriptionResponse is the response payload for the "set-description" action.
type SetDescriptionResponse struct {
	Session session.Info `json:"session"`
}

func (s *Server) handleSetDescription(data json.RawMessage) Response {
	var req SetDescriptionRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	if err := s.manager.SetDescription(req.ID, req.Description); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	sess, ok := s.manager.Get(req.ID)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("session not found: %s", req.ID)}
	}

	respData, _ := json.Marshal(SetDescriptionResponse{Session: sess.ToInfo()})
	return Response{Success: true, Data: respData}
}

type IDRequest struct {
	ID string `json:"id"`
}

// handleAttentionSeen acknowledges a session's completion receipt.
//
// Deliberately absent from readOnlyActions: a timeout leaves the outcome
// unknown, and MarkSeen is idempotent, so the client should say "your request
// may have gone through anyway" and let the user retry safely.
func (s *Server) handleAttentionSeen(data json.RawMessage) Response {
	var req IDRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.ID == "" {
		return Response{Success: false, Error: "id is required"}
	}

	info, err := s.manager.MarkSeen(req.ID)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	respData, _ := json.Marshal(info)
	return Response{Success: true, Data: respData}
}

// DeleteRequest extends IDRequest with worktree removal options.
type DeleteRequest struct {
	ID                  string `json:"id"`
	RemoveWorktree      bool   `json:"remove_worktree,omitempty"`
	ForceRemoveWorktree bool   `json:"force_remove_worktree,omitempty"`
}

func (s *Server) handleStart(data json.RawMessage) Response {
	var req IDRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	if err := s.manager.StartBackground(req.ID); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	return Response{Success: true}
}

func (s *Server) handleKill(data json.RawMessage) Response {
	var req IDRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	if err := s.manager.Kill(req.ID); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	return Response{Success: true}
}

func (s *Server) handleDelete(data json.RawMessage) Response {
	var req DeleteRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// PreCheckDelete runs the synchronous validations (session existence,
	// worktree resolution, dirty probe). Failures here surface to the
	// client on the request — the TUI's "worktree is dirty, retry with
	// force?" confirmation depends on this staying synchronous.
	dreq, err := s.manager.PreCheckDelete(req.ID, req.RemoveWorktree, req.ForceRemoveWorktree)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Flip Status to StatusDeleting so a `get` between here and the goroutine's
	// completion surfaces the in-flight state to the UI. MarkDeleting is a CAS: a
	// prior accept still finalizing returns ErrDeleteInFlight, so two finalize
	// goroutines cannot race on the same worktree. It also atomically captures
	// dreq.previousStatus for MarkDeletionFailed's rollback path.
	if err := s.manager.MarkDeleting(&dreq); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Dispatch the destructive tail (git worktree remove, tmux kill, store
	// delete, map drop) to a goroutine. Failures roll Status back to the
	// pre-delete value with ErrorMessage populated, observable through
	// `get`.
	go func() {
		if err := s.manager.DeleteFinalize(dreq); err != nil {
			s.manager.MarkDeletionFailed(dreq, err)
		}
	}()

	return Response{Success: true}
}

func (s *Server) handleStop() Response {
	// Stop in a goroutine to allow response to be sent first
	go func() {
		s.Stop()
		os.Exit(0)
	}()
	return Response{Success: true}
}

func (s *Server) handleDirHistory(data json.RawMessage) Response {
	var req struct {
		MaxEntries int `json:"max_entries"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.MaxEntries <= 0 {
		req.MaxEntries = 5
	}

	entries := s.stateMgr.GetDirHistory(req.MaxEntries)
	respData, _ := json.Marshal(entries)
	return Response{Success: true, Data: respData}
}

func (s *Server) handleRemoveDirHistory(data json.RawMessage) Response {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if err := s.stateMgr.RemoveDirHistory(req.Path); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
}

// PanePopupRequest is the request payload for the "pane-popup" action. It opens
// a tmux popup anchored to the session's pane, running Cmd in the session's
// working directory.
//
// The popup is told which jin to call back into and which session its work
// belongs to: JIN_SOCKET, JIN_BIN, JIN_DEBUG and JIN_SESSION_ID reach it as one
// tmux -e each, all four every time and empty when a value is unknown.
// jinenv.Identity.TmuxEnviron renders them, plus an empty JIN_PLUGIN_DEPTH, and
// says why omitting a key is not the same as leaving it unset.
type PanePopupRequest struct {
	ID     string `json:"id"`
	Cmd    string `json:"cmd"`
	Title  string `json:"title,omitempty"`
	Width  string `json:"width,omitempty"`
	Height string `json:"height,omitempty"`
}

func (s *Server) handlePanePopup(data json.RawMessage) Response {
	var req PanePopupRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.ID == "" {
		return Response{Success: false, Error: "id is required"}
	}
	if req.Cmd == "" {
		return Response{Success: false, Error: "cmd is required"}
	}
	if err := s.manager.PanePopup(req.ID, req.Cmd, req.Title, req.Width, req.Height); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
}

// PaneSplitRequest is the request payload for the "pane-split" action. Cmd is
// optional: an empty split just opens a shell in the new pane. Name enables the
// idempotent named-slot path; IfExists picks the policy when the named pane
// already exists (noop/respawn/error, empty = noop). The new pane is told the
// same variables PanePopupRequest names, and a slot restarted under
// IfExists=respawn is told them again.
type PaneSplitRequest struct {
	ID        string `json:"id"`
	Cmd       string `json:"cmd,omitempty"`
	Direction string `json:"direction,omitempty"` // down (default), up, left, right
	Size      string `json:"size,omitempty"`      // "30%" or "15"
	Full      bool   `json:"full,omitempty"`
	NoFocus   bool   `json:"no_focus,omitempty"`
	Name      string `json:"name,omitempty"`
	IfExists  string `json:"if_exists,omitempty"`
}

// PaneSplitResponse is the response payload for the "pane-split" action.
type PaneSplitResponse struct {
	PaneID string `json:"pane_id"`
}

func (s *Server) handlePaneSplit(data json.RawMessage) Response {
	var req PaneSplitRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.ID == "" {
		return Response{Success: false, Error: "id is required"}
	}
	opts := tmux.SplitOptions{
		Direction: req.Direction,
		Size:      req.Size,
		Full:      req.Full,
		NoFocus:   req.NoFocus,
		Cmd:       req.Cmd,
	}
	if err := tmux.ValidateSlotOptions(req.Name, req.IfExists, opts); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	paneID, err := s.manager.PaneSplit(req.ID, req.Name, req.IfExists, opts)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	respData, _ := json.Marshal(PaneSplitResponse{PaneID: paneID})
	return Response{Success: true, Data: respData}
}

// PaneCloseRequest is the request payload for the "pane-close" action: kill
// the pane created by a named-slot split ("pane-split" with name).
type PaneCloseRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) handlePaneClose(data json.RawMessage) Response {
	var req PaneCloseRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.ID == "" {
		return Response{Success: false, Error: "id is required"}
	}
	if req.Name == "" {
		return Response{Success: false, Error: "name is required"}
	}
	if err := tmux.ValidatePaneName(req.Name); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if err := s.manager.PaneClose(req.ID, req.Name); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
}

// PaneCaptureRequest is the request payload for the "pane-capture" action.
type PaneCaptureRequest struct {
	ID   string `json:"id"`
	ANSI bool   `json:"ansi,omitempty"`
}

// PaneCaptureResponse is the response payload for the "pane-capture" action.
type PaneCaptureResponse struct {
	Content string `json:"content"`
}

func (s *Server) handlePaneCapture(data json.RawMessage) Response {
	var req PaneCaptureRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.ID == "" {
		return Response{Success: false, Error: "id is required"}
	}
	content, err := s.manager.PaneCapture(req.ID, req.ANSI)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	respData, _ := json.Marshal(PaneCaptureResponse{Content: content})
	return Response{Success: true, Data: respData}
}

// PaneSendKeysRequest is the request payload for the "pane-send-keys" action.
type PaneSendKeysRequest struct {
	ID      string `json:"id"`
	Keys    string `json:"keys"`
	Literal bool   `json:"literal,omitempty"`
}

func (s *Server) handlePaneSendKeys(data json.RawMessage) Response {
	var req PaneSendKeysRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.ID == "" {
		return Response{Success: false, Error: "id is required"}
	}
	if req.Keys == "" {
		return Response{Success: false, Error: "keys is required"}
	}
	if err := s.manager.PaneSendKeys(req.ID, req.Keys, req.Literal); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
}

// PluginRunRequest is the request payload for the "plugin-run" action. It runs
// one plugin action on demand, bypassing matcher and debounce: against a
// session's current snapshot when SessionID is set, or as a global action when
// it is not. Action selects which manifest action runs; empty means the
// plugin's default action (actions[0]), so old clients that never send the
// field keep their pre-multi-action behaviour. Depth carries the caller CLI's
// JIN_PLUGIN_DEPTH so the dispatcher can reject a chained plugin run.
// CallerTmuxSocket/CallerTmuxPane carry the invoking CLI's tmux context so the
// plugin can address the pane it was launched from.
type PluginRunRequest struct {
	Plugin           string `json:"plugin"`
	Action           string `json:"action,omitempty"`
	SessionID        string `json:"session_id,omitempty"`
	Depth            int    `json:"depth,omitempty"`
	CallerTmuxSocket string `json:"caller_tmux_socket,omitempty"`
	CallerTmuxPane   string `json:"caller_tmux_pane,omitempty"`
}

// handlePluginRun checks Plugin and the dispatcher before touching the
// session store, so validation errors never depend on manager state. A success
// Response only means the run was accepted — the plugin executes asynchronously
// and its outcome is followed through the plugin log.
func (s *Server) handlePluginRun(data json.RawMessage) Response {
	var req PluginRunRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.Plugin == "" {
		return Response{Success: false, Error: "plugin is required"}
	}
	if s.pluginDisp == nil {
		return Response{Success: false, Error: "plugins are not enabled"}
	}

	ev := plugin.Event{Name: "action"}
	if req.SessionID != "" {
		sess, ok := s.manager.Get(req.SessionID)
		if !ok {
			return Response{Success: false, Error: fmt.Sprintf("session not found: %s", req.SessionID)}
		}
		ev.SessionID = sess.ID
		ev.Status = string(sess.Status)
		ev.AgentKind = sess.AgentKind
		ev.WorkDir = sess.WorkDir
		ev.TmuxPaneID = sess.TmuxPaneID
	}
	actx := plugin.ActionContext{
		TmuxSocket: req.CallerTmuxSocket,
		TmuxPane:   req.CallerTmuxPane,
	}
	if err := s.pluginDisp.RunAction(req.Plugin, req.Action, ev, req.Depth, actx); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
}
