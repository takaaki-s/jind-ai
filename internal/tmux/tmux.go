package tmux

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const (
	// SocketName is the built-in default tmux socket name for jin (inner tmux
	// for CC sessions). Use it only to mean "the default itself"; anything
	// that actually talks to a server — including a socket name interpolated
	// into a shell command — must go through DefaultSocketName so
	// JIN_TMUX_SOCKET is honored.
	SocketName = "jin"

	// MgrSocketName is the built-in default tmux socket name for the outer
	// layout manager. Same rule as SocketName: use DefaultMgrSocketName for
	// the effective name, so JIN_TMUX_MGR_SOCKET is honored.
	MgrSocketName = "jin-mgr"

	// SessionName is the tmux session name (used for the outer tmux session)
	SessionName = "jin"

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
	PaneKeepTag = "@jin-keep"

	// PaneNameOption is a custom pane option holding a named-slot pane's name
	// (see SplitPane + FindPaneByName). Named slots make plugin-driven splits
	// idempotent: split --name reuses the existing pane instead of stacking a
	// new one on every invocation.
	PaneNameOption = "@jin-pane-name"

	// PaneLabelOption is a custom pane option holding the text the outer
	// tmux's pane-border-format renders above a pane (see
	// cmd/jin/cmd/tui.go's paneBorderFormat). Pane options survive
	// respawn-pane and clear-history, so anything that empties a pane has to
	// reset this explicitly or the old label stays on the border.
	PaneLabelOption = "@session_name"
)

// IfExists policies for named-slot splits: what to do when a pane with the
// requested name already exists in the target window.
const (
	IfExistsNoop    = "noop"    // reuse the existing pane as-is (default)
	IfExistsRespawn = "respawn" // respawn the existing pane with the new command
	IfExistsError   = "error"   // fail instead of reusing
)

// ValidateIfExists checks an if-exists policy value. Empty means IfExistsNoop.
func ValidateIfExists(v string) error {
	switch v {
	case "", IfExistsNoop, IfExistsRespawn, IfExistsError:
		return nil
	}
	return fmt.Errorf("invalid if-exists %q (want noop, respawn or error)", v)
}

// ValidateSlotOptions is the composed validator both the CLI and the daemon
// handler run on a named-slot split. Keeping it in one place also keeps the
// error text (which the user sees) identical across both entry points.
func ValidateSlotOptions(name, ifExists string, opts SplitOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	if err := ValidateIfExists(ifExists); err != nil {
		return err
	}
	if ifExists != "" && name == "" {
		return fmt.Errorf("--if-exists requires --name")
	}
	if name != "" {
		return ValidatePaneName(name)
	}
	return nil
}

// paneNameRe restricts named-slot pane names to a shell- and tmux-format-safe
// subset: an alphanumeric first character, then up to 63 of [a-zA-Z0-9._-].
var paneNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// ValidatePaneName checks a named-slot pane name (the PaneNameOption value).
func ValidatePaneName(name string) error {
	if !paneNameRe.MatchString(name) {
		return fmt.Errorf("invalid pane name %q (want an alphanumeric first character, then up to 63 of letters, digits, '.', '_' or '-')", name)
	}
	return nil
}

// SocketPathFromEnv extracts the server socket path from a $TMUX value, whose
// format is "<socket-path>,<pid>,<session-index>". It returns "" for an empty
// value, so callers can treat "not inside tmux" and "no socket" uniformly.
func SocketPathFromEnv(v string) string {
	sock, _, _ := strings.Cut(v, ",")
	return sock
}

// Client wraps tmux CLI commands, always using the dedicated socket.
type Client struct {
	tmuxPath   string
	socketName string
	socketPath string // optional: "-S <path>" passed to tmux; takes precedence over socketName when set
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

// NewClientWithSocketPath creates a tmux client that targets an arbitrary server
// by socket path ("-S <path>"), such as the caller's outer tmux discovered via
// $TMUX. Unlike NewClientWithSocket, it addresses the server by filesystem path
// rather than by the jin-managed socket name.
func NewClientWithSocketPath(socketPath string) (*Client, error) {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return nil, fmt.Errorf("tmux not found: %w", err)
	}
	return &Client{tmuxPath: path, socketPath: socketPath}, nil
}

