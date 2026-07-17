package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/debug"
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
	listener   net.Listener
	createMu   sync.Mutex // Mutual exclusion for session creation
}

// Message types
//
// ProtocolVersion travels on every request and response so a version-mismatch
// between the CLI and a running daemon (typically: jin binary updated but the
// daemon was never restarted) fails loudly with an actionable message,
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

	mgr, err := session.NewManager(sessionsDir, stateDir, configMgr)
	if err != nil {
		return nil, err
	}

	// Wire the agent resolver so startSessionTmux / HandleHookEvent can
	// dispatch to the adapter that owns each session's kind. Layer C
	// description enhancers now live behind Agent.Description() — no
	// separate wiring is needed here.
	mgr.SetAgentResolver(agentResolverAdapter{})

	// Set up tmux client if tmux is available and jin tmux session exists
	if tc, err := tmux.NewClient(); err == nil {
		if tc.HasSession(tmux.SessionName) {
			mgr.SetTmuxClient(tc)
			mgr.RecoverTmuxSessions()
			debugLog("tmux client initialized (session: %s)", tmux.SessionName)
		}
	}

	hookRunner, err := worktreehook.NewRunner(stateDir)
	if err != nil {
		return nil, fmt.Errorf("initializing worktree hook runner: %w", err)
	}
	mgr.SetHookRunner(hookRunner)

	pluginCfg := configMgr.GetPluginsConfig()
	pluginReg := plugin.NewRegistry(paths.Plugins(), stateDir, pluginCfg)
	pluginDisp := plugin.NewDispatcher(pluginReg, paths.Plugins(), stateDir, socketPath,
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
	s.listener = listener

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
		conn, err := listener.Accept()
		if err != nil {
			if s.listener == nil {
				return nil // Server stopped
			}
			log.Printf("Accept error: %v", err)
			continue
		}
		go s.handleConnection(conn)
	}
}

// Stop stops the daemon server
func (s *Server) Stop() {
	if s.listener != nil {
		s.listener.Close()
		s.listener = nil
	}
	os.Remove(s.socketPath)
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
	case "plugin-run":
		return s.handlePluginRun(req.Data)
	default:
		return Response{Success: false, Error: fmt.Sprintf("unknown action: %s", req.Action)}
	}
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

	// Synchronous mode - mutual exclusion
	s.createMu.Lock()

	sess, warning, err := s.manager.CreateWithOptions(session.CreateOptions{
		Description:    req.Description,
		WorkDir:        req.WorkDir,
		Fleet:          req.Fleet,
		AgentKind:      req.AgentKind,
		Worktree:       req.Worktree,
		WorktreeName:   req.WorktreeName,
		WorktreeBranch: req.WorktreeBranch,
		WorktreeBase:   req.WorktreeBase,
		NoHook:         req.NoHook,
	})
	if err != nil {
		s.createMu.Unlock()
		return Response{Success: false, Error: err.Error()}
	}

	s.createMu.Unlock()

	// Record directory usage in persistent history
	_ = s.stateMgr.RecordDirUsage(req.WorkDir)

	// Start session in background if requested
	if req.Start {
		if err := s.manager.StartBackground(sess.ID); err != nil {
			_ = s.manager.Delete(sess.ID, false, false)
			return Response{Success: false, Error: err.Error()}
		}
	}

	respData, _ := json.Marshal(NewResponse{Info: sess.ToInfo(), Warning: warning})
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

	sess, ok := s.manager.Get(req.ID)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("session not found: %s", req.ID)}
	}

	info := sess.ToInfo()

	// Enrich with transcript data
	reader := transcript.NewReader()
	if info.AgentSessionID != "" && info.WorkDir != "" {
		if msgs, err := reader.GetLastMessages(info.WorkDir, info.AgentSessionID); err == nil && msgs != nil {
			if msgs.User != nil {
				info.LastUserMessage = transcript.TruncateMessage(msgs.User.Content, 500)
			}
			if msgs.Assistant != nil {
				info.LastAssistantMessage = transcript.TruncateMessageFromEnd(msgs.Assistant.Content, 500)
			}
		}
	}

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

	// Reject prompts that are empty or whitespace-only. The whitespace check
	// pairs with Manager.SendPrompt's verify path: sendVerifyOK treats a
	// whitespace-only prompt as trivially accepted (nothing meaningful to
	// look for in the pane), so allowing one through here would send an
	// unverified Enter to the TUI.
	if strings.TrimSpace(req.Prompt) == "" {
		return Response{Success: false, Error: "prompt is required"}
	}
	if err := s.manager.SendPrompt(req.ID, req.Prompt); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
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

	resp := ResultResponse{
		SessionID:      info.ID,
		AgentSessionID: info.AgentSessionID,
		Entries:        []transcript.Entry{},
	}

	if info.AgentSessionID == "" {
		respData, _ := json.Marshal(resp)
		return Response{Success: true, Data: respData}
	}

	workDir := info.CurrentWorkDir
	if workDir == "" {
		workDir = info.WorkDir
	}

	reader := transcript.NewReader()
	entries, err := reader.ReadEntries(workDir, info.AgentSessionID, req.Since)
	if err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	filtered := filterResultEntries(entries, req.Tool, req.ErrorsOnly)
	if req.Last > 0 && len(filtered) > req.Last {
		filtered = filtered[len(filtered)-req.Last:]
		resp.Truncated = true
	}
	resp.Entries = filtered

	respData, _ := json.Marshal(resp)
	return Response{Success: true, Data: respData}
}

// filterResultEntries keeps entries that contain at least one block matching the
// given tool name and/or error filter. An empty tool and errorsOnly=false returns
// the input as-is. Tool name matching uses the tool_use's name; for a tool_result
// entry, name matching requires having seen a corresponding tool_use earlier in
// the input (matched by tool_use_id).
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

	if err := s.manager.Delete(req.ID, req.RemoveWorktree, req.ForceRemoveWorktree); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

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

// PanePopupRequest is the request payload for the "pane-popup" action.
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
// optional: an empty split just opens a shell in the new pane. Name enables
// the idempotent named-slot path; IfExists picks the policy when the named
// pane already exists (noop/respawn/error, empty = noop).
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
// session's current snapshot when SessionID is set, or as a global action
// (all session fields empty) when it is not. Action selects which manifest
// action runs; empty means the plugin's default action (actions[0]), so old
// clients that never send the field keep their pre-multi-action behaviour.
// Depth carries the caller CLI's JIN_PLUGIN_DEPTH so the dispatcher can
// reject a plugin that tries to chain another plugin run.
// CallerTmuxSocket/CallerTmuxPane carry the invoking CLI's tmux context
// (from $TMUX/$TMUX_PANE) so the plugin can address the pane it was
// launched from.
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
