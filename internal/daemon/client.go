package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/takaaki-s/jindaiko/internal/config"
	"github.com/takaaki-s/jindaiko/internal/notify"
	"github.com/takaaki-s/jindaiko/internal/session"
)

// Client is the daemon client
type Client struct {
	socketPath string
}

// NewClient creates a new daemon client
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
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
		return nil, fmt.Errorf("daemon not running. Start with: jin daemon")
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
