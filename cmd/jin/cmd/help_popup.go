package cmd

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/takaaki-s/jindaiko/internal/config"
	"github.com/takaaki-s/jindaiko/internal/tui"
)

var helpPopupCmd = &cobra.Command{
	Use:    "help-popup",
	Short:  "Internal: help view for tmux popup",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		configMgr, _ := config.NewManager(getConfigDir())
		var keybindings config.KeybindingsConfig
		var detachKeyHint string
		if configMgr != nil {
			keybindings = configMgr.GetKeybindings()
			detachKeyHint = configMgr.GetDetachKeyHint()
		} else {
			keybindings = config.DefaultKeybindings()
			detachKeyHint = "Ctrl+]"
		}
		keys := tui.NewKeyMap(keybindings)

		model := tui.NewHelpModel(keys, detachKeyHint)
		p := tea.NewProgram(model, tea.WithAltScreen())
		_, err := p.Run()
		return err
	},
}

func init() {
	rootCmd.AddCommand(helpPopupCmd)
}
