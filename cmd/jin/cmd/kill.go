package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/exitcode"
	"github.com/takaaki-s/jind-ai/internal/session"
)

// resolveSession is a thin wrapper around resolveSelector that returns the
// (id, description) tuple used by legacy call sites. New callers should
// prefer resolveSelector directly.
func resolveSession(client *daemon.Client, selector string) (id, desc string, err error) {
	sess, err := resolveSelector(client, selector)
	if err != nil {
		return "", "", err
	}
	return sess.ID, sess.Description, nil
}

type actionResult struct {
	Success     bool   `json:"success"`
	ID          string `json:"id"`
	Description string `json:"description"`
}

var killCmd = &cobra.Command{
	Use:               "kill <selector>",
	Short:             "Kill a running session",
	Long:              `Kill a running Claude Code session without deleting it. The selector may be an ID prefix or a description substring (case-insensitive).`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		selector := args[0]
		client := daemon.NewClient(getSocketPath())

		sessionID, sessionDesc, err := resolveSession(client, selector)
		if err != nil {
			return err
		}

		if err := client.Kill(sessionID); err != nil {
			return err
		}
		if jsonOutput {
			return renderActionResultJSON(os.Stdout, actionResult{Success: true, ID: sessionID, Description: sessionDesc})
		}
		fmt.Printf("Killed session: %s\n", sessionDesc)
		return nil
	},
}

var deleteCmd = &cobra.Command{
	Use:               "delete <selector>",
	Aliases:           []string{"rm"},
	Short:             "Delete a session",
	Long:              `Delete a Claude Code session. This will kill the session if running. The selector may be an ID prefix or a description substring (case-insensitive).`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		removeWorktree, _ := cmd.Flags().GetBool("worktree")
		forceWorktree, _ := cmd.Flags().GetBool("force-worktree")

		if forceWorktree && !removeWorktree {
			return exitcode.Errorf(exitcode.GeneralError, "--force-worktree requires --worktree")
		}

		selector := args[0]
		client := daemon.NewClient(getSocketPath())

		sessionID, sessionDesc, err := resolveSession(client, selector)
		if err != nil {
			return err
		}

		if err := client.Delete(sessionID, removeWorktree, forceWorktree); err != nil {
			if errors.Is(err, session.ErrWorktreeDirty) {
				return exitcode.Wrap(err, exitcode.WorktreeDirty,
					"worktree has uncommitted changes (use --force-worktree to override)")
			}
			return err
		}
		if jsonOutput {
			return renderActionResultJSON(os.Stdout, actionResult{Success: true, ID: sessionID, Description: sessionDesc})
		}
		fmt.Printf("Deleted session: %s\n", sessionDesc)
		return nil
	},
}

func renderActionResultJSON(w io.Writer, result actionResult) error {
	return writeJSON(w, result)
}

func init() {
	sessionCmd.AddCommand(killCmd)
	sessionCmd.AddCommand(deleteCmd)

	deleteCmd.Flags().Bool("worktree", false, "Also remove the git worktree associated with this session")
	deleteCmd.Flags().Bool("force-worktree", false, "Force worktree removal even with uncommitted changes (requires --worktree)")
}