// NewClientForServer creates a client for an explicit local server identity.
func NewClientForServer(server ServerRef) (*Client, error) {
	if err := server.Validate(); err != nil {
		return nil, err
	}
	switch server.Kind {
	case ServerDefault:
		path, err := exec.LookPath("tmux")
		if err != nil {
			return nil, fmt.Errorf("tmux not found: %w", err)
		}
		return &Client{tmuxPath: path}, nil
	case ServerName:
		return NewClientWithSocket(server.Value)
	case ServerPath:
		return NewClientWithSocketPath(server.Value)
	default:
		return nil, fmt.Errorf("unknown tmux server kind %q", server.Kind)
	}
}

// ServerRef reports the exact local server addressed by this client.
func (c *Client) ServerRef() ServerRef {
	if c.socketPath != "" {
		return ServerRef{Kind: ServerPath, Value: c.socketPath}
	}
	if c.socketName != "" {
		return ServerRef{Kind: ServerName, Value: c.socketName}
	}
	return ServerRef{Kind: ServerDefault}
}

// NewClient creates a new tmux client using the default socket name resolved
// by DefaultSocketName (JIN_TMUX_SOCKET → SocketName).
// Returns error if tmux is not found.
func NewClient() (*Client, error) {
	return NewClientWithSocket(DefaultSocketName())
}

// DefaultSocketName returns the socket name NewClient targets. When the
// JIN_TMUX_SOCKET environment variable is a non-empty value it wins, otherwise
// the built-in SocketName ("jin") is returned. Exposed so callers that build a
// Client through other paths (e.g. the session Manager's lazy fallback in
// ensureTmuxClient) apply the exact same resolution as NewClient — used by the
// e2e suite to redirect every implicit tmux access to a per-run socket.
func DefaultSocketName() string {
	if v := os.Getenv("JIN_TMUX_SOCKET"); v != "" {
		return v
	}
	return SocketName
}

// DefaultMgrSocketName returns the socket name NewMgrClient targets, honoring
// JIN_TMUX_MGR_SOCKET the same way DefaultSocketName honors JIN_TMUX_SOCKET for
// the inner socket — which lets `jin ui` point at a throwaway outer tmux server
// during manual verification instead of the real "jin-mgr".
//
// Resolved via a live os.Getenv read rather than a cached package var, for the
// same reason as DefaultSocketName: NewMgrClient() is already called fresh per
// CLI invocation, so a live read costs nothing extra.
func DefaultMgrSocketName() string {
	if v := os.Getenv("JIN_TMUX_MGR_SOCKET"); v != "" {
		return v
	}
	return MgrSocketName
}

// NewMgrClient creates a tmux client for the outer layout manager socket,
// resolved via DefaultMgrSocketName. Uses -f /dev/null to prevent loading
// user's ~/.tmux.conf, ensuring user key bindings (e.g., Shift+Arrow) pass
// through to the inner tmux.
func NewMgrClient() (*Client, error) {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return nil, fmt.Errorf("tmux not found: %w", err)
	}
	return &Client{tmuxPath: path, socketName: DefaultMgrSocketName(), configFile: "/dev/null"}, nil
}

// HasTmux returns true if tmux is available on the system.
func HasTmux() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// baseArgs returns the common tmux arguments (socket, config file).
func (c *Client) baseArgs() []string {
	var args []string
	if c.socketPath != "" {
		args = []string{"-S", c.socketPath}
	} else if c.socketName != "" {
		args = []string{"-L", c.socketName}
	}
	if c.configFile != "" {
		args = append(args, "-f", c.configFile)
	}
	return args
}

// run executes a tmux command with the dedicated socket.
func (c *Client) run(args ...string) (string, error) {
	return c.runInput(nil, args...)
}

// runInput is run with a stdin source, for the verbs that take their payload
// on stdin rather than in argv.
func (c *Client) runInput(stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command(c.tmuxPath, append(c.baseArgs(), args...)...)
	cmd.Stdin = stdin
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
	return parseEnvironmentOutput(out)[name]
}

