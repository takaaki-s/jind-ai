package cmd

import (
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/takaaki-s/claude-code-valet/internal/config"
	"github.com/takaaki-s/claude-code-valet/internal/daemon"
	"github.com/takaaki-s/claude-code-valet/internal/tmux"
	"github.com/takaaki-s/claude-code-valet/internal/tui"
	"golang.org/x/term"
)

const envCcvaletTmux = "CCVALET_TMUX"

var tuiCmd = &cobra.Command{
	Use:     "ui",
	Aliases: []string{"tui"},
	Short:   "Open the interactive TUI",
	Long:    `Open the interactive terminal user interface for managing Claude Code sessions.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// If running inside ccvalet tmux session, run the TUI directly
		if os.Getenv(envCcvaletTmux) == "1" {
			return runTUIInner()
		}
		// Otherwise, set up tmux and attach
		return runTUIWithTmux()
	},
}

func init() {
	rootCmd.AddCommand(tuiCmd)
}

// tuiInnerCommand returns the shell command for the inner TUI process.
func tuiInnerCommand() (string, error) {
	selfBin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to get executable path: %w", err)
	}
	return fmt.Sprintf("%s=1 '%s' ui", envCcvaletTmux, selfBin), nil
}

// runTUIWithTmux creates or reattaches to a tmux session with 2-pane layout.
func runTUIWithTmux() error {
	client := daemon.NewClient(getSocketPath())
	if !client.IsRunning() {
		return fmt.Errorf("daemon is not running. Start with: ccvalet daemon start")
	}

	// Use the manager socket (ccvalet-mgr) for the outer tmux
	tc, err := tmux.NewMgrClient()
	if err != nil {
		return fmt.Errorf("tmux is required: %w", err)
	}

	tuiInnerCmd, err := tuiInnerCommand()
	if err != nil {
		return err
	}

	// Reattach to existing session if it exists
	if tc.HasSession(tmux.SessionName) {
		return reattachTmux(tc, tuiInnerCmd)
	}

	// Create new tmux session
	return createAndAttachTmux(tc, tuiInnerCmd)
}

// createAndAttachTmux creates a new outer tmux session with 2-pane fixed layout and attaches.
// The outer tmux (ccvalet-mgr) has prefix=None so all keystrokes pass through to the inner tmux.
func createAndAttachTmux(tc *tmux.Client, tuiInnerCmd string) error {
	// Load config for detach key
	configMgr, _ := config.NewManager(getConfigDir())
	detachTmuxKey := "C-]"
	if configMgr != nil {
		detachTmuxKey = configMgr.GetDetachKeyTmux()
	}

	// Get terminal size
	cols, rows := 120, 40
	if term.IsTerminal(int(os.Stdout.Fd())) {
		if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			cols, rows = w, h
		}
	}

	// Create outer tmux session with the TUI command running in the "ui" window
	if err := tc.NewSessionWithCmd(tmux.SessionName, cols, rows, tmux.UIWindowName, tuiInnerCmd); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Normalize indices to 0-based (override user's .tmux.conf settings)
	tc.SetOption("base-index", "0", true)
	tc.SetOption("pane-base-index", "0", true)

	// Get TUI pane ID (the only pane so far, use window target to avoid index issues)
	windowTarget := tmux.SessionName + ":" + tmux.UIWindowName
	tuiPaneID, _ := tc.GetPaneID(windowTarget)

	// Configure the outer session
	tc.SetupAutoCleanDeadPanes() // Safety net: auto-kill untagged dead panes
	if tuiPaneID != "" {
		tc.TagManagedPane(tuiPaneID) // TUI pane survives exit
	}
	tc.SetOption("status", "off", true) // Hide tmux status bar
	tc.SetOption("mouse", "on", true)
	tc.SetOption("focus-events", "on", true)      // Enable focus reporting for Bubble Tea FocusMsg/BlurMsg
	tc.SetOption("set-clipboard", "on", true)     // Enable clipboard via OSC 52 for copy-mode
	tc.SetOption("allow-passthrough", "on", true) // Allow OSC 52 passthrough from inner tmux

	// Pane border: color and session name display
	tc.SetOption("pane-active-border-style", "fg=green", true)
	tc.SetOption("pane-border-style", "fg=colour240", true)
	tc.SetOption("pane-border-status", "top", true)
	tc.SetOption("pane-border-format", "#{?#{@session_name}, #{@session_name} ,}", true)

	// prefix=None: prevent outer tmux from capturing user keystrokes
	tc.SetOption("prefix", "None", true)
	tc.SetOption("prefix2", "None", true)

	// Create right pane (75%) for session display.
	// Split using window target (not pane index) to avoid pane-base-index issues.
	tc.SplitWindow(windowTarget, true, 75, tmux.PlaceholderCmd)

	// After split, the new pane (display) is the active pane. Get its ID.
	displayPaneID, _ := tc.GetPaneID(windowTarget)
	if displayPaneID != "" {
		tc.TagManagedPane(displayPaneID) // Display pane survives exit
	}

	// Store pane IDs for runTUIInner to use
	if tuiPaneID != "" {
		tc.SetEnvironment(tmux.SessionName, "CCVALET_TUI_PANE", tuiPaneID)
	}
	if displayPaneID != "" {
		tc.SetEnvironment(tmux.SessionName, "CCVALET_DISPLAY_PANE", displayPaneID)
	}
	// Propagate SSH_AUTH_SOCK to tmux session so popups can access it
	if sshAuthSock := os.Getenv("SSH_AUTH_SOCK"); sshAuthSock != "" {
		tc.SetEnvironment(tmux.SessionName, "SSH_AUTH_SOCK", sshAuthSock)
	}

	// Focus TUI pane (left)
	if tuiPaneID != "" {
		tc.SelectPane(tuiPaneID)
	}

	// Bind detach key to switch to TUI pane (prefix-free binding, works with prefix=None)
	tc.BindKey(detachTmuxKey, "select-pane", "-L")

	return attachToSession(tc)
}

// reattachTmux reattaches to an existing outer tmux session, respawning dead panes.
func reattachTmux(tc *tmux.Client, tuiInnerCmd string) error {
	// Ensure pane-died hook is active (handles upgrade from older version)
	tc.SetupAutoCleanDeadPanes()
	// Update SSH_AUTH_SOCK in tmux session (may have changed on reconnect)
	if sshAuthSock := os.Getenv("SSH_AUTH_SOCK"); sshAuthSock != "" {
		tc.SetEnvironment(tmux.SessionName, "SSH_AUTH_SOCK", sshAuthSock)
	}
	tc.SetOption("focus-events", "on", true)      // Ensure focus reporting is enabled
	tc.SetOption("set-clipboard", "on", true)     // Enable clipboard via OSC 52 for copy-mode
	tc.SetOption("allow-passthrough", "on", true) // Allow OSC 52 passthrough from inner tmux
	tc.SetOption("pane-border-status", "top", true)
	tc.SetOption("pane-border-format", "#{?#{@session_name}, #{@session_name} ,}", true)

	tuiPaneID := tc.GetEnvironment(tmux.SessionName, "CCVALET_TUI_PANE")

	if tuiPaneID != "" {
		if tc.IsPaneDead(tuiPaneID) {
			// TUI pane exists but dead → respawn it
			tc.RespawnPane(tuiPaneID, tuiInnerCmd)
		}
		// Select TUI pane
		tc.SelectPane(tuiPaneID)
	} else {
		// No tracked TUI pane → respawn in UI window pane 0
		tc.RespawnPane(tmux.UITarget(0), tuiInnerCmd)
		tc.SelectWindow(tmux.SessionName + ":" + tmux.UIWindowName)
	}

	// Restore right pane if dead
	displayPaneID := tc.GetEnvironment(tmux.SessionName, "CCVALET_DISPLAY_PANE")
	if displayPaneID != "" && tc.IsPaneDead(displayPaneID) {
		tc.RespawnPane(displayPaneID, tmux.PlaceholderCmd)
	}

	return attachToSession(tc)
}

// attachToSession attaches to the tmux session and blocks until detach.
func attachToSession(tc *tmux.Client) error {
	attachCmd := tc.AttachCmd(tmux.SessionName)
	attachCmd.Stdin = os.Stdin
	attachCmd.Stdout = os.Stdout
	attachCmd.Stderr = os.Stderr
	return attachCmd.Run()
}

// runTUIInner runs the Bubble Tea TUI inside the outer tmux pane.
func runTUIInner() error {
	client := daemon.NewClient(getSocketPath())
	if !client.IsRunning() {
		return fmt.Errorf("daemon is not running. Start with: ccvalet daemon start")
	}

	// Use the manager socket (ccvalet-mgr) for the outer tmux
	tc, err := tmux.NewMgrClient()
	if err != nil {
		return fmt.Errorf("tmux not available in inner mode: %w", err)
	}

	// Load config for detach key
	configMgr, _ := config.NewManager(getConfigDir())
	detachTmuxKey := "C-]"
	if configMgr != nil {
		detachTmuxKey = configMgr.GetDetachKeyTmux()
	}

	// Get TUI pane ID from $TMUX_PANE (set by tmux for every pane process — most reliable)
	tuiPaneID := os.Getenv("TMUX_PANE")
	if tuiPaneID == "" {
		// Fallback: read from stored env (set by createAndAttachTmux)
		tuiPaneID = tc.GetEnvironment(tmux.SessionName, "CCVALET_TUI_PANE")
	}
	if tuiPaneID != "" {
		tc.SetEnvironment(tmux.SessionName, "CCVALET_TUI_PANE", tuiPaneID)
		tc.TagManagedPane(tuiPaneID)
		// Rebind detach key to focus TUI pane by ID (works from any pane)
		tc.BindKey(detachTmuxKey, "run-shell",
			fmt.Sprintf("tmux -L %s select-pane -t %s", tmux.MgrSocketName, tuiPaneID))
	}

	// Get display pane ID: find the pane in the UI window that is NOT the TUI pane.
	// On first startup, createAndAttachTmux may not have created the display pane yet
	// (race between TUI process startup and SplitWindow), so retry with backoff.
	windowTarget := tmux.SessionName + ":" + tmux.UIWindowName
	displayPaneID := ""
	for retries := 0; retries < 20; retries++ {
		if panes, err := tc.ListPaneIDs(windowTarget); err == nil {
			for _, p := range panes {
				if p != tuiPaneID {
					displayPaneID = p
					break
				}
			}
		}
		if displayPaneID == "" {
			displayPaneID = tc.GetEnvironment(tmux.SessionName, "CCVALET_DISPLAY_PANE")
		}
		if displayPaneID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if displayPaneID != "" {
		tc.SetEnvironment(tmux.SessionName, "CCVALET_DISPLAY_PANE", displayPaneID)
	}

	model := tui.NewModelWithTmux(client, tc, tuiPaneID, displayPaneID)

	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithReportFocus())
	if _, err := p.Run(); err != nil {
		return err
	}

	// Detach the client instead of killing the session.
	// The outer tmux session stays alive with CC processes running in inner tmux.
	tc.DetachClient(tmux.SessionName)
	return nil
}
