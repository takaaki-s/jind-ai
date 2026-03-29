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
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/takaaki-s/claude-code-valet/internal/config"
	"github.com/takaaki-s/claude-code-valet/internal/debug"
	"github.com/takaaki-s/claude-code-valet/internal/host"
	"github.com/takaaki-s/claude-code-valet/internal/notify"
	"github.com/takaaki-s/claude-code-valet/internal/session"
	"github.com/takaaki-s/claude-code-valet/internal/tmux"
	"github.com/takaaki-s/claude-code-valet/internal/transcript"
	"github.com/takaaki-s/claude-code-valet/internal/tunnel"
)

var debugLog = debug.NewLogger("daemon-debug.log")

const remoteReconnectInterval = 10 * time.Second

// Server is the daemon server
type Server struct {
	socketPath     string
	hostID         string // This daemon's host ID (e.g., "mac", "ec2"; default "local")
	manager        *session.Manager
	configMgr      *config.Manager
	stateMgr       *config.StateManager
	listener       net.Listener
	createMu       sync.Mutex      // Mutual exclusion for session creation
	hostRegistry   *host.Registry  // Multi-host management
	tunnelMgr      *tunnel.Manager // SSH tunnel management
	stopPoll       chan struct{}   // Signal to stop background goroutines; initialized once in initRemoteSlaves, never reassigned
	reconnectingMu sync.Mutex      // Protects reconnecting map
	reconnecting   map[string]bool // Tracks hosts with a reconnect goroutine in progress
}

// Message types
type Request struct {
	Action string          `json:"action"`
	Data   json.RawMessage `json:"data,omitempty"`
	// Visited tracks host IDs that have already processed this request.
	// Used by forwardToHost (targeted routing) and handleList/handleNotificationHistory
	// (aggregation) to prevent routing loops in bidirectional daemon topologies.
	Visited []string `json:"visited,omitempty"`
}

type Response struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// NewServer creates a new daemon server
func NewServer(socketPath, dataDir, configDir, hostID string) (*Server, error) {
	configMgr, err := config.NewManager(configDir)
	if err != nil {
		return nil, err
	}

	stateMgr, err := config.NewStateManager(configDir)
	if err != nil {
		return nil, err
	}

	mgr, err := session.NewManager(dataDir, configDir, configMgr)
	if err != nil {
		return nil, err
	}

	// Set up tmux client if tmux is available and ccvalet tmux session exists
	if tc, err := tmux.NewClient(); err == nil {
		if tc.HasSession(tmux.SessionName) {
			mgr.SetTmuxClient(tc)
			mgr.RecoverTmuxSessions()
			debugLog("tmux client initialized (session: %s)", tmux.SessionName)
		}
	}

	if hostID == "" {
		hostID = "local"
	}

	s := &Server{
		socketPath:   socketPath,
		hostID:       hostID,
		manager:      mgr,
		configMgr:    configMgr,
		stateMgr:     stateMgr,
		reconnecting: make(map[string]bool),
	}

	// Initialize multi-host support (always create registry for peer registration)
	hosts := configMgr.GetHosts()
	s.tunnelMgr = tunnel.NewManager()
	s.hostRegistry = host.NewRegistry(hosts)
	if len(hosts) > 0 {
		s.initRemoteSlaves()
	}

	return s, nil
}

// Start starts the daemon server
func (s *Server) Start() error {
	// Remove existing socket
	os.Remove(s.socketPath)

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0755); err != nil {
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
	// Stop background goroutines (remote notifications and connection watcher)
	if s.stopPoll != nil {
		close(s.stopPoll)
	}

	// Clean up tunnels
	if s.tunnelMgr != nil {
		s.tunnelMgr.CloseAll()
	}

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
		return s.handleList(req.Visited)
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
	case "list-hosts":
		return s.handleListHosts()
	case "hook":
		return s.handleHook(req.Data)
	case "notification-history":
		return s.handleNotificationHistory(req.Visited)
	case "dir-history":
		return s.handleDirHistory(req.Data)
	case "remove-dir-history":
		return s.handleRemoveDirHistory(req.Data)
	default:
		return Response{Success: false, Error: fmt.Sprintf("unknown action: %s", req.Action)}
	}
}

