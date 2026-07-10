package cmd

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/tmux"
	"github.com/takaaki-s/jind-ai/internal/tui"
)

var notifyPopupCmd = &cobra.Command{
	Use:    "notify-popup",
	Short:  "Internal: notification history for tmux popup",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		client := daemon.NewClient(getSocketPath())
		entries, err := client.NotificationHistory()
		if err != nil {
			return fmt.Errorf("failed to get notification history: %w", err)
		}

		model := tui.NewNotifyModel(entries)
		p := tea.NewProgram(model, tea.WithAltScreen())
		finalModel, err := p.Run()
		if err != nil {
			return err
		}

		// If user selected an entry, set tmux env var for parent TUI to pick up
		if m, ok := finalModel.(tui.NotifyModel); ok && m.Selected() != "" {
			tc, err := tmux.NewMgrClient()
			if err == nil {
				_ = tc.SetEnvironment(tmux.SessionName, "JIN_NOTIFY_SESSION", m.Selected())
			}
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(notifyPopupCmd)
}