// parseEnvironmentOutput parses "show-environment" output into a name→value
// map. Each line is either "NAME=value" or "-NAME" (unset marker); unset
// markers and empty/malformed lines all lack a "=" and are skipped in one
// pass by strings.Cut.
func parseEnvironmentOutput(out string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		env[name] = value
	}
	return env
}

// ListEnvironment returns every environment variable set on the tmux session
// as a name→value map in a single tmux call. Returns an empty (non-nil) map
// on tmux error so callers can range safely.
func (c *Client) ListEnvironment(session string) map[string]string {
	out, err := c.run("show-environment", "-t", session)
	if err != nil {
		return map[string]string{}
	}
	return parseEnvironmentOutput(out)
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

// appendEnvArgs appends one -e per assignment, in slice order. Empty values are
// kept: tmux has no unset form for -e, so "K=" is how a pane is told a value is
// unknown rather than left to inherit the server's. split-window, display-popup
// and respawn-pane all take this form, which is why it is here and not in each.
func appendEnvArgs(args, env []string) []string {
	for _, assignment := range env {
		args = append(args, "-e", assignment)
	}
	return args
}

// buildRespawnArgs translates a respawn into respawn-pane arguments. Kept as a
// pure function so the flag translation is unit-testable without tmux, as
// buildSplitArgs and buildPopupArgs are.
func buildRespawnArgs(target, shellCmd string, env []string) []string {
	args := appendEnvArgs([]string{"respawn-pane", "-t", target, "-k"}, env)
	if shellCmd != "" {
		args = append(args, shellCmd)
	}
	return args
}

// RespawnPane respawns a dead pane with a new command. Runner.RespawnPane says
// what env carries and what a nil one asserts.
func (c *Client) RespawnPane(target, shellCmd string, env []string) error {
	return c.runSilent(buildRespawnArgs(target, shellCmd, env)...)
}

// ClearHistory wipes both scrollback and the visible screen of the target pane
// (requires tmux 3.2+ for the -H flag). Callers use this after RespawnPane on
// panes with remain-on-exit to drop leftover "[detached]" / "[exited]" markers
// from prior processes.
func (c *Client) ClearHistory(target string) error {
	return c.runSilent("clear-history", "-H", "-t", target)
}

// --- Pane management ---

// SplitOptions configures a SplitPane call, mirroring tmux split-window's
// vocabulary rather than inventing a new one.
type SplitOptions struct {
	Direction string   // where the new pane opens: "down" (empty = down), "up", "left", "right"
	Size      string   // new pane size: "30%" (percent) or "15" (lines/columns); empty = tmux default
	Full      bool     // span the full window width/height (-f)
	NoFocus   bool     // keep focus on the current pane (-d)
	Dir       string   // working directory for the new pane (-c); empty = tmux default
	Cmd       string   // command to run in the new pane; empty = shell
	Env       []string // KEY=VALUE assignments, one -e each; empty is not neutral — see jinenv.Identity.TmuxEnviron
}

// Validate checks Direction and Size. Percent sizes are limited to 1-99;
// line/column sizes must be positive integers.
func (o SplitOptions) Validate() error {
	switch o.Direction {
	case "", "down", "up", "left", "right":
	default:
		return fmt.Errorf("invalid direction %q (want down, up, left or right)", o.Direction)
	}
	if o.Size == "" {
		return nil
	}
	if pct, ok := strings.CutSuffix(o.Size, "%"); ok {
		if n, err := strconv.Atoi(pct); err == nil && n >= 1 && n <= 99 {
			return nil
		}
	} else if n, err := strconv.Atoi(o.Size); err == nil && n >= 1 {
		return nil
	}
	return fmt.Errorf("invalid size %q (want a percentage like \"30%%\" or a line count like \"15\")", o.Size)
}

// buildSplitArgs translates SplitOptions into split-window arguments. Kept as
// a pure function so the flag translation is unit-testable without tmux.
func buildSplitArgs(target string, o SplitOptions) []string {
	args := []string{"split-window", "-t", target, "-P", "-F", "#{pane_id}"}
	switch o.Direction {
	case "up":
		args = append(args, "-v", "-b")
	case "right":
		args = append(args, "-h")
	case "left":
		args = append(args, "-h", "-b")
	default: // "", "down"
		args = append(args, "-v")
	}
	if o.Full {
		args = append(args, "-f")
	}
	if o.NoFocus {
		args = append(args, "-d")
	}
	if o.Size != "" {
		args = append(args, "-l", o.Size)
	}
	if o.Dir != "" {
		args = append(args, "-c", o.Dir)
	}
	args = appendEnvArgs(args, o.Env)
	if o.Cmd != "" {
		args = append(args, o.Cmd)
	}
	return args
}

// SplitPane splits the target pane per opts and returns the new pane's ID
// (via split-window -P -F "#{pane_id}"). Trusts opts — callers reaching this
// layer (daemon handler + CLI) have already run Validate at the trust
// boundary.
func (c *Client) SplitPane(target string, opts SplitOptions) (string, error) {
	return c.run(buildSplitArgs(target, opts)...)
}

// matchPaneByName scans list-panes output ("<pane_id> <name>" per line, name
// possibly empty) for an exact name match. Pure helper for FindPaneByName.
func matchPaneByName(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		if id, got, _ := strings.Cut(line, " "); id != "" && got == name {
			return id
		}
	}
	return ""
}

