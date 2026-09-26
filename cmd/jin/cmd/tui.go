package cmd

import (
	"fmt"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/jinenv"
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
	paneBorderFormat        = "#{?#{" + tmux.PaneLabelOption + "}, #[bold]#{" + tmux.PaneLabelOption + "}#[nobold] ,}"
	// tuiPaneBorderLabel is the pane-border-format text shown on the TUI (left)
	// pane. Deliberately empty: the session list renders its own header row
	// directly below the border, carrying strictly more than a label could —
	// counts and the permission warning colour. paneBorderFormat is a
	// conditional, so an empty label leaves a plain border. Labelling only the
	// display pane also makes "labelled == live session" a useful asymmetry.
	//
	// Do NOT delete the SetPaneOption calls that write this value. @session_name
	// is a tmux-side pane option that outlives the process, so dropping the
	// writes would leave the old "sessions" text on any tmux server an earlier
	// build already labelled. Writing the empty string is what clears it.
	tuiPaneBorderLabel = ""
)

// applyPaneBorderStyle applies the modern pane-border styling to the outer
// tmux server. Safe to call multiple times (idempotent).
func applyPaneBorderStyle(tc *tmux.Client) {
	_ = tc.SetOption("pane-active-border-style", activePaneBorderStyle, true)
	_ = tc.SetOption("pane-border-style", inactivePaneBorderStyle, true)
	_ = tc.SetOption("pane-border-status", "top", true)
	_ = tc.SetOption("pane-border-format", paneBorderFormat, true)
}

// uiIdentity is the jin that this UI, and everything it opens, must reach.
//
// This process is where that answer is made: it resolves a socket from the
// user's environment and checks a daemon is listening there before building
// anything. Every other process in the tree gets the answer from here, because
// none of them is in a position to make it — they run in panes and popups of
// the outer tmux server, so left alone they read that server's environment. The
// two were measured disagreeing: with a stale JIN_SOCKET on the server, `jin
// ui` validated a live daemon and the TUI then died against a dead one, 3 of 3
// trials; with the daemons swapped, `jin ui` refused to start while the daemon
// the TUI would have used was up, also 3 of 3.
//
// BinPath comes from JIN_BIN and nowhere else, exactly as callerPaneEnv takes
// it. A `jin ui` launched from a plain shell has none, and emitting it empty is
// the point rather than a shortfall: an inherited stale path exits 127, while
// `"${JIN_BIN:-jin}"` substitutes on empty.
func uiIdentity() jinenv.Identity {
	return jinenv.Identity{
		SocketPath: getSocketPath(),
		BinPath:    os.Getenv("JIN_BIN"),
		Debug:      debugEnabled(),
	}
}

// uiChildEnv is the environment every process this UI starts is given, and the
// single list both destinations below draw from — the TUI pane's -e and the
// outer tmux session. Two lists would be two chances to disagree about a key,
// and a key only one of them carries is a process that reads the tmux server's
// value instead.
//
// No session, because nothing here belongs to one: emitting JIN_SESSION_ID
// empty is also what clears a stale id the outer server may hold, which
// jinenv.Identity.TmuxEnviron records as the value to fear — a leftover UUID is
// plausible where an absent one is not. No plugin chain either, and that one is
// cleared by TmuxEnviron rather than here, for every pane jind-ai opens.
func uiChildEnv() []string {
	return uiIdentity().TmuxEnviron("")
}

// tuiPaneRespawner is the minimal tmux surface respawnTUIPane needs.
// *tmux.Client satisfies it directly; tests inject a fake.
type tuiPaneRespawner interface {
	RespawnPane(target, shellCmd string, env []string) error
}

// respawnTUIPane starts the inner TUI in a pane of the outer tmux server.
//
// The environment is not a parameter, so the three call sites that start this
// pane — a fresh layout, a dead pane on reattach, and the untracked-pane
// fallback — cannot differ in what they pass. `tc.RespawnPane(target, cmd,
// nil)` remains callable, and is called legitimately below for the display
// pane; reaching for it here would not fail, the pane would simply come up on
// whatever daemon the tmux server names, silently.
func respawnTUIPane(tc tuiPaneRespawner, target, shellCmd string) error {
	return tc.RespawnPane(target, shellCmd, uiChildEnv())
}

