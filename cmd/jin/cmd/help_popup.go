package cmd

import (
	"sort"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/action"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/tui"
)

var helpPopupCmd = &cobra.Command{
	Use:    "help-popup",
	Short:  "Internal: help view for tmux popup",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		configMgr, _ := config.NewManager(getConfigDir())
		var keybindings config.KeybindingsConfig
		var detachKeyHint string
		var actionPanelHint string
		var sessionFilterHint string
		var pluginHints []tui.PluginBindingHint
		if configMgr != nil {
			keybindings = configMgr.GetKeybindings()
			detachKeyHint = configMgr.GetDetachKeyHint()
			if apk := configMgr.GetActionPanelKeys(); len(apk) > 0 {
				actionPanelHint = action.FormatKeyHint(apk[0])
			}
			if sfk := configMgr.GetSessionFilterKeys(); len(sfk) > 0 {
				sessionFilterHint = action.FormatKeyHint(sfk[0])
			}
			pluginHints = pluginBindingHints(configMgr.GetPluginKeybindings())
		} else {
			keybindings = config.DefaultKeybindings()
			detachKeyHint = "Ctrl+]"
		}
		keys := tui.NewKeyMap(keybindings)

		model := tui.NewHelpModel(keys, detachKeyHint, actionPanelHint, sessionFilterHint, pluginHints)
		p := tea.NewProgram(model, tea.WithAltScreen())
		_, err := p.Run()
		return err
	},
}

func init() {
	rootCmd.AddCommand(helpPopupCmd)
}

// pluginBindingHints flattens the plugin → action → keys map returned by
// config.Manager.GetPluginKeybindings into one help line per bound action,
// sorted by name for stable display. The default action renders as
// "<plugin> / default" — the action ID stays visible so the help view and
// the config file speak the same vocabulary. Actions with no keys are
// omitted; only the first key is shown (same convention as the palette).
func pluginBindingHints(bindings map[string]map[string][]string) []tui.PluginBindingHint {
	var hints []tui.PluginBindingHint
	for name, actions := range bindings {
		for actionID, keys := range actions {
			if len(keys) == 0 {
				continue
			}
			hints = append(hints, tui.PluginBindingHint{
				KeyHint: action.FormatKeyHint(keys[0]),
				Name:    name + " / " + actionID,
			})
		}
	}
	sort.Slice(hints, func(i, j int) bool {
		return hints[i].Name < hints[j].Name
	})
	return hints
}