// HookRequest represents a Claude Code hook event
type HookRequest struct {
	SessionID        string `json:"session_id"`
	CcvaletSessionID string `json:"ccvalet_session_id,omitempty"`
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
	s.manager.HandleHookEvent(req.SessionID, req.CcvaletSessionID, req.HookEventName, req.NotificationType, req.CWD, req.StopReason)
	return Response{Success: true}
}

func (s *Server) handleNotificationHistory(visited []string) Response {
	// Copy and add self to visited (avoid mutating caller's backing array)
	visited = append(append([]string(nil), visited...), s.hostID)

	// Get local notification history
	localEntries := s.manager.NotificationHistory()
	for i := range localEntries {
		if localEntries[i].HostID == "" {
			localEntries[i].HostID = s.hostID
		}
	}

	// Return only local if no host registry
	if s.hostRegistry == nil {
		data, _ := json.Marshal(localEntries)
		return Response{Success: true, Data: data}
	}

	// Fetch from all reachable hosts in parallel, skipping visited
	allEntries := localEntries
	reachable := s.hostRegistry.AllReachable()

	var targets []*host.Host
	for _, h := range reachable {
		if !slices.Contains(visited, h.ID) && h.Client != nil {
			targets = append(targets, h)
		}
	}

	if len(targets) > 0 {
		type remoteResult struct {
			entries []notify.Entry
			err     error
			hostID  string
		}

		results := make(chan remoteResult, len(targets))
		for _, h := range targets {
			go func(rh *host.Host) {
				entries, err := rh.Client.NotificationHistoryWithHostID(visited)
				results <- remoteResult{entries: entries, err: err, hostID: rh.ID}
			}(h)
		}

		for range targets {
			result := <-results
			if result.err != nil {
				debugLog("[REMOTE] Failed to get notification history from %s: %v", result.hostID, result.err)
				continue
			}
			allEntries = append(allEntries, result.entries...)
		}
	}

	// Sort by Timestamp descending
	sort.Slice(allEntries, func(i, j int) bool {
		return allEntries[i].Timestamp.After(allEntries[j].Timestamp)
	})

	data, _ := json.Marshal(allEntries)
	return Response{Success: true, Data: data}
}

type NewRequest struct {
	Name        string `json:"name"`
	WorkDir     string `json:"work_dir"`
	Start       bool   `json:"start"`
	HostID      string `json:"host_id,omitempty"`       // Target host (empty = "local")
	SSHAuthSock string `json:"ssh_auth_sock,omitempty"` // SSH_AUTH_SOCK (for git operations)
	Fleet       string `json:"fleet"`                   // Fleet name for session grouping
}

func (s *Server) handleNew(data json.RawMessage) Response {
	var req NewRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Forward to the corresponding slave if destined for a remote host
	if req.HostID != "" && req.HostID != "local" {
		targetHostID := req.HostID
		// Clear SSH_AUTH_SOCK: local socket path doesn't exist on remote host.
		// The slave uses its own SSH_AUTH_SOCK from the SSH tunnel.
		req.SSHAuthSock = ""
		// Clear HostID so the slave handles the request locally instead of
		// trying to forward it again (which would fail or loop).
		req.HostID = ""
		forwardData, _ := json.Marshal(req)
		return s.forwardToHost(targetHostID, Request{Action: "new", Data: forwardData})
	}

	// Synchronous mode - mutual exclusion
	s.createMu.Lock()

	sess, err := s.manager.CreateWithOptions(session.CreateOptions{
		Name:    req.Name,
		WorkDir: req.WorkDir,
		Fleet:   req.Fleet,
	})
	if err != nil {
		s.createMu.Unlock()
		return Response{Success: false, Error: err.Error()}
	}

	s.createMu.Unlock()

	// Record directory usage in persistent history
	histHostID := req.HostID
	if histHostID == "" {
		histHostID = "local"
	}
	_ = s.stateMgr.RecordDirUsage(histHostID, req.WorkDir)

	// Start session in background if requested
	if req.Start {
		if err := s.manager.StartBackground(sess.ID); err != nil {
			_ = s.manager.Delete(sess.ID, false, false)
			return Response{Success: false, Error: err.Error()}
		}
	}

	respData, _ := json.Marshal(sess.ToInfo())
	return Response{Success: true, Data: respData}
}