// identityMoved reports whether the outer tmux session already names a
// different jin than this invocation resolved. prev is that session's recorded
// JIN_SOCKET, which the caller reads with the other entries it needs.
//
// An empty entry is not a move: it means a first run, or a server written by a
// build older than the identity write, and in both cases the session is about
// to be given this identity rather than disagreeing with it.
//
// Only JIN_SOCKET is compared — it is what decides which daemon answers, and
// JIN_BIN and JIN_DEBUG cannot select a different jind-ai on their own.
func identityMoved(prev string) bool {
	return prev != "" && prev != uiIdentity().SocketPath
}

// applyOuterSessionIdentity publishes the same identity onto the outer tmux
// session, which is the floor under everything tmux starts without jind-ai in
// the loop: the popups bound to M-p / M-f, and the `jin plugin run` a plugin key
// binding fires through run-shell. Those are issued once at startup as tmux
// commands, so they carry no environment of their own and would otherwise read
// the server's.
//
// Written as values rather than removed, empty included, for the reason
// jinenv.Identity.TmuxEnviron gives: a session entry set to the empty string
// masks the server's global one, while unsetting it merely stops overriding.
//
// It takes agentEnvSetter rather than a set-only interface although it never
// unsets: having UnsetEnvironment in reach is what lets a test assert that
// nothing here unsets, which is the mistake this function has to keep not
// making.
func applyOuterSessionIdentity(tc agentEnvSetter) {
	for _, assignment := range uiChildEnv() {
		// Unreachable today: uiChildEnv is two lines above and every entry it
		// builds is KEY=VALUE. Kept anyway, where internal/tui dropped a
		// comparable nil guard, because the failure differs — that one panicked
		// on the way to the guard, while a bare word here would quietly set a
		// variable named after the whole token.
		name, value, ok := strings.Cut(assignment, "=")
		if !ok {
			continue
		}
		_ = tc.SetEnvironment(tmux.SessionName, name, value)
	}
}

// uiSessionOps is the outer-tmux surface applyOuterSessionSetup needs: the
// environment writes and the key bindings. *tmux.Client satisfies it; tests
// inject a fake.
type uiSessionOps interface {
	agentEnvSetter
	BindKey(key string, cmdArgs ...string) error
}

// applyOuterSessionSetup applies everything the outer tmux session needs that
// does not depend on how this invocation reached it: the identity every process
// tmux starts from this session is given, and the bindings that start them.
//
// One function rather than the same calls written out in createAndAttachTmux
// and reattachTmux, which had five of them each. Adding a sixth to one and not
// the other would not fail — the UI would come up on whichever daemon the tmux
// server names, silently. The orchestrators still decide *when* to call it.
func applyOuterSessionSetup(tc uiSessionOps, s outerSessionSetup) {
	setTransientAgentEnv(tc, s.AgentFlag)
	applyOuterSessionIdentity(tc)
	applyTogglePaneBinding(tc, s.ConfigMgr, s.DisplayPaneID)
	applyActionPanelBinding(tc, s.ConfigMgr, s.SelfBin)
	applySessionFilterBinding(tc, s.ConfigMgr, s.SelfBin)
	applyPluginActionBindings(tc, s.ConfigMgr, s.SelfBin, s.InstalledPlugins)
}

// outerSessionSetup is what applyOuterSessionSetup needs, as named fields
// rather than three strings in a row.
//
// The three are interchangeable to the compiler and not to tmux: transposing
// two of them binds a key that resizes a pane named `/usr/local/bin/jin`, or
// opens a popup by running `'%7' action-popup`, and neither reports anything.
// Naming them makes such a swap visible where it is written; it does not
// prevent one, since every field is a string — measured, after a comment here
// claimed otherwise. What catches it is the e2e pass over both orchestrators,
// and covering only one of the two left the transpositions alive on the other.
type outerSessionSetup struct {
	ConfigMgr        *config.Manager
	AgentFlag        string
	SelfBin          string
	DisplayPaneID    string
	InstalledPlugins installedPluginSetFn
}

