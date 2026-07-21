package cmd

import (
	"fmt"
	"log"
	"maps"
	"os"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/paths"
	"github.com/takaaki-s/jind-ai/internal/plugin"
	"github.com/takaaki-s/jind-ai/internal/tmux"
	"github.com/takaaki-s/jind-ai/internal/tui"
	"golang.org/x/term"
)

const envJinTmux = "JIN_TMUX"

// Pane border styling — kept in sync with the TUI's Tokyo Night palette so
// the outer tmux frame reads as one design with the inner session list.
// Active pane uses bright blue + bold, inactive uses a muted gray. The
// session-name label on the display pane is bolded when present.
const (
	activePaneBorderStyle   = "fg=#7aa2f7,bold"
	inactivePaneBorderStyle = "fg=#414868"
	paneBorderFormat        = "#{?#{@session_name}, #[bold]#{@session_name}#[nobold] ,}"
	// tuiPaneBorderLabel is the pane-border-format text shown on the TUI (left)
	// pane. Kept short since the user already knows they're in jind-ai.
	tuiPaneBorderLabel = "sessions"
)

// applyPaneBorderStyle applies the modern pane-border styling to the outer
// tmux server. Safe to call multiple times (idempotent).
func applyPaneBorderStyle(tc *tmux.Client) {
	_ = tc.SetOption("pane-active-border-style", activePaneBorderStyle, true)
	_ = tc.SetOption("pane-border-style", inactivePaneBorderStyle, true)
	_ = tc.SetOption("pane-border-status", "top", true)
	_ = tc.SetOption("pane-border-format", paneBorderFormat, true)
}

// togglePaneBinder is the minimal tmux surface applyTogglePaneBinding needs.
// *tmux.Client satisfies it directly; tests inject a fake.
type togglePaneBinder interface {
	BindKey(key string, cmdArgs ...string) error
}

// applyTogglePaneBinding wires the outer tmux root bindings that zoom/unzoom
// the display pane (sidebar toggle). Idempotent: re-issuing bind-key overwrites
// the prior mapping. No-op when configMgr is nil, displayPaneID is empty, or
// the user set TogglePane to an explicit empty slice.
func applyTogglePaneBinding(tc togglePaneBinder, configMgr *config.Manager, displayPaneID string) {
	if configMgr == nil || displayPaneID == "" {
		return
	}
	for _, key := range configMgr.GetTogglePaneKeys() {
		if key == "" {
			continue
		}
		_ = tc.BindKey(key, "resize-pane", "-Z", "-t", displayPaneID)
	}
}

// actionPanelBinder is the minimal tmux surface applyActionPanelBinding needs.
// *tmux.Client satisfies it directly; tests inject a fake.
type actionPanelBinder interface {
	BindKey(key string, cmdArgs ...string) error
}

// applyActionPanelBinding wires the outer tmux root bindings that launch the
// action palette popup. Idempotent: re-issuing bind-key overwrites the prior
// mapping. No-op when configMgr is nil, selfBin is empty, or the user set
// ActionPanel to an explicit empty slice.
func applyActionPanelBinding(tc actionPanelBinder, configMgr *config.Manager, selfBin string) {
	if configMgr == nil || selfBin == "" {
		return
	}
	width, height := configMgr.GetPopupSize(config.PopupAction)
	popupCmd := fmt.Sprintf("'%s' action-popup", selfBin)
	for _, key := range configMgr.GetActionPanelKeys() {
		if key == "" {
			continue
		}
		_ = tc.BindKey(key,
			"display-popup",
			"-w", width,
			"-h", height,
			"-T", " Action Palette ",
			"-E", popupCmd,
		)
	}
}

// applySessionFilterBinding wires the outer tmux root bindings that launch
// the session filter popup. Idempotent: re-issuing bind-key overwrites the
// prior mapping. No-op when configMgr is nil, selfBin is empty, or the
// user set Search to an explicit empty slice.
func applySessionFilterBinding(tc actionPanelBinder, configMgr *config.Manager, selfBin string) {
	if configMgr == nil || selfBin == "" {
		return
	}
	width, height := configMgr.GetPopupSize(config.PopupSessionFilter)
	popupCmd := fmt.Sprintf("'%s' session-filter-popup", selfBin)
	for _, key := range configMgr.GetSessionFilterKeys() {
		if key == "" {
			continue
		}
		_ = tc.BindKey(key,
			"display-popup",
			"-w", width,
			"-h", height,
			"-T", " Session Filter ",
			"-E", popupCmd,
		)
	}
}

