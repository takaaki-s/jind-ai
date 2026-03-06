package tmux

import (
	"fmt"
	"os/exec"
	"strings"
)

const (
	// SocketName is the dedicated tmux socket name for ccvalet (inner tmux for CC sessions)
	SocketName = "ccvalet"

	// MgrSocketName is the tmux socket name for the outer layout manager
	MgrSocketName = "ccvalet-mgr"

	// SessionName is the tmux session name (used for the outer tmux session)
	SessionName = "ccvalet"

	// UIWindowName is the name of the window that contains the TUI + display pane
	UIWindowName = "ui"

	// WindowPrefix is prepended to session IDs for tmux window names
	WindowPrefix = "sess-"

	// SessionPrefix is prepended to session IDs for inner tmux session names
	SessionPrefix = "sess-"

	// PlaceholderCmd is the command run in the placeholder right pane
	PlaceholderCmd = "tail -f /dev/null"

	// PaneKeepTag is a custom pane option that marks managed panes (CC, TUI).
	// Panes with this tag are preserved on exit; untagged panes are auto-killed.
	PaneKeepTag = "@ccvalet-keep"
)

// Client wraps tmux CLI commands, always using the dedicated socket.
type Client struct {
	tmuxPath   string
	socketName string
	configFile string // optional: "-f <path>" passed to tmux (empty = use default ~/.tmux.conf)
}

// NewClientWithSocket creates a new tmux client with a specific socket name.
// Returns error if tmux is not found.
func NewClientWithSocket(socketName string) (*Client, error) {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return nil, fmt.Errorf("tmux not found: %w", err)
	}
	return &Client{tmuxPath: path, socketName: socketName}, nil
}

// NewClient creates a new tmux client using the default SocketName.
// Returns error if tmux is not found.
func NewClient() (*Client, error) {
	return NewClientWithSocket(SocketName)
}

// NewMgrClient creates a tmux client for the outer layout manager socket.
// Uses -f /dev/null to prevent loading user's ~/.tmux.conf, ensuring
// user key bindings (e.g., Shift+Arrow) pass through to the inner tmux.
func NewMgrClient() (*Client, error) {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return nil, fmt.Errorf("tmux not found: %w", err)
	}
	return &Client{tmuxPath: path, socketName: MgrSocketName, configFile: "/dev/null"}, nil
}

// HasTmux returns true if tmux is available on the system.
func HasTmux() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// baseArgs returns the common tmux arguments (socket, config file).
func (c *Client) baseArgs() []string {
	args := []string{"-L", c.socketName}
	if c.configFile != "" {
		args = append(args, "-f", c.configFile)
	}
	return args
}

// run executes a tmux command with the dedicated socket.
func (c *Client) run(args ...string) (string, error) {
	cmd := exec.Command(c.tmuxPath, append(c.baseArgs(), args...)...)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil && output != "" {
		return output, fmt.Errorf("%s: %w", output, err)
	}
	return output, err
}

// runSilent executes a tmux command, ignoring output.
func (c *Client) runSilent(args ...string) error {
	_, err := c.run(args...)
	return err
}

// --- Session management ---

// NewSession creates a new tmux session.
// detach=true creates the session without attaching.
func (c *Client) NewSession(name string, width, height int, detach bool) error {
	args := []string{"new-session", "-s", name, "-x", fmt.Sprintf("%d", width), "-y", fmt.Sprintf("%d", height)}
	if detach {
		args = append(args, "-d")
	}
	return c.runSilent(args...)
}

// NewSessionWithCmd creates a new detached tmux session with a named window running a shell command.
func (c *Client) NewSessionWithCmd(name string, width, height int, windowName, shellCmd string) error {
	args := []string{"new-session", "-s", name, "-x", fmt.Sprintf("%d", width), "-y", fmt.Sprintf("%d", height), "-d"}
	if windowName != "" {
		args = append(args, "-n", windowName)
	}
	if shellCmd != "" {
		args = append(args, shellCmd)
	}
	return c.runSilent(args...)
}

// NewSessionWithCmdInDir creates a new detached tmux session with a starting directory and command.
func (c *Client) NewSessionWithCmdInDir(name string, width, height int, dir, shellCmd string) error {
	args := []string{"new-session", "-s", name, "-x", fmt.Sprintf("%d", width), "-y", fmt.Sprintf("%d", height), "-d"}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	if shellCmd != "" {
		args = append(args, shellCmd)
	}
	return c.runSilent(args...)
}