// localPluginSet is the InstalledPlugins both orchestrators pass: the on-disk
// registry, read when the bindings are issued rather than now. Spelled once for
// the reason applyOuterSessionSetup exists — two copies are two chances to name
// a different directory, and a set read from the wrong one issues no bindings
// and says nothing.
func localPluginSet(configMgr *config.Manager) installedPluginSetFn {
	return func() map[string]struct{} {
		return installedEnabledPluginNames(paths.Plugins(), configMgr)
	}
}

// tuiModelForPane builds the Model the inner TUI runs, with this process's
// identity supplied rather than asked for. Split out from runTUIInner because
// runTUIInner cannot be entered from a test — it takes over the terminal — and
// which jin the Model was given is the answer everything the TUI opens inherits.
func tuiModelForPane(client *daemon.Client, tc, innerTC *tmux.Client, tuiPaneID, displayPaneID string) tui.Model {
	return tui.NewModelWithTmux(client, tc, innerTC, tuiPaneID, displayPaneID, uiIdentity())
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
// the switch-session popup. Idempotent: re-issuing bind-key overwrites the
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
			"-T", " Switch Session ",
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
//
// Bindings are issued only for plugins currently installed AND enabled
// (StateEnabled). Uninstalled / broken / incompatible plugins are silently
// skipped with a single log line each — config vs. installed set drift is
// common in dev environments and must never block TUI startup. Action IDs are
// not validated against the manifest here: a typo'd action fails at `jin plugin
// run` time with the daemon's error listing valid IDs. Key collisions with core
// outer-tmux bindings are warned once (see reservedOuterTmuxKeys) but not
// blocked; tmux's last-write-wins semantics apply.
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
			// when a captured pane is available.
			//
			// It discards the errors too, which is why this is the one caller
			// of `jin plugin run` that can fail in total silence. A run the
			// dispatcher declines is recorded on the daemon side for that
			// reason — see EventDispatcher.RunAction — so a binding refused
			// there is diagnosable from plugin-debug.log, on a daemon started
			// with JIN_DEBUG=1. Only that far: a request the daemon rejects
			// before the dispatcher sees it leaves nothing, and neither does a
			// CLI that cannot reach a daemon at all.
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
	Long:    `Open the interactive terminal user interface for managing coding-agent sessions.`,
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
		// Write the TUI pane's border label via the shared @session_name option
		// (same mechanism the display pane uses for the current session name).
		// The label is empty by design — see tuiPaneBorderLabel; the write stays
		// so a server still holding an older build's label gets cleared.
		_ = tc.SetPaneOption(tuiPaneID, tmux.PaneLabelOption, tuiPaneBorderLabel)
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
	selfBin, _ := os.Executable()
	applyOuterSessionSetup(tc, outerSessionSetup{
		ConfigMgr:        configMgr,
		AgentFlag:        agentFlag,
		SelfBin:          selfBin,
		DisplayPaneID:    displayPaneID,
		InstalledPlugins: localPluginSet(configMgr),
	})
	// Propagate SSH_AUTH_SOCK to tmux session so popups can access it
	if sshAuthSock := os.Getenv("SSH_AUTH_SOCK"); sshAuthSock != "" {
		_ = tc.SetEnvironment(tmux.SessionName, "SSH_AUTH_SOCK", sshAuthSock)
	}

	// Now that both panes exist and JIN_DISPLAY_PANE is set, respawn the real TUI
	// command into the left pane and focus it. Doing this last (instead of at
	// NewSessionWithCmd) guarantees the TUI process sees a fully-built layout on
	// its first look, so its display-pane discovery never times out on a cold
	// start.
	//
	// The -e respawnTUIPane adds is redundant here, since the session environment
	// is already written and both are uiChildEnv. It stays because reattach runs
	// the opposite order, where it is the only source.
	if tuiPaneID != "" {
		_ = respawnTUIPane(tc, tuiPaneID, tuiInnerCmd)
		_ = tc.SelectPane(tuiPaneID)
	}

	// Bind detach key to switch to TUI pane (prefix-free binding, works with prefix=None)
	_ = tc.BindKey(detachTmuxKey, "select-pane", "-L")

	return attachToSessionFn(tc)
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

	// One tmux call for the three entries this function reads. Reattach is a
	// path a human waits on, and each GetEnvironment is its own fork/exec.
	sessionEnv := tc.ListEnvironment(tmux.SessionName)
	tuiPaneID := sessionEnv["JIN_TUI_PANE"]

	if tuiPaneID != "" {
		// Dead, or alive on a jin that has moved. The second restarts something
		// the user is watching, which earlier builds refused to do — but what
		// that refusal left behind was measured: a plugin key binding followed
		// the new session environment 3 of 3 while the running TUI stayed on the
		// old daemon, and such a binding reports nothing when it lands on the
		// wrong one (applyPluginActionBindings has why). A restart the user can
		// see is the cheaper failure. The popups tmux opens were never part of
		// it — they inherit the firing client's environment, 5 of 5.
		if tc.IsPaneDead(tuiPaneID) || identityMoved(sessionEnv["JIN_SOCKET"]) {
			_ = respawnTUIPane(tc, tuiPaneID, tuiInnerCmd)
		}
		// Re-apply the border label in case the outer tmux server was
		// restarted between sessions and cleared the per-pane option. Also the
		// path that clears a stale label left behind by an older build, so keep
		// the write even though tuiPaneBorderLabel is empty.
		_ = tc.SetPaneOption(tuiPaneID, tmux.PaneLabelOption, tuiPaneBorderLabel)
		// Select TUI pane
		_ = tc.SelectPane(tuiPaneID)
	} else {
		// No tracked TUI pane → respawn in UI window pane 0
		_ = respawnTUIPane(tc, tmux.UITarget(0), tuiInnerCmd)
		_ = tc.SelectWindow(tmux.SessionName + ":" + tmux.UIWindowName)
	}

	// Restore right pane if dead. Pane options survive respawn-pane, so the
	// label has to be cleared alongside it — otherwise the placeholder comes
	// back wearing the name of whatever session the pane died on.
	displayPaneID := sessionEnv["JIN_DISPLAY_PANE"]
	if displayPaneID != "" && tc.IsPaneDead(displayPaneID) {
		_ = tc.RespawnPane(displayPaneID, tmux.PlaceholderCmd, nil)
		_ = tc.SetPaneOption(displayPaneID, tmux.PaneLabelOption, "")
	}
	// Republishing the identity matters most here: reattach is the path where the
	// server predates this invocation, so it is the one most likely to be holding
	// another jin's values. It runs after the respawns above rather than before,
	// and the order is not load-bearing for the pane — a respawned one is handed
	// the same values as -e, which beat the session environment whenever they
	// disagree. It IS load-bearing for the test that reads that pane's
	// environment: -e and the session entry carry identical values, so the only
	// thing making the pane's copy attributable to -e is that nothing else has
	// written it yet. Move this call above the respawns and that test still
	// passes and stops meaning anything.
	selfBin, _ := os.Executable()
	applyOuterSessionSetup(tc, outerSessionSetup{
		ConfigMgr:        configMgr,
		AgentFlag:        agentFlag,
		SelfBin:          selfBin,
		DisplayPaneID:    displayPaneID,
		InstalledPlugins: localPluginSet(configMgr),
	})

	return attachToSessionFn(tc)
}

// attachToSessionFn is the last thing createAndAttachTmux and reattachTmux do,
// and the reason neither could be entered from a test: it takes over the
// terminal and blocks until the user detaches. A function value so a test can
// stop there and inspect what the orchestrator set up on the way — chiefly
// whether it published this process's identity, which nothing else observes.
// Production never reassigns it.
var attachToSessionFn = attachToSession

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
			fmt.Sprintf("tmux -L %s select-pane -t %s", tc.GetSocketName(), tuiPaneID))
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

	model := tuiModelForPane(client, tc, innerTC, tuiPaneID, displayPaneID)

	// Cell-motion is the cheapest mouse mode Bubble Tea offers that still
	// reports button presses and wheel notches. Trade-off: while the TUI pane
	// owns the mouse, terminal drag-to-select there needs Shift held.
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithReportFocus(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		return err
	}

	// Detach the client instead of killing the session.
	// The outer tmux session stays alive with CC processes running in inner tmux.
	_ = tc.DetachClient(tmux.SessionName)
	return nil
}