// installedPluginSetFn is a function that returns the set of currently
// installed and enabled plugin names. Injected into
// applyPluginActionBindings so tests can bypass the on-disk registry read.
type installedPluginSetFn func() map[string]struct{}

// applyPluginActionBindings wires outer tmux root bindings that fire
// `jin plugin run <name> <action>` for each configured plugin action.
// Idempotent: re-issuing bind-key overwrites the prior mapping. No-op when
// configMgr is nil, selfBin is empty, or no plugin bindings are configured.
// Bindings are issued only for plugins currently installed AND enabled
// (StateEnabled). Uninstalled / broken / incompatible plugins are silently
// skipped with a single log line each — config vs. installed set drift is
// common in dev environments and must never block TUI startup. Action IDs
// are not validated against the manifest here: a typo'd action fails at
// `jin plugin run` time with the daemon's error listing valid IDs. Key
// collisions with core outer-tmux bindings are warned once (see
// reservedOuterTmuxKeys) but not blocked; tmux's last-write-wins semantics
// apply. Reuses actionPanelBinder — same one-method interface as the two
// existing binder callers.
func applyPluginActionBindings(tc actionPanelBinder, configMgr *config.Manager, selfBin string, installedFn installedPluginSetFn) {
	if configMgr == nil || selfBin == "" || installedFn == nil {
		return
	}
	bindings := configMgr.GetPluginKeybindings()
	if len(bindings) == 0 {
		return
	}
	installed := installedFn()
	reserved := reservedOuterTmuxKeys(configMgr)
	for _, name := range slices.Sorted(maps.Keys(bindings)) {
		actions := bindings[name]
		if _, ok := installed[name]; !ok {
			// Runnable() filters to StateEnabled, dropping broken/incompatible/
			// disabled alike — so "not enabled" is the accurate umbrella.
			log.Printf("plugin key binding skipped: %s not in the enabled plugin set (uninstalled, disabled, broken, or incompatible)", name)
			continue
		}
		for _, actionID := range slices.Sorted(maps.Keys(actions)) {
			if actionID == "" {
				continue
			}
			// `>/dev/null 2>&1` is belt-and-suspenders on top of `-b`: some
			// tmux builds still surface `run-shell -b` stdout via view-mode
			// when a captured pane is available. The daemon dispatches
			// asynchronously so nothing depends on the CLI's stdout.
			runShellCmd := fmt.Sprintf("'%s' plugin run %s %s >/dev/null 2>&1", selfBin, name, actionID)
			for _, key := range actions[actionID] {
				if key == "" {
					continue
				}
				if other, ok := reserved[key]; ok {
					log.Printf("plugin %s key %q collides with %s; last binding wins", name, key, other)
				}
				_ = tc.BindKey(key, "run-shell", "-b", runShellCmd)
			}
		}
	}
}

// installedEnabledPluginNames returns the set of plugins currently in
// StateEnabled from the local registry. Any registry read error is treated
// as an empty set (no bindings issued) plus a single log line — fail-open,
// matching the dispatcher's warnOnce policy.
func installedEnabledPluginNames(pluginsDir string, configMgr *config.Manager) map[string]struct{} {
	if configMgr == nil {
		return nil
	}
	reg := plugin.NewRegistry(pluginsDir, getStateDir(), configMgr.GetPluginsConfig())
	entries, err := reg.Runnable()
	if err != nil {
		log.Printf("plugin registry load failed: %v (plugin key bindings skipped)", err)
		return nil
	}
	out := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		out[e.Name] = struct{}{}
	}
	return out
}

// reservedOuterTmuxKeys builds a lookup of outer-tmux root keys already
// bound by core features (ActionPanel / TogglePane / SessionFilter). Used
// to warn on plugin key collisions.
func reservedOuterTmuxKeys(configMgr *config.Manager) map[string]string {
	out := map[string]string{}
	add := func(keys []string, tag string) {
		for _, k := range keys {
			if k != "" {
				out[k] = tag
			}
		}
	}
	add(configMgr.GetActionPanelKeys(), "core:action-panel")
	add(configMgr.GetTogglePaneKeys(), "core:toggle-pane")
	add(configMgr.GetSessionFilterKeys(), "core:session-filter")
	return out
}

