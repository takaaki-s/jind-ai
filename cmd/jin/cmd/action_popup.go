package cmd

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/action"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/paths"
	"github.com/takaaki-s/jind-ai/internal/plugin"
	"github.com/takaaki-s/jind-ai/internal/tmux"
	"github.com/takaaki-s/jind-ai/internal/tui"
)

var actionPopupCmd = &cobra.Command{
	Use:    "action-popup",
	Short:  "Internal: action palette for tmux popup",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if tc, err := tmux.NewMgrClient(); err == nil {
			ensureSSHAuthSockFromTmux(tc)
		}

		configMgr, _ := config.NewManager(getConfigDir())
		kb := actionKeyBindingsFromConfig(configMgr)
		core := action.CoreActions(kb)

		var plugins []action.Action
		if configMgr != nil {
			reg := plugin.NewRegistry(paths.Plugins(), getStateDir(), configMgr.GetPluginsConfig())
			if entries, err := reg.Runnable(); err == nil {
				plugins = action.PluginActions(entries, configMgr.GetPluginKeybindings())
			}
		}

		var cursorID, cursorDesc string
		if tc, err := tmux.NewMgrClient(); err == nil {
			cursorID = tc.GetEnvironment(tmux.SessionName, "JIN_CURSOR_SESSION")
		}
		if cursorID != "" {
			client := daemon.NewClient(getSocketPath())
			if info, err := client.Get(cursorID); err == nil {
				cursorDesc = info.Description
			}
		}

		model := tui.NewPaletteModel(core, plugins, cursorID, cursorDesc)
		p := tea.NewProgram(model, tea.WithAltScreen())
		finalModel, err := p.Run()
		if err != nil {
			return err
		}

		// If user selected an action, set tmux env var for parent TUI to pick up
		if m, ok := finalModel.(tui.PaletteModel); ok && m.Selected() != "" {
			if tc, err := tmux.NewMgrClient(); err == nil {
				_ = tc.SetEnvironment(tmux.SessionName, "JIN_ACTION_ID", m.Selected())
			}
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(actionPopupCmd)
}

// actionKeyBindingsFromConfig builds the narrow action.KeyBindings used to
// resolve palette Shortcut display from the full config.KeybindingsConfig.
// A nil manager (config load failure) yields empty bindings so CoreActions
// still returns rows, just without shortcut hints.
//
// TogglePane and Search are resolved via their dedicated getters because
// GetKeybindings intentionally omits both from its len==0 fallback: they
// preserve a nil↔empty distinction (nil=default binding, empty=user
// disabled). Reading kb.TogglePane / kb.Search directly leaks a raw nil
// into the palette on any install without a config file, blanking the
// corresponding shortcut hint.
func actionKeyBindingsFromConfig(mgr *config.Manager) action.KeyBindings {
	if mgr == nil {
		return action.KeyBindings{}
	}
	kb := mgr.GetKeybindings()
	return action.KeyBindings{
		New:        kb.New,
		Kill:       kb.Kill,
		Delete:     kb.Delete,
		Refresh:    kb.Refresh,
		Vscode:     kb.Vscode,
		Help:       kb.Help,
		TogglePane: mgr.GetTogglePaneKeys(),
		Search:     mgr.GetSessionFilterKeys(),
	}
}
