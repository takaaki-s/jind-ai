package cmd

import (
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
		if configMgr != nil {
			keybindings = configMgr.GetKeybindings()
			detachKeyHint = configMgr.GetDetachKeyHint()
			if apk := configMgr.GetActionPanelKeys(); len(apk) > 0 {
				actionPanelHint = action.FormatKeyHint(apk[0])
			}
		} else {
			keybindings = config.DefaultKeybindings()
			detachKeyHint = "Ctrl+]"
		}
		keys := tui.NewKeyMap(keybindings)

		model := tui.NewHelpModel(keys, detachKeyHint, actionPanelHint)
		p := tea.NewProgram(model, tea.WithAltScreen())
		_, err := p.Run()
		return err
	},
}

func init() {
	rootCmd.AddCommand(helpPopupCmd)
}