// FindPaneByName returns the pane in target's window whose PaneNameOption
// equals name, or "" when no such pane exists. target may be a pane ID or a
// window target; tmux resolves either to the containing window.
func (c *Client) FindPaneByName(target, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("pane name is required")
	}
	out, err := c.run("list-panes", "-t", target, "-F", "#{pane_id} #{"+PaneNameOption+"}")
	if err != nil {
		return "", err
	}
	return matchPaneByName(out, name), nil
}

// PaneSlotOps is the subset of pane operations EnsureNamedPane needs. Both
// Runner and *Client satisfy it.
type PaneSlotOps interface {
	FindPaneByName(target, name string) (string, error)
	SplitPane(target string, opts SplitOptions) (string, error)
	SetPaneOption(target, option, value string) error
	// RespawnPane replaces the process running in a pane. env is the assignments
	// this call adds, one -e each; nil adds none, which is right only where
	// something else has already answered — see the call site.
	RespawnPane(target, cmd string, env []string) error
	KillPane(target string) error
}

// EnsureNamedPane runs the named-slot split procedure shared by the daemon
// path (session.Manager.PaneSplit) and the CLI --here path: when a pane named
// name already exists in target's window, apply the ifExists policy
// (IfExistsNoop/Respawn/Error); otherwise split target per opts and tag the
// new pane with name. An empty name degrades to a plain split. The
// find-then-split sequence is check-then-act: callers that need idempotency
// under concurrent invocations serialize calls themselves (session.Manager
// holds a lock; --here is unarbitrated).
func EnsureNamedPane(ops PaneSlotOps, target, name, ifExists string, opts SplitOptions) (string, error) {
	if ifExists == "" {
		ifExists = IfExistsNoop
	}
	if name != "" {
		existing, err := ops.FindPaneByName(target, name)
		if err != nil {
			return "", err
		}
		if existing != "" {
			switch ifExists {
			case IfExistsRespawn:
				if err := ops.RespawnPane(existing, opts.Cmd, opts.Env); err != nil {
					return "", err
				}
				return existing, nil
			case IfExistsError:
				return "", fmt.Errorf("pane %q already exists", name)
			default: // IfExistsNoop
				return existing, nil
			}
		}
	}
	newID, err := ops.SplitPane(target, opts)
	if err != nil {
		return "", err
	}
	if name != "" {
		if err := ops.SetPaneOption(newID, PaneNameOption, name); err != nil {
			// An unnamed leftover pane would break the slot's idempotency for
			// good (never found, split again every time), so tear it down.
			_ = ops.KillPane(newID)
			return "", fmt.Errorf("pane %s created but naming failed (pane removed): %w", newID, err)
		}
	}
	return newID, nil
}

