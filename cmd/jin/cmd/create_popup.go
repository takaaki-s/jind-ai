package cmd

import (
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/tmux"
	"github.com/takaaki-s/jind-ai/internal/tui"
)

var createPopupCmd = &cobra.Command{
	Use:    "create-popup",
	Short:  "Internal: session creation form for tmux popup",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Ensure SSH_AUTH_SOCK is available (tmux popup may not inherit it),
		// and read the transient default adapter kind set by `jin ui --agent`.
		// One tmux client shared for both to keep the popup fast.
		initialAgentKind := ""
		if tc, err := tmux.NewMgrClient(); err == nil {
			if os.Getenv("SSH_AUTH_SOCK") == "" {
				if sock := tc.GetEnvironment(tmux.SessionName, "SSH_AUTH_SOCK"); sock != "" {
					os.Setenv("SSH_AUTH_SOCK", sock)
				}
			}
			initialAgentKind = tc.GetEnvironment(tmux.SessionName, "JIN_UI_AGENT")
		}

		model := tui.NewCreateFormModel(getSocketPath(), initialAgentKind)
		p := tea.NewProgram(model, tea.WithAltScreen())
		_, err := p.Run()
		return err
	},
}

func init() {
	rootCmd.AddCommand(createPopupCmd)
}