func (s *Server) handleList(visited []string) Response {
	// Copy and add self to visited (avoid mutating caller's backing array)
	visited = append(append([]string(nil), visited...), s.hostID)

	// Get local session list
	localSessions := s.manager.List()
	for i := range localSessions {
		if localSessions[i].HostID == "" {
			localSessions[i].HostID = s.hostID
		}
	}

	// Return only local if no host registry; manager.List() is already sorted.
	if s.hostRegistry == nil {
		data, _ := json.Marshal(localSessions)
		return Response{Success: true, Data: data}
	}

	// Fetch from all reachable hosts (remotes + peers) in parallel, skipping visited
	allSessions := localSessions
	reachable := s.hostRegistry.AllReachable()

	var targets []*host.Host
	for _, h := range reachable {
		if !slices.Contains(visited, h.ID) && h.Client != nil {
			targets = append(targets, h)
		}
	}

	if len(targets) > 0 {
		type remoteResult struct {
			sessions []session.Info
			err      error
			hostID   string
		}

		results := make(chan remoteResult, len(targets))
		for _, h := range targets {
			go func(rh *host.Host) {
				sessions, err := rh.Client.ListWithHostID(visited)
				results <- remoteResult{sessions: sessions, err: err, hostID: rh.ID}
			}(h)
		}

		for range targets {
			result := <-results
			if result.err != nil {
				debugLog("[REMOTE] Failed to list from %s: %v", result.hostID, result.err)
				continue
			}
			allSessions = append(allSessions, result.sessions...)
		}
	}

	// Re-sort after merging remote sessions; manager.List() is already sorted
	// but remote results arrive in arbitrary order.
	session.SortInfos(allSessions)

	data, _ := json.Marshal(allSessions)
	return Response{Success: true, Data: data}
}

