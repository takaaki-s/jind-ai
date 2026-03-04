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
	"github.com/takaaki-s/claude-code-valet/internal/host"
	"github.com/takaaki-s/claude-code-valet/internal/notify"
	"github.com/takaaki-s/claude-code-valet/internal/session"
	"github.com/takaaki-s/claude-code-valet/internal/tmux"
	"github.com/takaaki-s/claude-code-valet/internal/tunnel"
)

// debugEnabled controls debug logging output
var debugEnabled = os.Getenv("CCVALET_DEBUG") == "1"
var debugLogPath string

func init() {
	if debugEnabled {
		home, _ := os.UserHomeDir()
		debugLogPath = filepath.Join(home, ".ccvalet", "daemon-debug.log")
	}
}

func debugLog(format string, args ...interface{}) {
	if !debugEnabled || debugLogPath == "" {
		return
	}
	f, err := os.OpenFile(debugLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("15:04:05"), msg)
}

// Server is the daemon server
type Server struct {
	socketPath   string
	manager      *session.Manager
	configMgr    *config.Manager
	stateMgr     *config.StateManager
	listener     net.Listener
	createMu     sync.Mutex      // セッション作成の排他制御用
	hostRegistry *host.Registry  // マルチホスト管理
	tunnelMgr    *tunnel.Manager // SSHトンネル管理
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

	// マルチホスト対応の初期化
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
	// トンネルをクリーンアップ
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
	encoder.Encode(resp)
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
	// ローカルの通知履歴を取得
	localEntries := s.manager.NotificationHistory()
	for i := range localEntries {
		if localEntries[i].HostID == "" {
			localEntries[i].HostID = "local"
		}
	}

	// リモートホストがない場合はローカルのみ返す
	if s.hostRegistry == nil {
		data, _ := json.Marshal(localEntries)
		return Response{Success: true, Data: data}
	}

	// リモートの通知履歴を並列取得して統合
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

	// Timestamp降順でソート
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
	HostID      string `json:"host_id,omitempty"`       // 対象ホスト（空="local"）
	SSHAuthSock string `json:"ssh_auth_sock,omitempty"` // SSH_AUTH_SOCK（git操作用）
}

func (s *Server) handleNew(data json.RawMessage) Response {
	var req NewRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// リモートホスト宛の場合、該当slaveに転送
	if req.HostID != "" && req.HostID != "local" {
		// Clear SSH_AUTH_SOCK: local socket path doesn't exist on remote host.
		// The slave uses its own SSH_AUTH_SOCK from the SSH tunnel.
		req.SSHAuthSock = ""
		forwardData, _ := json.Marshal(req)
		return s.forwardToSlave(req.HostID, Request{Action: "new", Data: forwardData})
	}

	// 同期モード - 排他制御
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

	// Start session in background if requested
	if req.Start {
		if err := s.manager.StartBackground(sess.ID); err != nil {
			s.manager.Delete(sess.ID)
			return Response{Success: false, Error: err.Error()}
		}
	}

	respData, _ := json.Marshal(sess.ToInfo())
	return Response{Success: true, Data: respData}
}

func (s *Server) handleList() Response {
	// ローカルセッション一覧を取得
	localSessions := s.manager.List()
	for i := range localSessions {
		if localSessions[i].HostID == "" {
			localSessions[i].HostID = "local"
		}
	}

	// リモートホストがない場合はローカルのみ返す
	if s.hostRegistry == nil {
		data, _ := json.Marshal(localSessions)
		return Response{Success: true, Data: data}
	}

	// リモートセッション一覧を並列取得して統合
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
	HostID string `json:"host_id,omitempty"` // 対象ホスト（空="local"）
}

func (s *Server) handleStart(data json.RawMessage) Response {
	var req IDRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return Response{Success: false, Error: err.Error()}
	}

	// リモートホスト宛の場合、該当slaveに転送
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

	// リモートホスト宛の場合、該当slaveに転送
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

	// リモートホスト宛の場合、該当slaveに転送
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

// --- マルチホスト対応 ---

// initRemoteSlaves はリモートホストのSlaveデーモンを起動し、トンネルを確立し、daemon clientを設定する
func (s *Server) initRemoteSlaves() {
	if s.hostRegistry == nil || s.tunnelMgr == nil {
		return
	}

	for _, h := range s.hostRegistry.Remotes() {
		// Step 1: Slaveデーモンを自動起動（冪等: 既に起動済みなら何もしない）
		if err := host.StartSlave(h.Config); err != nil {
			debugLog("[REMOTE] Failed to start slave on %s: %v", h.ID, err)
			continue
		}
		debugLog("[REMOTE] Slave started on %s", h.ID)

		// Step 2: SSHトンネル/Docker接続を確立
		localSocket, err := s.tunnelMgr.Open(h.Config)
		if err != nil {
			debugLog("[REMOTE] Failed to open tunnel to %s: %v", h.ID, err)
			continue
		}

		// Step 3: RemoteClientを作成して登録
		client := NewRemoteClient(localSocket, h.ID)
		s.hostRegistry.SetClient(h.ID, client)
		debugLog("[REMOTE] Connected to slave %s via %s", h.ID, localSocket)
	}
}

// forwardToSlave はリクエストをリモートslaveに転送する
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

	// SlaveClientインターフェースから*Clientにキャスト
	client, ok := h.Client.(*Client)
	if !ok {
		return Response{Success: false, Error: fmt.Sprintf("host %s has incompatible client type", hostID)}
	}

	// host_idをリクエストデータから除去してスレーブに転送する
	// スレーブがhost_idを見て再度forwardしようとするのを防ぐ
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

// --- ホスト情報クエリ ---

// HostInfo はホスト情報を表す
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
