package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/takaaki-s/claude-code-valet/internal/config"
	"github.com/takaaki-s/claude-code-valet/internal/notify"
	"github.com/takaaki-s/claude-code-valet/internal/session"
)

// Client is the daemon client
type Client struct {
	socketPath string
	hostID     string // Host identifier ("local", "ec2", "docker-dev", etc.)
}

// NewClient creates a new daemon client
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath, hostID: "local"}
}

// NewRemoteClient creates a daemon client for a remote slave
func NewRemoteClient(socketPath, hostID string) *Client {
	return &Client{socketPath: socketPath, hostID: hostID}
}

// HostID returns the host identifier for this client
func (c *Client) HostID() string {
	return c.hostID
}

// IsRunning checks if the daemon is running
func (c *Client) IsRunning() bool {
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (c *Client) send(req Request) (*Response, error) {
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("daemon not running. Start with: ccvalet daemon")
	}
	defer conn.Close()

	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(req); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(conn)
	var resp Response
	if err := decoder.Decode(&resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// NewOptions contains options for creating a new session
type NewOptions struct {
	Name    string
	WorkDir string
	Start   bool
	HostID  string // Target host (empty = "local")
	Fleet   string // Fleet name for session grouping
}

// New creates a new session
func (c *Client) New(name, workDir string, start bool) (*session.Info, error) {
	return c.NewWithOptions(NewOptions{
		Name:    name,
		WorkDir: workDir,
		Start:   start,
	})
}

// NewWithOptions creates a new session with full options
func (c *Client) NewWithOptions(opts NewOptions) (*session.Info, error) {
	data, _ := json.Marshal(NewRequest{
		Name:        opts.Name,
		WorkDir:     opts.WorkDir,
		Start:       opts.Start,
		HostID:      opts.HostID,
		SSHAuthSock: os.Getenv("SSH_AUTH_SOCK"),
		Fleet:       opts.Fleet,
	})

	resp, err := c.send(Request{Action: "new", Data: data})
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

// Get retrieves a single session by ID, optionally on a remote host
func (c *Client) Get(id string, hostID string) (*session.Info, error) {
	data, _ := json.Marshal(IDRequest{ID: id, HostID: hostID})

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

// Send sends a prompt to a session, optionally on a remote host
func (c *Client) Send(id, prompt, hostID string) error {
	data, _ := json.Marshal(SendRequest{ID: id, Prompt: prompt, HostID: hostID})
	resp, err := c.send(Request{Action: "send", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
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

// ListWithHostID lists sessions on the slave, passing visited to prevent routing loops,
// and tags each result with this client's HostID.
func (c *Client) ListWithHostID(visited []string) ([]session.Info, error) {
	resp, err := c.send(Request{Action: "list", Visited: visited})
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
	for i := range sessions {
		sessions[i].HostID = c.hostID
	}
	return sessions, nil
}

// Start starts a session
func (c *Client) Start(id string, hostID string) error {
	data, _ := json.Marshal(IDRequest{ID: id, HostID: hostID})
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
func (c *Client) Kill(id string, hostID string) error {
	data, _ := json.Marshal(IDRequest{ID: id, HostID: hostID})
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
func (c *Client) Delete(id, hostID string, removeWorktree, forceRemoveWorktree bool) error {
	data, _ := json.Marshal(DeleteRequest{ID: id, HostID: hostID, RemoveWorktree: removeWorktree, ForceRemoveWorktree: forceRemoveWorktree})
	resp, err := c.send(Request{Action: "delete", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		if strings.Contains(resp.Error, session.ErrWorktreeDirty.Error()) {
			return session.ErrWorktreeDirty
		}
		return errors.New(resp.Error)
	}
	return nil
}

// Stop stops the daemon
func (c *Client) Stop() error {
	resp, err := c.send(Request{Action: "stop"})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// SendHook sends a Claude Code hook event to the daemon
func (c *Client) SendHook(req HookRequest) error {
	data, _ := json.Marshal(req)
	resp, err := c.send(Request{Action: "hook", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// NotificationHistory retrieves the notification history
func (c *Client) NotificationHistory() ([]notify.Entry, error) {
	resp, err := c.send(Request{Action: "notification-history"})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, errors.New(resp.Error)
	}

	var entries []notify.Entry
	if err := json.Unmarshal(resp.Data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// NotificationHistoryWithHostID retrieves notification history from the slave, passing visited
// to prevent routing loops, and tags each entry with this client's HostID.
func (c *Client) NotificationHistoryWithHostID(visited []string) ([]notify.Entry, error) {
	resp, err := c.send(Request{Action: "notification-history", Visited: visited})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, errors.New(resp.Error)
	}
	var entries []notify.Entry
	if err := json.Unmarshal(resp.Data, &entries); err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].HostID = c.hostID
	}
	return entries, nil
}

// DirHistory retrieves directory usage history
func (c *Client) DirHistory(hostID string, maxEntries int) ([]config.DirHistoryEntry, error) {
	data, _ := json.Marshal(struct {
		HostID     string `json:"host_id"`
		MaxEntries int    `json:"max_entries"`
	}{HostID: hostID, MaxEntries: maxEntries})

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
func (c *Client) RemoveDirHistory(hostID, path string) error {
	data, _ := json.Marshal(struct {
		HostID string `json:"host_id"`
		Path   string `json:"path"`
	}{HostID: hostID, Path: path})

	resp, err := c.send(Request{Action: "remove-dir-history", Data: data})
	if err != nil {
		return err
	}
	if !resp.Success {
		return errors.New(resp.Error)
	}
	return nil
}

// SendRaw sends a raw JSON request and returns the raw JSON response.
// Implements host.SlaveClient interface for use in forwardToHost.
func (c *Client) SendRaw(action string, data, visited []byte) ([]byte, error) {
	req := Request{Action: action}
	if data != nil {
		req.Data = json.RawMessage(data)
	}
	if visited != nil {
		if err := json.Unmarshal(visited, &req.Visited); err != nil {
			return nil, fmt.Errorf("invalid visited JSON: %w", err)
		}
	}

	resp, err := c.send(req)
	if err != nil {
		return nil, err
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// ListHosts retrieves the list of hosts
func (c *Client) ListHosts() ([]HostInfo, error) {
	resp, err := c.send(Request{Action: "list-hosts"})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, errors.New(resp.Error)
	}
	var hosts []HostInfo
	if err := json.Unmarshal(resp.Data, &hosts); err != nil {
		return nil, err
	}
	return hosts, nil
}
