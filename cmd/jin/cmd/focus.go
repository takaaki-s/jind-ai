package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jindaiko/internal/daemon"
	"github.com/takaaki-s/jindaiko/internal/tmux"
)

var focusCmd = &cobra.Command{
	Use:   "focus <selector>",
	Short: "Switch the running TUI to display a session",
	Long: `Switch the running TUI's display pane to the given session. If the TUI was
launched from inside an outer tmux, also best-effort bring that tmux window to
the front. The selector may be an ID prefix or a description substring
(case-insensitive), same as other 'jin session' commands.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		selector := args[0]

		client := daemon.NewClient(getSocketPath())
		sessionID, sessionDesc, err := resolveSession(client, selector)
		if err != nil {
			return err
		}

		tc, err := tmux.NewMgrClient()
		if err != nil {
			return fmt.Errorf("tmux is required: %w", err)
		}
		if !tc.HasSession(tmux.SessionName) {
			return fmt.Errorf("TUI is not running. Start with: jin ui")
		}

		if err := tc.SetEnvironment(tmux.SessionName, "JIN_NOTIFY_SESSION", sessionID); err != nil {
			return err
		}

		jumpOuterToTUI(tc)

		fmt.Fprintf(cmd.OutOrStdout(), "Focused session: %s\n", sessionDesc)
		return nil
	},
}

// jumpOuterToTUI best-effort brings the outer tmux window hosting the TUI to the
// front. The display switch already succeeded via JIN_NOTIFY_SESSION, so every
// failure here (missing/stale records, dead outer pane) is silently ignored.
func jumpOuterToTUI(mgr *tmux.Client) {
	socketPath := mgr.GetEnvironment(tmux.SessionName, "JIN_UI_OUTER_SOCKET")
	pane := mgr.GetEnvironment(tmux.SessionName, "JIN_UI_OUTER_PANE")
	if socketPath == "" || pane == "" {
		return
	}
	outer, err := tmux.NewClientWithSocketPath(socketPath)
	if err != nil {
		return
	}
	_ = outer.SelectWindow(pane)
	_ = outer.SelectPane(pane)
}

func init() {
	sessionCmd.AddCommand(focusCmd)
}