// KillSession kills a tmux session.
func (c *Client) KillSession(name string) error {
	return c.runSilent("kill-session", "-t", name)
}

// HasSession checks if a session exists.
func (c *Client) HasSession(name string) bool {
	err := c.runSilent("has-session", "-t", name)
	return err == nil
}

// AttachSession attaches to an existing session (replaces current terminal).
func (c *Client) AttachSession(name string) error {
	cmd := exec.Command(c.tmuxPath, append(c.baseArgs(), "attach-session", "-t", name)...)
	cmd.Stdin = nil // will be set by caller
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// AttachCmd returns an exec.Cmd for attaching to a session.
// The caller is responsible for setting Stdin/Stdout/Stderr and calling Run().
func (c *Client) AttachCmd(name string) *exec.Cmd {
	return exec.Command(c.tmuxPath, append(c.baseArgs(), "attach-session", "-t", name)...)
}

// --- Window management ---

// NewWindow creates a new window in the given session.
// If shellCmd is non-empty, it runs that command in the window.
// detach=true creates the window without switching to it.
func (c *Client) NewWindow(session, windowName, shellCmd string, detach bool) error {
	args := []string{"new-window", "-t", session, "-n", windowName}
	if detach {
		args = append(args, "-d")
	}
	if shellCmd != "" {
		args = append(args, shellCmd)
	}
	return c.runSilent(args...)
}

// NewWindowInDir creates a new window with a specific starting directory.
func (c *Client) NewWindowInDir(session, windowName, dir, shellCmd string, detach bool) error {
	args := []string{"new-window", "-t", session, "-n", windowName}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	if detach {
		args = append(args, "-d")
	}
	if shellCmd != "" {
		args = append(args, shellCmd)
	}
	return c.runSilent(args...)
}

// SetEnvironment sets an environment variable for the tmux session.
func (c *Client) SetEnvironment(session, name, value string) error {
	return c.runSilent("set-environment", "-t", session, name, value)
}

// GetEnvironment gets an environment variable from the tmux session.
// Returns empty string if not set.
func (c *Client) GetEnvironment(session, name string) string {
	out, err := c.run("show-environment", "-t", session, name)
	if err != nil {
		return ""
	}
	// Output format: "NAME=value"
	if idx := strings.Index(out, "="); idx >= 0 {
		return out[idx+1:]
	}
	return ""
}

// UnsetEnvironment removes an environment variable from the tmux session.
func (c *Client) UnsetEnvironment(session, name string) error {
	return c.runSilent("set-environment", "-t", session, "-u", name)
}

// KillWindow kills a specific window.
func (c *Client) KillWindow(target string) error {
	return c.runSilent("kill-window", "-t", target)
}

// RenameWindow renames a window.
func (c *Client) RenameWindow(target, newName string) error {
	return c.runSilent("rename-window", "-t", target, newName)
}

// ListWindows returns the list of window names in a session.
func (c *Client) ListWindows(session string) ([]string, error) {
	out, err := c.run("list-windows", "-t", session, "-F", "#{window_name}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// RespawnPane respawns a dead pane with a new command.
func (c *Client) RespawnPane(target, shellCmd string) error {
	args := []string{"respawn-pane", "-t", target, "-k"}
	if shellCmd != "" {
		args = append(args, shellCmd)
	}
	return c.runSilent(args...)
}

// --- Pane management ---

// SplitWindow splits a window into panes.
// horizontal=true creates a left-right split.
// percent is the size of the new pane (e.g., 75 for 75%).
// shellCmd is the command to run in the new pane.
func (c *Client) SplitWindow(target string, horizontal bool, percent int, shellCmd string) error {
	args := []string{"split-window", "-t", target}
	if horizontal {
		args = append(args, "-h")
	}
	if percent > 0 {
		args = append(args, "-l", fmt.Sprintf("%d%%", percent))
	}
	if shellCmd != "" {
		args = append(args, shellCmd)
	}
	return c.runSilent(args...)
}

// SwapPane swaps two panes.
// detach=true prevents focus change.
func (c *Client) SwapPane(src, dst string, detach bool) error {
	args := []string{"swap-pane", "-s", src, "-t", dst}
	if detach {
		args = append(args, "-d")
	}
	return c.runSilent(args...)
}

// SelectPane selects (focuses) a pane.
func (c *Client) SelectPane(target string) error {
	return c.runSilent("select-pane", "-t", target)
}

// SelectPaneRight selects the pane to the right of the current pane.
func (c *Client) SelectPaneRight() error {
	return c.runSilent("select-pane", "-R")
}

// BreakPane breaks a pane out into a new window.
// detach=true keeps focus on the current window.
// windowName sets the name of the new window (empty string to use default).
func (c *Client) BreakPane(target string, detach bool, windowName string) error {
	args := []string{"break-pane", "-s", target}
	if detach {
		args = append(args, "-d")
	}
	if windowName != "" {
		args = append(args, "-n", windowName)
	}
	return c.runSilent(args...)
}

// JoinPane moves a pane from one window to another.
// horizontal=true joins side by side.
// percent is the size of the joined pane.
// before=true places the pane before (to the left of) the target.
// full=true spans the full window width/height (not just the target pane).
func (c *Client) JoinPane(src, dst string, horizontal bool, percent int, before bool, full bool) error {
	args := []string{"join-pane", "-s", src, "-t", dst}
	if horizontal {
		args = append(args, "-h")
	}
	if before {
		args = append(args, "-b")
	}
	if full {
		args = append(args, "-f")
	}
	if percent > 0 {
		args = append(args, "-l", fmt.Sprintf("%d%%", percent))
	}
	return c.runSilent(args...)
}

// --- I/O ---

// SendKeys sends keystrokes to a pane.
func (c *Client) SendKeys(target, keys string) error {
	return c.runSilent("send-keys", "-t", target, keys)
}

// SendKeysLiteral sends literal text to a pane (no key name lookup).
func (c *Client) SendKeysLiteral(target, text string) error {
	return c.runSilent("send-keys", "-t", target, "-l", text)
}

// CapturePane captures the visible content of a pane.
// If ansi=true, includes ANSI escape sequences.
func (c *Client) CapturePane(target string, ansi bool) (string, error) {
	args := []string{"capture-pane", "-p", "-t", target}
	if ansi {
		args = append(args, "-e")
	}
	return c.run(args...)
}

// PipePane pipes pane output to a shell command.
// If cmd is empty, stops piping.
func (c *Client) PipePane(target, cmd string) error {
	args := []string{"pipe-pane", "-o", "-t", target}
	if cmd != "" {
		args = append(args, cmd)
	}
	return c.runSilent(args...)
}

// --- Options ---

// SetOption sets a tmux option.
// global=true sets it as a server-wide option.
func (c *Client) SetOption(option, value string, global bool) error {
	args := []string{"set-option"}
	if global {
		args = append(args, "-g")
	}
	args = append(args, option, value)
	return c.runSilent(args...)
}

// SetWindowOption sets a window option on a target.
func (c *Client) SetWindowOption(target, option, value string) error {
	return c.runSilent("set-window-option", "-t", target, option, value)
}

// SetPaneOption sets a pane-level option on a target (requires tmux 3.0+).
func (c *Client) SetPaneOption(target, option, value string) error {
	return c.runSilent("set-option", "-p", "-t", target, option, value)
}

// SetHook sets a tmux hook.
// global=true sets the hook at the server level.
func (c *Client) SetHook(name, command string, global bool) error {
	args := []string{"set-hook"}
	if global {
		args = append(args, "-g")
	}
	args = append(args, name, command)
	return c.runSilent(args...)
}

// SetupAutoCleanDeadPanes installs a pane-died hook that automatically kills
// dead panes unless they have the PaneKeepTag custom option set.
// This allows CC and TUI panes to stay for respawning, while user-added panes
// are cleaned up immediately on shell exit.
func (c *Client) SetupAutoCleanDeadPanes() error {
	return c.SetHook("pane-died",
		`if-shell -F "#{`+PaneKeepTag+`}" "" "kill-pane"`, true)
}

// TagManagedPane marks a pane with PaneKeepTag so it is preserved on exit.
// Also sets remain-on-exit at the pane level, so only managed panes stay
// after process exit. User-added panes (untagged) are immediately destroyed.
func (c *Client) TagManagedPane(target string) error {
	c.SetPaneOption(target, "remain-on-exit", "on")
	return c.SetPaneOption(target, PaneKeepTag, "1")
}

// --- Keybindings ---

// BindKey binds a key without prefix requirement.
// cmdArgs are the tmux command and its arguments (e.g., "select-pane", "-L").

func (c *Client) BindKey(key string, cmdArgs ...string) error {
	args := append([]string{"bind-key", "-n", key}, cmdArgs...)
	return c.runSilent(args...)
}

// --- Client management ---

// DetachClient detaches all clients from the given session.
func (c *Client) DetachClient(session string) error {
	return c.runSilent("detach-client", "-s", session)
}

// --- Window focus ---

// SelectWindow selects (focuses) a window.
func (c *Client) SelectWindow(target string) error {
	return c.runSilent("select-window", "-t", target)
}

// --- Utility ---

// WindowName returns the tmux window name for a session ID.
func WindowName(sessionID string) string {
	return WindowPrefix + sessionID
}

// InnerSessionName returns the inner tmux session name for a CC session ID.
func InnerSessionName(sessionID string) string {
	return SessionPrefix + sessionID
}

// GetSocketName returns the socket name used by this client.
func (c *Client) GetSocketName() string {
	return c.socketName
}

// WindowTarget returns the full target for a window pane.
// e.g., "ccvalet:sess-abc123.0"
func WindowTarget(windowName string, pane int) string {
	return fmt.Sprintf("%s:%s.%d", SessionName, windowName, pane)
}

// UITarget returns the target for the UI window's pane.
func UITarget(pane int) string {
	return WindowTarget(UIWindowName, pane)
}

// PaneCount returns the number of panes in a window.
func (c *Client) PaneCount(target string) (int, error) {
	out, err := c.run("list-panes", "-t", target, "-F", "#{pane_id}")
	if err != nil {
		return 0, err
	}
	if out == "" {
		return 0, nil
	}
	return len(strings.Split(out, "\n")), nil
}

// ListPaneIDs returns all pane IDs (e.g., ["%0", "%1"]) in a window/session target.
func (c *Client) ListPaneIDs(target string) ([]string, error) {
	out, err := c.run("list-panes", "-t", target, "-F", "#{pane_id}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// IsPaneDead checks if a pane's process has exited.
func (c *Client) IsPaneDead(target string) bool {
	out, _ := c.run("display-message", "-t", target, "-p", "#{pane_dead}")
	return out == "1"
}

// GetPaneID returns the unique pane ID (e.g., "%42") for a target.
func (c *Client) GetPaneID(target string) (string, error) {
	return c.run("display-message", "-t", target, "-p", "#{pane_id}")
}

// ResizePaneWidth sets the width of a pane to the specified number of columns.
func (c *Client) ResizePaneWidth(target string, width int) error {
	return c.runSilent("resize-pane", "-t", target, "-x", fmt.Sprintf("%d", width))
}

// ZoomPane toggles the zoom state of a pane (resize-pane -Z).
func (c *Client) ZoomPane(target string) error {
	return c.runSilent("resize-pane", "-Z", "-t", target)
}

// KillPane kills a specific pane. If it's the last pane in the window, the window is also destroyed.
func (c *Client) KillPane(target string) error {
	return c.runSilent("kill-pane", "-t", target)
}

// GetPaneWindowName returns the window name that contains the given pane.
func (c *Client) GetPaneWindowName(paneID string) (string, error) {
	return c.run("display-message", "-t", paneID, "-p", "#{window_name}")
}

// GetPaneCurrentPath returns the current working directory of the given pane.
func (c *Client) GetPaneCurrentPath(target string) (string, error) {
	return c.run("display-message", "-t", target, "-p", "#{pane_current_path}")
}

// --- Popup ---

// DisplayPopupOptions configures a tmux display-popup.
type DisplayPopupOptions struct {
	Width  string // e.g., "80%"
	Height string // e.g., "80%"
	Cmd    string // command to run inside the popup
	Title  string // popup title (tmux 3.3+)
}

// DisplayPopup opens a tmux popup that runs a command and closes when it exits.
func (c *Client) DisplayPopup(opts DisplayPopupOptions) error {
	args := []string{"display-popup", "-E"}
	if opts.Width != "" {
		args = append(args, "-w", opts.Width)
	}
	if opts.Height != "" {
		args = append(args, "-h", opts.Height)
	}
	if opts.Title != "" {
		args = append(args, "-T", opts.Title)
	}
	if opts.Cmd != "" {
		args = append(args, opts.Cmd)
	}
	return c.runSilent(args...)
}