// CloseNamedPane is EnsureNamedPane's teardown mirror: find the named pane in
// target's window and kill it, refusing to touch protected (empty = no
// protected pane). Callers keep the "which pane is off-limits" decision
// (session's agent pane / --here's anchor pane) at their own layer.
func CloseNamedPane(ops PaneSlotOps, target, name, protected string) error {
	paneID, err := ops.FindPaneByName(target, name)
	if err != nil {
		return err
	}
	if paneID == "" {
		return fmt.Errorf("no pane named %q", name)
	}
	if protected != "" && paneID == protected {
		return fmt.Errorf("refusing to kill protected pane %s", paneID)
	}
	return ops.KillPane(paneID)
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
//
// The `--` is load-bearing: see SendKeysLiteral.
func (c *Client) SendKeys(target, keys string) error {
	return c.runSilent(buildSendKeysArgs(target, keys)...)
}

// buildSendKeysArgs and buildSendKeysLiteralArgs exist so the `--` placement
// is assertable without spawning tmux.
func buildSendKeysArgs(target, keys string) []string {
	return []string{"send-keys", "-t", target, "--", keys}
}

func buildSendKeysLiteralArgs(target, text string) []string {
	return []string{"send-keys", "-t", target, "-l", "--", text}
}

// SendKeysLiteral sends literal text to a pane (no key name lookup).
//
// The `--` terminates option parsing so text beginning with a dash reaches the
// pane instead of being read as a flag. Without it tmux fails one of two ways,
// and the quiet one is the dangerous one:
//
//	send-keys -t %0 -l "-abc"   -> exit 1, "unknown flag -a"
//	send-keys -t %0 -l "-R"     -> exit 0, and NOTHING is sent
//
// The second case reports success while silently dropping the payload, so
// Manager.SendPrompt would carry on and commit a prompt with a hole in it. That
// matters here because SendPrompt splits long prompts on a fixed byte boundary:
// a chunk can start with a dash even when the prompt does not, and bullet lists
// and diffs make leading dashes common in exactly the large prompts this path
// exists to carry.
func (c *Client) SendKeysLiteral(target, text string) error {
	return c.runSilent(buildSendKeysLiteralArgs(target, text)...)
}

// buildLoadBufferArgs and buildPasteBufferArgs exist so the flag set is
// assertable without spawning tmux — `-p` in particular, see PasteBuffer.
func buildLoadBufferArgs(name string) []string {
	// "-" reads the payload from stdin, so the prompt never appears in argv
	// (no 16341-byte limit, nothing for `ps` to show) nor on disk.
	return []string{"load-buffer", "-b", name, "-"}
}

func buildPasteBufferArgs(target, name string) []string {
	return []string{"paste-buffer", "-p", "-d", "-b", name, "-t", target}
}

// LoadBuffer stores content in a named tmux paste buffer, feeding it through
// stdin rather than the command line.
func (c *Client) LoadBuffer(name, content string) error {
	_, err := c.runInput(strings.NewReader(content), buildLoadBufferArgs(name)...)
	return err
}

// PasteBuffer pastes a named buffer into a pane as a bracketed paste, then
// deletes the buffer.
//
// `-p` is not optional. It wraps the payload in the bracketed-paste markers
// that tell a TUI "this is pasted, not typed", which decides two things:
//
//   - Without it, tmux replays every newline as a Return keypress, so a
//     multi-line prompt is SUBMITTED ONE LINE AT A TIME. Measured against a
//     real OpenCode pane: a three-line buffer became three separate messages.
//   - With it the TUI inserts the payload in one operation instead of
//     simulating thousands of keystrokes — see docs/gotchas.md "Session send".
//
// `-d` drops the buffer afterwards so the prompt does not linger in tmux's
// buffer stack where any client could read it back.
func (c *Client) PasteBuffer(target, name string) error {
	return c.runSilent(buildPasteBufferArgs(target, name)...)
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

// GetPaneOption reads a pane-level option back off the server. Returns an
// empty string for an option that was never set, since tmux expands an unknown
// #{@name} to nothing rather than failing.
func (c *Client) GetPaneOption(target, option string) (string, error) {
	return c.run("display-message", "-t", target, "-p", "#{"+option+"}")
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
	if err := c.SetPaneOption(target, "remain-on-exit", "on"); err != nil {
		return err
	}
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
// e.g., "jin:sess-abc123.0"
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
	dead, _ := c.PaneDeath(target)
	return dead
}

// PaneDeath reports whether the pane's process has exited and, when it has, the
// status it exited with. Both come from one tmux call so the status cannot
// describe a different process than the death did.
//
// status is 0 whenever tmux does not name one. The case that actually happens
// is a process killed by a signal: tmux fills `pane_dead_signal` and leaves
// `pane_dead_status` empty (measured on 3.5a, 3/3 across SIGINT, SIGTERM and
// send-keys C-c). An agent killed from outside therefore reads as a clean exit
// here. Callers read a non-zero status as "this process failed", and answering
// 0 on a doubt keeps the doubt from being read as failure: the caller that has
// one restarts the agent, and doing that to a process the user shut down costs
// them the session they were reading.
func (c *Client) PaneDeath(target string) (dead bool, status int) {
	out, _ := c.run("display-message", "-t", target, "-p", "#{pane_dead} #{pane_dead_status}")
	return parsePaneDeath(out)
}

// parsePaneDeath reads the two-field format PaneDeath asks tmux for. Split out
// because every way the pair can be incomplete resolves the same direction and
// a test can say so; what it cannot check is that the format string still names
// the fields tmux knows — nor that `remain-on-exit` is on, without which tmux
// destroys the pane instead of holding it dead.
func parsePaneDeath(out string) (dead bool, status int) {
	fields := strings.Fields(out)
	if len(fields) == 0 || fields[0] != "1" {
		return false, 0
	}
	if len(fields) < 2 {
		return true, 0
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		return true, 0
	}
	return true, n
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

// parsePanePID converts a `#{pane_pid}` reading into a usable pid. Split out
// from TerminatePaneProcess so the parsing half is testable without signalling
// a real process: every rejected input here is one that would otherwise reach
// syscall.Kill. Non-positive values are refused because negative pids address a
// whole process group and 0 addresses the caller's own group — either would
// turn a pane-scoped stop into a much wider kill.
func parsePanePID(out string) (int, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return 0, fmt.Errorf("empty pane_pid")
	}
	pid, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("unparsable pane_pid %q: %w", trimmed, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pane_pid %d", pid)
	}
	return pid, nil
}

// TerminatePaneProcess hangs up the process tmux started in target, leaving the
// pane itself in place. On a pane carrying remain-on-exit (every managed pane —
// see TagManagedPane) the pane survives as a dead pane, keeping its scrollback
// and its slot in the window layout, so RespawnPane can later revive the agent
// exactly where it was.
//
// The signal is SIGHUP, not SIGTERM, because of what actually sits at the end of
// a pane's pid. tmux starts the command through `/bin/sh -c`, but that wrapper
// execs its way down the chain, so for a command shaped like the one
// session.Manager builds the pane's pid is the *interactive* shell — and
// interactive shells ignore SIGTERM. Measured on zsh 5.9: SIGTERM leaves it in
// Ss with pane_dead=0, SIGHUP takes it and its jobs down. SIGHUP is also what a
// pane's processes already receive when kill-pane closes the pty.
//
// It goes to the pane's own pid rather than its process group: the agent runs in
// a job of its own, which a killpg on the pane's group would miss entirely.
// Hanging up the shell is enough — it HUPs its jobs on the way out, and tmux
// closes the pty behind it.
//
// A pane whose process ignores SIGHUP too stays alive and reports no error;
// callers that need the stop guaranteed must confirm via IsPaneDead and fall
// back to KillPane. IsPaneDead only answers for that direct child: a descendant
// that escaped the shell's job control and the pty (double-forked, setsid'd)
// keeps running with the pane reported dead. No agent adapter shipped so far
// does that, but it is the assumption to re-check when adding one.
//
// Do not call this on a pane that has already exited. tmux keeps reporting the
// pid such a pane started with, and the OS is free to have reissued that number
// to an unrelated process — check IsPaneDead first.
func (c *Client) TerminatePaneProcess(target string) error {
	out, err := c.run("display-message", "-t", target, "-p", "#{pane_pid}")
	if err != nil {
		return fmt.Errorf("read pane_pid of %s: %w", target, err)
	}
	pid, err := parsePanePID(out)
	if err != nil {
		return fmt.Errorf("pane %s: %w", target, err)
	}
	return syscall.Kill(pid, syscall.SIGHUP)
}

// GetPaneWindowName returns the window name that contains the given pane.
func (c *Client) GetPaneWindowName(paneID string) (string, error) {
	return c.run("display-message", "-t", paneID, "-p", "#{window_name}")
}

// GetPaneCurrentPath returns the current working directory of the given pane.
func (c *Client) GetPaneCurrentPath(target string) (string, error) {
	return c.run("display-message", "-t", target, "-p", "#{pane_current_path}")
}

// GetPaneTTY returns the TTY path of a pane (e.g., "/dev/ttys005").
func (c *Client) GetPaneTTY(target string) (string, error) {
	return c.run("display-message", "-t", target, "-p", "#{pane_tty}")
}

// SwitchClient switches an inner tmux client (identified by its TTY) to a different session.
// This avoids killing the tmux attach process, preventing "pane is dead" issues.
func (c *Client) SwitchClient(clientTTY, targetSession string) error {
	return c.runSilent("switch-client", "-c", clientTTY, "-t", targetSession)
}

// DetachClientByTTY detaches the tmux client identified by its TTY path.
func (c *Client) DetachClientByTTY(clientTTY string) error {
	return c.runSilent("detach-client", "-t", clientTTY)
}

// ClientSessionForTTY returns the session name the client on the given TTY
// is attached to, or "" when no such client exists.
func (c *Client) ClientSessionForTTY(tty string) (string, error) {
	out, err := c.run("list-clients", "-F", "#{client_tty} #{client_session}")
	if err != nil {
		return "", err
	}
	return clientSessionForTTY(out, tty), nil
}

// clientSessionForTTY parses "list-clients -F '#{client_tty} #{client_session}'"
// output and returns the session for tty, or "" if no client matches.
func clientSessionForTTY(out, tty string) string {
	for _, line := range strings.Split(out, "\n") {
		if t, sess, ok := strings.Cut(line, " "); ok && t == tty {
			return sess
		}
	}
	return ""
}

// --- Popup ---

// DisplayPopupOptions configures a tmux display-popup.
type DisplayPopupOptions struct {
	Target string   // pane/session target for the popup (-t); empty uses the active client
	Width  string   // e.g., "80%"
	Height string   // e.g., "80%"
	Dir    string   // working directory for the command inside the popup (-d)
	Cmd    string   // command to run inside the popup
	Title  string   // popup title (tmux 3.3+)
	Env    []string // KEY=VALUE assignments, one -e each; empty is not neutral — see jinenv.Identity.TmuxEnviron
}

// buildPopupArgs translates DisplayPopupOptions into display-popup arguments.
// Kept as a pure function so the flag translation is unit-testable without
// tmux, as buildSplitArgs is.
func buildPopupArgs(o DisplayPopupOptions) []string {
	args := []string{"display-popup", "-E"}
	if o.Target != "" {
		args = append(args, "-t", o.Target)
	}
	if o.Width != "" {
		args = append(args, "-w", o.Width)
	}
	if o.Height != "" {
		args = append(args, "-h", o.Height)
	}
	if o.Dir != "" {
		args = append(args, "-d", o.Dir)
	}
	if o.Title != "" {
		args = append(args, "-T", o.Title)
	}
	args = appendEnvArgs(args, o.Env)
	if o.Cmd != "" {
		args = append(args, o.Cmd)
	}
	return args
}

// DisplayPopup opens a tmux popup that runs a command and closes when it exits.
func (c *Client) DisplayPopup(opts DisplayPopupOptions) error {
	return c.runSilent(buildPopupArgs(opts)...)
}