func (s *Server) handleGet(data json.RawMessage) Response {
	var req IDRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Forward to the corresponding slave if destined for a remote host.
	// Clear HostID in the forwarded payload so the slave handles it locally.
	if req.HostID != "" && req.HostID != "local" {
		fwdReq := IDRequest{ID: req.ID, HostID: ""}
		fwdData, _ := json.Marshal(fwdReq)
		return s.forwardToHost(req.HostID, Request{Action: "get", Data: fwdData})
	}

	sess, ok := s.manager.Get(req.ID)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("session not found: %s", req.ID)}
	}

	info := sess.ToInfo()
	if info.HostID == "" {
		info.HostID = "local"
	}

	// Enrich with transcript data
	reader := transcript.NewReader()
	if info.ClaudeSessionID != "" && info.WorkDir != "" {
		if msgs, err := reader.GetLastMessages(info.WorkDir, info.ClaudeSessionID); err == nil && msgs != nil {
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
	HostID string `json:"host_id,omitempty"` // Target host (empty = "local")
}

func (s *Server) handleSend(data json.RawMessage) Response {
	var req SendRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Forward to the corresponding slave if destined for a remote host.
	// Clear HostID in the forwarded payload so the slave handles it locally.
	if req.HostID != "" && req.HostID != "local" {
		fwdReq := SendRequest{ID: req.ID, Prompt: req.Prompt, HostID: ""}
		fwdData, _ := json.Marshal(fwdReq)
		return s.forwardToHost(req.HostID, Request{Action: "send", Data: fwdData})
	}

	if req.Prompt == "" {
		return Response{Success: false, Error: "prompt is required"}
	}
	if err := s.manager.SendPrompt(req.ID, req.Prompt); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
}

type IDRequest struct {
	ID     string `json:"id"`
	HostID string `json:"host_id,omitempty"` // Target host (empty = "local")
}

// DeleteRequest extends IDRequest with worktree removal options.
type DeleteRequest struct {
	ID                  string `json:"id"`
	HostID              string `json:"host_id,omitempty"`
	RemoveWorktree      bool   `json:"remove_worktree,omitempty"`
	ForceRemoveWorktree bool   `json:"force_remove_worktree,omitempty"`
}

func (s *Server) handleStart(data json.RawMessage) Response {
	var req IDRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Forward to the corresponding slave if destined for a remote host.
	// Clear HostID in the forwarded payload so the slave handles it locally.
	if req.HostID != "" && req.HostID != "local" {
		fwdReq := IDRequest{ID: req.ID, HostID: ""}
		fwdData, _ := json.Marshal(fwdReq)
		return s.forwardToHost(req.HostID, Request{Action: "start", Data: fwdData})
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

	// Forward to the corresponding slave if destined for a remote host.
	// Clear HostID in the forwarded payload so the slave handles it locally.
	if req.HostID != "" && req.HostID != "local" {
		fwdReq := IDRequest{ID: req.ID, HostID: ""}
		fwdData, _ := json.Marshal(fwdReq)
		return s.forwardToHost(req.HostID, Request{Action: "kill", Data: fwdData})
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

	// Forward to the corresponding slave if destined for a remote host.
	// Clear HostID in the forwarded payload so the slave handles it locally.
	if req.HostID != "" && req.HostID != "local" {
		fwdReq := DeleteRequest{ID: req.ID, RemoveWorktree: req.RemoveWorktree, ForceRemoveWorktree: req.ForceRemoveWorktree}
		fwdData, _ := json.Marshal(fwdReq)
		return s.forwardToHost(req.HostID, Request{Action: "delete", Data: fwdData})
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

// RegisterPeer registers a peer daemon (connected via reverse tunnel)
func (s *Server) RegisterPeer(id, hostType string, client host.SlaveClient) {
	if s.hostRegistry == nil {
		return
	}
	s.hostRegistry.RegisterPeer(id, hostType, client)
	debugLog("[PEER] Registered peer %s (type: %s)", id, hostType)
}

// --- Multi-host support ---

// initRemoteSlaves starts slave daemons on remote hosts, establishes tunnels, and sets up daemon clients
func (s *Server) initRemoteSlaves() {
	if s.hostRegistry == nil || s.tunnelMgr == nil {
		return
	}

	// Validate hostID before using it in shell commands (reverse tunnel, bootstrap)
	if err := host.ValidateIdentifier(s.hostID); err != nil {
		debugLog("[REMOTE] Invalid host ID %q: %v", s.hostID, err)
		return
	}

	for _, h := range s.hostRegistry.Remotes() {
		if err := s.connectRemoteSlave(h); err != nil {
			debugLog("[REMOTE] Failed to connect to %s: %v", h.ID, err)
			continue
		}
		debugLog("[REMOTE] Connected to slave %s", h.ID)
	}

	// Start polling remote notification histories for desktop notifications
	s.stopPoll = make(chan struct{})
	go s.pollRemoteNotifications()
	go s.watchRemoteConnections()
}

// connectRemoteSlave runs the full 3-step connection sequence for one remote host:
// start slave daemon, open tunnel, register client.
// Called during initial setup and reconnect. StartSlave is idempotent;
// tunnelMgr.Open returns the existing socket if the tunnel is still alive.
func (s *Server) connectRemoteSlave(h *host.Host) error {
	peerSocketPath := filepath.Join(tunnel.PeerSocketDir, s.hostID, "daemon.sock")
	bootstrapOpts := host.BootstrapOptions{
		PeerSocketPath: peerSocketPath,
		PeerHostID:     s.hostID,
	}
	tunnelOpts := tunnel.TunnelOptions{
		ReverseEnabled:    true,
		LocalHostID:       s.hostID,
		LocalDaemonSocket: s.socketPath,
	}
	if err := host.StartSlave(h.Config, bootstrapOpts); err != nil {
		return fmt.Errorf("start slave: %w", err)
	}
	localSocket, err := s.tunnelMgr.Open(h.Config, tunnelOpts)
	if err != nil {
		return fmt.Errorf("open tunnel: %w", err)
	}
	s.hostRegistry.SetClient(h.ID, NewRemoteClient(localSocket, h.ID))
	return nil
}

// watchRemoteConnections periodically checks SSH tunnel liveness and reconnects
// any dead tunnels. Runs until stopPoll is closed.
func (s *Server) watchRemoteConnections() {
	ticker := time.NewTicker(remoteReconnectInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopPoll:
			return
		case <-ticker.C:
		}
		s.reconnectDeadTunnels()
	}
}

// reconnectDeadTunnels checks all configured SSH remote hosts and reconnects
// any whose tunnel is no longer alive. Docker hosts are skipped (no process to restart).
// At most one reconnect goroutine runs per host at a time.
func (s *Server) reconnectDeadTunnels() {
	for _, h := range s.hostRegistry.Remotes() {
		if h.Type != "ssh" {
			continue
		}
		if s.tunnelMgr.IsAlive(h.ID) {
			continue
		}
		s.reconnectingMu.Lock()
		if s.reconnecting[h.ID] {
			s.reconnectingMu.Unlock()
			continue
		}
		s.reconnecting[h.ID] = true
		s.reconnectingMu.Unlock()

		debugLog("[REMOTE] Tunnel to %s is dead, attempting reconnect", h.ID)
		go func(rh *host.Host) {
			defer func() {
				s.reconnectingMu.Lock()
				delete(s.reconnecting, rh.ID)
				s.reconnectingMu.Unlock()
			}()
			if err := s.connectRemoteSlave(rh); err != nil {
				debugLog("[REMOTE] Reconnect to %s failed: %v", rh.ID, err)
				return
			}
			debugLog("[REMOTE] Reconnected to %s", rh.ID)
		}(h)
	}
}

// pollRemoteNotifications periodically fetches notification histories from remote
// slaves and fires local desktop notifications for any new entries.
func (s *Server) pollRemoteNotifications() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// Track the latest timestamp seen per host to detect new entries
	lastSeen := make(map[string]time.Time)

	for {
		select {
		case <-s.stopPoll:
			return
		case <-ticker.C:
		}

		s.pollRemoteOnce(lastSeen)
	}
}

// pollRemoteOnce performs a single iteration of remote notification polling.
// It fetches notification histories from all remote slaves, compares against
// lastSeen timestamps, and sends local desktop notifications for new entries.
func (s *Server) pollRemoteOnce(lastSeen map[string]time.Time) {
	// Pass the master's own hostID so slaves don't query the master back,
	// preventing infinite loops in bidirectional topologies.
	pollVisited := []string{s.hostID}
	for _, h := range s.hostRegistry.AllReachable() {
		if h.Client == nil {
			continue
		}
		entries, err := h.Client.NotificationHistoryWithHostID(pollVisited)
		if err != nil {
			continue
		}
		cutoff := lastSeen[h.ID]
		for _, entry := range entries {
			if !entry.Timestamp.After(cutoff) {
				continue
			}
			// New entry — send local desktop notification
			switch entry.Type {
			case "permission":
				s.manager.NotifyDesktop("Permission Required", entry.Message)
			case "task_complete":
				s.manager.NotifyDesktop("Task Complete", entry.Message)
			case "error":
				s.manager.NotifyDesktop("Error", entry.Message)
			}
			if entry.Timestamp.After(lastSeen[h.ID]) {
				lastSeen[h.ID] = entry.Timestamp
			}
		}
	}
}

// forwardToHost forwards a request to a remote or peer host using visited-based routing
func (s *Server) forwardToHost(hostID string, req Request) Response {
	if s.hostRegistry == nil {
		return Response{Success: false, Error: "host registry not initialized"}
	}

	// Check for routing loop
	if slices.Contains(req.Visited, hostID) {
		return Response{Success: false, Error: "routing loop detected"}
	}

	h, ok := s.hostRegistry.Get(hostID)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("unknown host: %s", hostID)}
	}

	if h.Client == nil {
		return Response{Success: false, Error: fmt.Sprintf("host %s not connected", hostID)}
	}

	// Add self to visited list before forwarding (copy to avoid mutating caller's slice)
	visited := make([]string, len(req.Visited)+1)
	copy(visited, req.Visited)
	visited[len(req.Visited)] = s.hostID
	req.Visited = visited

	// Use SlaveClient interface — no type assertion needed
	visitedJSON, _ := json.Marshal(req.Visited)
	rawResp, err := h.Client.SendRaw(req.Action, req.Data, visitedJSON)
	if err != nil {
		return Response{Success: false, Error: fmt.Sprintf("failed to forward to %s: %v", hostID, err)}
	}

	var resp Response
	if err := json.Unmarshal(rawResp, &resp); err != nil {
		return Response{Success: false, Error: fmt.Sprintf("failed to decode response from %s: %v", hostID, err)}
	}
	return resp
}

// --- Host info queries ---

// HostInfo represents host information
type HostInfo struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Connected bool   `json:"connected"`
	IsPeer    bool   `json:"is_peer,omitempty"`
}

