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
	"github.com/takaaki-s/claude-code-valet/internal/tunnel"
)

var debugLog = debug.NewLogger("daemon-debug.log")

// Server is the daemon server
type Server struct {
	socketPath   string
	manager      *session.Manager
	configMgr    *config.Manager
	stateMgr     *config.StateManager
	listener     net.Listener
	createMu     sync.Mutex      // Mutual exclusion for session creation
	hostRegistry *host.Registry  // Multi-host management
	tunnelMgr    *tunnel.Manager // SSH tunnel management
	stopPoll     chan struct{}    // Signal to stop remote notification polling
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

// NewServer creates a new daemon server
func NewServer(socketPath, dataDir, configDir string) (*Server, error) {
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

	s := &Server{
		socketPath: socketPath,
		manager:    mgr,
		configMgr:  configMgr,
		stateMgr:   stateMgr,
	}

	// Initialize multi-host support
	hosts := configMgr.GetHosts()
	if len(hosts) > 0 {
		s.tunnelMgr = tunnel.NewManager()
		s.hostRegistry = host.NewRegistry(hosts)
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
	// Stop remote notification polling
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
		return s.handleList()
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
		return s.handleNotificationHistory()
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
}

func (s *Server) handleHook(data json.RawMessage) Response {
	var req HookRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}
	s.manager.HandleHookEvent(req.SessionID, req.CcvaletSessionID, req.HookEventName, req.NotificationType, req.CWD)
	return Response{Success: true}
}

func (s *Server) handleNotificationHistory() Response {
	// Get local notification history
	localEntries := s.manager.NotificationHistory()
	for i := range localEntries {
		if localEntries[i].HostID == "" {
			localEntries[i].HostID = "local"
		}
	}

	// Return only local if no remote hosts
	if s.hostRegistry == nil {
		data, _ := json.Marshal(localEntries)
		return Response{Success: true, Data: data}
	}

	// Fetch remote notification history in parallel and merge
	allEntries := localEntries
	remotes := s.hostRegistry.Remotes()

	if len(remotes) > 0 {
		type remoteResult struct {
			entries []notify.Entry
			err     error
			hostID  string
		}

		results := make(chan remoteResult, len(remotes))
		for _, h := range remotes {
			go func(rh *host.Host) {
				if rh.Client == nil {
					results <- remoteResult{hostID: rh.ID, err: fmt.Errorf("not connected")}
					return
				}
				entries, err := rh.Client.NotificationHistoryWithHostID()
				results <- remoteResult{entries: entries, err: err, hostID: rh.ID}
			}(h)
		}

		for range remotes {
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
}

func (s *Server) handleNew(data json.RawMessage) Response {
	var req NewRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Forward to the corresponding slave if destined for a remote host
	if req.HostID != "" && req.HostID != "local" {
		// Clear SSH_AUTH_SOCK: local socket path doesn't exist on remote host.
		// The slave uses its own SSH_AUTH_SOCK from the SSH tunnel.
		req.SSHAuthSock = ""
		forwardData, _ := json.Marshal(req)
		return s.forwardToSlave(req.HostID, Request{Action: "new", Data: forwardData})
	}

	// Synchronous mode - mutual exclusion
	s.createMu.Lock()

	sess, err := s.manager.CreateWithOptions(session.CreateOptions{
		Name:    req.Name,
		WorkDir: req.WorkDir,
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
			_ = s.manager.Delete(sess.ID)
			return Response{Success: false, Error: err.Error()}
		}
	}

	respData, _ := json.Marshal(sess.ToInfo())
	return Response{Success: true, Data: respData}
}

func (s *Server) handleList() Response {
	// Get local session list
	localSessions := s.manager.List()
	for i := range localSessions {
		if localSessions[i].HostID == "" {
			localSessions[i].HostID = "local"
		}
	}

	// Return only local if no remote hosts
	if s.hostRegistry == nil {
		data, _ := json.Marshal(localSessions)
		return Response{Success: true, Data: data}
	}

	// Fetch remote session list in parallel and merge
	allSessions := localSessions
	remotes := s.hostRegistry.Remotes()

	if len(remotes) > 0 {
		type remoteResult struct {
			sessions []session.Info
			err      error
			hostID   string
		}

		results := make(chan remoteResult, len(remotes))
		for _, h := range remotes {
			go func(rh *host.Host) {
				if rh.Client == nil {
					results <- remoteResult{hostID: rh.ID, err: fmt.Errorf("not connected")}
					return
				}
				sessions, err := rh.Client.ListWithHostID()
				results <- remoteResult{sessions: sessions, err: err, hostID: rh.ID}
			}(h)
		}

		for range remotes {
			result := <-results
			if result.err != nil {
				debugLog("[REMOTE] Failed to list from %s: %v", result.hostID, result.err)
				continue
			}
			allSessions = append(allSessions, result.sessions...)
		}
	}

	data, _ := json.Marshal(allSessions)
	return Response{Success: true, Data: data}
}

type IDRequest struct {
	ID     string `json:"id"`
	HostID string `json:"host_id,omitempty"` // Target host (empty = "local")
}

func (s *Server) handleStart(data json.RawMessage) Response {
	var req IDRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Forward to the corresponding slave if destined for a remote host
	if req.HostID != "" && req.HostID != "local" {
		return s.forwardToSlave(req.HostID, Request{Action: "start", Data: data})
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

	// Forward to the corresponding slave if destined for a remote host
	if req.HostID != "" && req.HostID != "local" {
		return s.forwardToSlave(req.HostID, Request{Action: "kill", Data: data})
	}

	if err := s.manager.Kill(req.ID); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	return Response{Success: true}
}

func (s *Server) handleDelete(data json.RawMessage) Response {
	var req IDRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// Forward to the corresponding slave if destined for a remote host
	if req.HostID != "" && req.HostID != "local" {
		return s.forwardToSlave(req.HostID, Request{Action: "delete", Data: data})
	}

	if err := s.manager.Delete(req.ID); err != nil {
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

// --- Multi-host support ---

// initRemoteSlaves starts slave daemons on remote hosts, establishes tunnels, and sets up daemon clients
func (s *Server) initRemoteSlaves() {
	if s.hostRegistry == nil || s.tunnelMgr == nil {
		return
	}

	for _, h := range s.hostRegistry.Remotes() {
		// Step 1: Auto-start slave daemon (idempotent: no-op if already running)
		if err := host.StartSlave(h.Config); err != nil {
			debugLog("[REMOTE] Failed to start slave on %s: %v", h.ID, err)
			continue
		}
		debugLog("[REMOTE] Slave started on %s", h.ID)

		// Step 2: Establish SSH tunnel / Docker connection
		localSocket, err := s.tunnelMgr.Open(h.Config)
		if err != nil {
			debugLog("[REMOTE] Failed to open tunnel to %s: %v", h.ID, err)
			continue
		}

		// Step 3: Create and register RemoteClient
		client := NewRemoteClient(localSocket, h.ID)
		s.hostRegistry.SetClient(h.ID, client)
		debugLog("[REMOTE] Connected to slave %s via %s", h.ID, localSocket)
	}

	// Start polling remote notification histories for desktop notifications
	s.stopPoll = make(chan struct{})
	go s.pollRemoteNotifications()
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
	for _, h := range s.hostRegistry.Remotes() {
		if h.Client == nil {
			continue
		}
		entries, err := h.Client.NotificationHistoryWithHostID()
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
			}
			if entry.Timestamp.After(lastSeen[h.ID]) {
				lastSeen[h.ID] = entry.Timestamp
			}
		}
	}
}

// forwardToSlave forwards a request to a remote slave
func (s *Server) forwardToSlave(hostID string, req Request) Response {
	if s.hostRegistry == nil {
		return Response{Success: false, Error: "host registry not initialized"}
	}

	h, ok := s.hostRegistry.Get(hostID)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("unknown host: %s", hostID)}
	}

	if h.Client == nil {
		return Response{Success: false, Error: fmt.Sprintf("host %s not connected", hostID)}
	}

	// Cast from SlaveClient interface to *Client
	client, ok := h.Client.(*Client)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("host %s has incompatible client type", hostID)}
	}

	// Strip host_id from request data before forwarding to slave
	// Prevents the slave from trying to forward again based on host_id
	if req.Data != nil {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(req.Data, &m); err == nil {
			delete(m, "host_id")
			req.Data, _ = json.Marshal(m)
		}
	}

	resp, err := client.send(req)
	if err != nil {
		return Response{Success: false, Error: fmt.Sprintf("failed to forward to %s: %v", hostID, err)}
	}

	return *resp
}

// --- Host info queries ---

// HostInfo represents host information
type HostInfo struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Connected bool   `json:"connected"`
}

func (s *Server) handleListHosts() Response {
	hosts := []HostInfo{
		{ID: "local", Type: "local", Connected: true},
	}

	if s.hostRegistry != nil {
		for _, h := range s.hostRegistry.Remotes() {
			connected := h.Client != nil && h.Client.IsRunning()
			hosts = append(hosts, HostInfo{
				ID:        h.ID,
				Type:      h.Type,
				Connected: connected,
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