var tuiCmd = &cobra.Command{
	Use:     "ui",
	Aliases: []string{"tui"},
	Short:   "Open the interactive TUI",
	Long:    `Open the interactive terminal user interface for managing Claude Code sessions.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		agentFlag, _ := cmd.Flags().GetString("agent")
		if err := validateTuiAgentFlag(agentFlag); err != nil {
			return err
		}

		// If running inside jin tmux session, run the TUI directly
		if os.Getenv(envJinTmux) == "1" {
			return runTUIInner()
		}
		// Otherwise, set up tmux and attach
		return runTUIWithTmux(agentFlag)
	},
}

func init() {
	rootCmd.AddCommand(tuiCmd)
	tuiCmd.Flags().String("agent", "", "Transient default adapter kind for the create form (does not modify config.default_agent)")
}

// validateTuiAgentFlag returns nil for empty (unset) or a registered kind.
// Non-empty unknown kinds surface agent.Lookup's error text unchanged so the
// user sees the same "available: ..." list as `jin session new --agent`.
func validateTuiAgentFlag(kind string) error {
	if kind == "" {
		return nil
	}
	_, err := agent.Lookup(kind)
	return err
}

// agentEnvSetter is the minimal tmux surface setTransientAgentEnv needs.
// *tmux.Client satisfies it directly; tests inject a fake.
type agentEnvSetter interface {
	SetEnvironment(session, name, value string) error
	UnsetEnvironment(session, name string) error
}

// setTransientAgentEnv writes (or clears) the outer-tmux env variable that
// tells create-popup which adapter kind to preselect. Clearing on empty flag
// prevents a stale value from a prior `jin ui --agent codex` invocation on
// the same outer tmux server from leaking into a subsequent plain `jin ui`.
func setTransientAgentEnv(tc agentEnvSetter, agentFlag string) {
	if agentFlag == "" {
		_ = tc.UnsetEnvironment(tmux.SessionName, "JIN_UI_AGENT")
		return
	}
	_ = tc.SetEnvironment(tmux.SessionName, "JIN_UI_AGENT", agentFlag)
}

// tuiInnerCommand returns the shell command for the inner TUI process.
func tuiInnerCommand() (string, error) {
	selfBin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to get executable path: %w", err)
	}
	return fmt.Sprintf("%s=1 '%s' ui", envJinTmux, selfBin), nil
}

// runTUIWithTmux creates or reattaches to a tmux session with 2-pane layout.
func runTUIWithTmux(agentFlag string) error {
	client := daemon.NewClient(getSocketPath())
	if !client.IsRunning() {
		return fmt.Errorf("daemon is not running. Start with: jin daemon start")
	}

	// Use the manager socket (jin-mgr) for the outer tmux
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
		return reattachTmux(tc, tuiInnerCmd, agentFlag)
	}

	// Create new tmux session
	return createAndAttachTmux(tc, tuiInnerCmd, agentFlag)
}

// createAndAttachTmux creates a new outer tmux session with 2-pane fixed layout and attaches.
// The outer tmux (jin-mgr) has prefix=None so all keystrokes pass through to the inner tmux.
func createAndAttachTmux(tc *tmux.Client, tuiInnerCmd, agentFlag string) error {
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

	// Create the outer tmux session with a placeholder in the "ui" window. The real
	// TUI command is respawned into this pane only AFTER the display pane and
	// JIN_DISPLAY_PANE env are in place (see RespawnPane below). Launching the TUI
	// here would race the SplitPane call: on a cold tmux server boot (e.g. right
	// after `pkill tmux`) the TUI's display-pane discovery loop in runTUIInner can
	// time out before the split lands, leaving displayPaneID empty and the right
	// pane permanently blank until the next `jin ui`.
	if err := tc.NewSessionWithCmd(tmux.SessionName, cols, rows, tmux.UIWindowName, tmux.PlaceholderCmd); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Normalize indices to 0-based (override user's .tmux.conf settings)
	_ = tc.SetOption("base-index", "0", true)
	_ = tc.SetOption("pane-base-index", "0", true)

	// Get TUI pane ID (the only pane so far, use window target to avoid index issues)
	windowTarget := tmux.SessionName + ":" + tmux.UIWindowName
	tuiPaneID, _ := tc.GetPaneID(windowTarget)

	// Configure the outer session
	_ = tc.SetupAutoCleanDeadPanes() // Safety net: auto-kill untagged dead panes
	if tuiPaneID != "" {
		_ = tc.TagManagedPane(tuiPaneID) // TUI pane survives exit
		// Label the TUI pane's top border via the shared @session_name option
		// (same mechanism the display pane uses for the current session name).
		_ = tc.SetPaneOption(tuiPaneID, "@session_name", tuiPaneBorderLabel)
	}
	_ = tc.SetOption("status", "off", true) // Hide tmux status bar
	_ = tc.SetOption("mouse", "on", true)
	_ = tc.SetOption("focus-events", "on", true)      // Enable focus reporting for Bubble Tea FocusMsg/BlurMsg
	_ = tc.SetOption("set-clipboard", "on", true)     // Enable clipboard via OSC 52 for copy-mode
	_ = tc.SetOption("allow-passthrough", "on", true) // Allow OSC 52 passthrough from inner tmux

	// Pane border: Tokyo Night palette matched to the TUI, bold on active for
	// unambiguous focus indication (tmux default green was too subtle and
	// clashed with the rest of the palette).
	applyPaneBorderStyle(tc)

	// prefix=None: prevent outer tmux from capturing user keystrokes
	_ = tc.SetOption("prefix", "None", true)
	_ = tc.SetOption("prefix2", "None", true)

	// Create right pane (75%) for session display.
	// Split using window target (not pane index) to avoid pane-base-index issues.
	_, _ = tc.SplitPane(windowTarget, tmux.SplitOptions{Direction: "right", Size: "75%", Cmd: tmux.PlaceholderCmd})

	// After split, the new pane (display) is the active pane. Get its ID.
	displayPaneID, _ := tc.GetPaneID(windowTarget)
	if displayPaneID != "" {
		_ = tc.TagManagedPane(displayPaneID) // Display pane survives exit
	}

	// Store pane IDs for runTUIInner to use
	if tuiPaneID != "" {
		_ = tc.SetEnvironment(tmux.SessionName, "JIN_TUI_PANE", tuiPaneID)
	}
	if displayPaneID != "" {
		_ = tc.SetEnvironment(tmux.SessionName, "JIN_DISPLAY_PANE", displayPaneID)
	}
	setTransientAgentEnv(tc, agentFlag)
	applyTogglePaneBinding(tc, configMgr, displayPaneID)
	selfBin, _ := os.Executable()
	applyActionPanelBinding(tc, configMgr, selfBin)
	applySessionFilterBinding(tc, configMgr, selfBin)
	applyPluginActionBindings(tc, configMgr, selfBin, func() map[string]struct{} {
		return installedEnabledPluginNames(paths.Plugins(), configMgr)
	})
	// Propagate SSH_AUTH_SOCK to tmux session so popups can access it
	if sshAuthSock := os.Getenv("SSH_AUTH_SOCK"); sshAuthSock != "" {
		_ = tc.SetEnvironment(tmux.SessionName, "SSH_AUTH_SOCK", sshAuthSock)
	}

	// Now that both panes exist and JIN_DISPLAY_PANE is set, respawn the real TUI
	// command into the left pane and focus it. Doing this last (instead of at
	// NewSessionWithCmd) guarantees the TUI process sees a fully-built layout on its
	// first look, so its display-pane discovery never times out on a cold start.
	if tuiPaneID != "" {
		_ = tc.RespawnPane(tuiPaneID, tuiInnerCmd)
		_ = tc.SelectPane(tuiPaneID)
	}

	// Bind detach key to switch to TUI pane (prefix-free binding, works with prefix=None)
	_ = tc.BindKey(detachTmuxKey, "select-pane", "-L")

	return attachToSession(tc)
}

// reattachTmux reattaches to an existing outer tmux session, respawning dead panes.
func reattachTmux(tc *tmux.Client, tuiInnerCmd, agentFlag string) error {
	// Load config so we can re-apply outer-tmux bindings (toggle_pane etc.) on reattach
	configMgr, _ := config.NewManager(getConfigDir())
	// Ensure pane-died hook is active (handles upgrade from older version)
	_ = tc.SetupAutoCleanDeadPanes()
	// Update SSH_AUTH_SOCK in tmux session (may have changed on reconnect)
	if sshAuthSock := os.Getenv("SSH_AUTH_SOCK"); sshAuthSock != "" {
		_ = tc.SetEnvironment(tmux.SessionName, "SSH_AUTH_SOCK", sshAuthSock)
	}
	_ = tc.SetOption("focus-events", "on", true)      // Ensure focus reporting is enabled
	_ = tc.SetOption("set-clipboard", "on", true)     // Enable clipboard via OSC 52 for copy-mode
	_ = tc.SetOption("allow-passthrough", "on", true) // Allow OSC 52 passthrough from inner tmux
	// Re-apply pane border styling in case the outer tmux server was restarted
	// or the options were tampered with between sessions.
	applyPaneBorderStyle(tc)

	tuiPaneID := tc.GetEnvironment(tmux.SessionName, "JIN_TUI_PANE")

	if tuiPaneID != "" {
		if tc.IsPaneDead(tuiPaneID) {
			// TUI pane exists but dead → respawn it
			_ = tc.RespawnPane(tuiPaneID, tuiInnerCmd)
		}
		// Re-apply the border label in case the outer tmux server was
		// restarted between sessions and cleared the per-pane option.
		_ = tc.SetPaneOption(tuiPaneID, "@session_name", tuiPaneBorderLabel)
		// Select TUI pane
		_ = tc.SelectPane(tuiPaneID)
	} else {
		// No tracked TUI pane → respawn in UI window pane 0
		_ = tc.RespawnPane(tmux.UITarget(0), tuiInnerCmd)
		_ = tc.SelectWindow(tmux.SessionName + ":" + tmux.UIWindowName)
	}

	// Restore right pane if dead
	displayPaneID := tc.GetEnvironment(tmux.SessionName, "JIN_DISPLAY_PANE")
	if displayPaneID != "" && tc.IsPaneDead(displayPaneID) {
		_ = tc.RespawnPane(displayPaneID, tmux.PlaceholderCmd)
	}
	setTransientAgentEnv(tc, agentFlag)
	applyTogglePaneBinding(tc, configMgr, displayPaneID)
	selfBin, _ := os.Executable()
	applyActionPanelBinding(tc, configMgr, selfBin)
	applySessionFilterBinding(tc, configMgr, selfBin)
	applyPluginActionBindings(tc, configMgr, selfBin, func() map[string]struct{} {
		return installedEnabledPluginNames(paths.Plugins(), configMgr)
	})

	return attachToSession(tc)
}

// attachToSession attaches to the tmux session and blocks until detach.
func attachToSession(tc *tmux.Client) error {
	recordOuterLocation(tc)

	attachCmd := tc.AttachCmd(tmux.SessionName)
	attachCmd.Stdin = os.Stdin
	attachCmd.Stdout = os.Stdout
	attachCmd.Stderr = os.Stderr
	return attachCmd.Run()
}

// recordOuterLocation stores where `jin ui` was launched from onto the jin-mgr
// session env, so `jin session focus` can jump the outer tmux back to the TUI
// window. When launched outside tmux, any stale records from a prior run are
// cleared. Best-effort: failures never block TUI startup.
func recordOuterLocation(tc *tmux.Client) {
	outerTmux := os.Getenv("TMUX")
	if outerTmux == "" {
		_ = tc.UnsetEnvironment(tmux.SessionName, "JIN_UI_OUTER_SOCKET")
		_ = tc.UnsetEnvironment(tmux.SessionName, "JIN_UI_OUTER_PANE")
		return
	}
	_ = tc.SetEnvironment(tmux.SessionName, "JIN_UI_OUTER_SOCKET", tmux.SocketPathFromEnv(outerTmux))
	_ = tc.SetEnvironment(tmux.SessionName, "JIN_UI_OUTER_PANE", os.Getenv("TMUX_PANE"))
}

// runTUIInner runs the Bubble Tea TUI inside the outer tmux pane.
func runTUIInner() error {
	client := daemon.NewClient(getSocketPath())
	if !client.IsRunning() {
		return fmt.Errorf("daemon is not running. Start with: jin daemon start")
	}

	// Use the manager socket (jin-mgr) for the outer tmux
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
		tuiPaneID = tc.GetEnvironment(tmux.SessionName, "JIN_TUI_PANE")
	}
	if tuiPaneID != "" {
		_ = tc.SetEnvironment(tmux.SessionName, "JIN_TUI_PANE", tuiPaneID)
		_ = tc.TagManagedPane(tuiPaneID)
		// Rebind detach key to focus TUI pane by ID (works from any pane)
		_ = tc.BindKey(detachTmuxKey, "run-shell",
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
			displayPaneID = tc.GetEnvironment(tmux.SessionName, "JIN_DISPLAY_PANE")
		}
		if displayPaneID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if displayPaneID != "" {
		_ = tc.SetEnvironment(tmux.SessionName, "JIN_DISPLAY_PANE", displayPaneID)
	}

	// Create inner tmux client (-L jin) for switch-client operations
	innerTC, _ := tmux.NewClient()

	model := tui.NewModelWithTmux(client, tc, innerTC, tuiPaneID, displayPaneID)

	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithReportFocus())
	if _, err := p.Run(); err != nil {
		return err
	}

	// Detach the client instead of killing the session.
	// The outer tmux session stays alive with CC processes running in inner tmux.
	_ = tc.DetachClient(tmux.SessionName)
	return nil
}