func (s *Server) handleListHosts() Response {
	hosts := []HostInfo{
		{ID: s.hostID, Type: "local", Connected: true},
	}

	if s.hostRegistry != nil {
		for _, h := range s.hostRegistry.AllReachable() {
			connected := h.Client != nil && h.Client.IsRunning()
			hosts = append(hosts, HostInfo{
				ID:        h.ID,
				Type:      h.Type,
				Connected: connected,
				IsPeer:    h.IsPeer,
			})
		}
	}

	data, _ := json.Marshal(hosts)
	return Response{Success: true, Data: data}
}

func (s *Server) handleDirHistory(data json.RawMessage) Response {
	var req struct {
		HostID     string `json:"host_id"`
		MaxEntries int    `json:"max_entries"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.HostID == "" {
		req.HostID = "local"
	}
	if req.MaxEntries <= 0 {
		req.MaxEntries = 5
	}

	// If request is for a remote host, forward to the slave daemon.
	// The slave stores its history as HostID="local", so clear HostID before forwarding.
	if req.HostID != "local" {
		forwardReq := struct {
			HostID     string `json:"host_id"`
			MaxEntries int    `json:"max_entries"`
		}{HostID: "local", MaxEntries: req.MaxEntries}
		forwardData, _ := json.Marshal(forwardReq)
		return s.forwardToHost(req.HostID, Request{Action: "dir-history", Data: forwardData})
	}

	entries := s.stateMgr.GetDirHistory(req.HostID, req.MaxEntries)
	respData, _ := json.Marshal(entries)
	return Response{Success: true, Data: respData}
}

func (s *Server) handleRemoveDirHistory(data json.RawMessage) Response {
	var req struct {
		HostID string `json:"host_id"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	if req.HostID == "" {
		req.HostID = "local"
	}
	if err := s.stateMgr.RemoveDirHistory(req.HostID, req.Path); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	return Response{Success: true}
}

// replaceEnv replaces or appends an environment variable in the given env slice.
func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
