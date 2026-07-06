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
	"sync"
	"syscall"

	"github.com/takaaki-s/jindaiko/internal/agent"
	"github.com/takaaki-s/jindaiko/internal/config"
	"github.com/takaaki-s/jindaiko/internal/debug"
	"github.com/takaaki-s/jindaiko/internal/session"
	"github.com/takaaki-s/jindaiko/internal/tmux"
	"github.com/takaaki-s/jindaiko/internal/transcript"
	"github.com/takaaki-s/jindaiko/internal/worktreehook"
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
	listener   net.Listener
	createMu   sync.Mutex // Mutual exclusion for session creation
}

// Message types
type Request struct {
	Action string          `json:"action"`
	Data   json.RawMessage `json:"data,omitempty"`
}

type Response struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
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

	return &Server{
		socketPath: socketPath,
		manager:    mgr,
		configMgr:  configMgr,
		stateMgr:   stateMgr,
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

	resp := s.handleRequest(&req)
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
	case "notification-history":
		return s.handleNotificationHistory()
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

func (s *Server) handleNotificationHistory() Response {
	entries := s.manager.NotificationHistory()
	data, _ := json.Marshal(entries)
	return Response{Success: true, Data: data}
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

	if req.Prompt == "" {
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
