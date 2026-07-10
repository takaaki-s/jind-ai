package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/tmux"
)

var attachCmd = &cobra.Command{
	Use:               "attach <selector>",
	Short:             "Attach to a session",
	Long:              `Attach to a Claude Code session. Stopped sessions are automatically resumed. The selector may be an ID prefix or a description substring (case-insensitive).`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		client := daemon.NewClient(getSocketPath())

		sess, err := resolveSelector(client, args[0])
		if err != nil {
			return err
		}

		if sess.Status == session.StatusCreating {
			return fmt.Errorf("cannot attach to session being created")
		}

		// Start stopped sessions (resume)
		if sess.Status == session.StatusStopped {
			if err := client.Start(sess.ID); err != nil {
				return fmt.Errorf("failed to start session: %w", err)
			}
			fmt.Printf("Resuming session: %s\n", sess.Description)
		}

		// Determine tmux window name
		windowName := sess.TmuxWindowName
		if windowName == "" {
			windowName = tmux.InnerSessionName(sess.ID)
		}

		tc, err := tmux.NewClient()
		if err != nil {
			return fmt.Errorf("tmux not available: %w", err)
		}

		attachExec := tc.AttachCmd(windowName)
		attachExec.Stdin = os.Stdin
		attachExec.Stdout = os.Stdout
		attachExec.Stderr = os.Stderr
		return attachExec.Run()
	},
}

func init() {
	sessionCmd.AddCommand(attachCmd)
}
