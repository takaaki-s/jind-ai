package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/honjin/internal/daemon"
	"github.com/takaaki-s/honjin/internal/exitcode"
	"github.com/takaaki-s/honjin/internal/session"
)

type actionResult struct {
	Success bool   `json:"success"`
	ID      string `json:"id"`
	Name    string `json:"name"`
}

var killCmd = &cobra.Command{
	Use:               "kill <session-name>",
	Short:             "Kill a running session",
	Long:              `Kill a running Claude Code session without deleting it. You can specify either session name or ID.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]
		client := daemon.NewClient(getSocketPath())

		sessionID, sessionName, hostID, err := resolveSession(client, nameOrID)
		if err != nil {
			return err
		}

		if err := client.Kill(sessionID, hostID); err != nil {
			return err
		}
		if jsonOutput {
			return renderActionResultJSON(os.Stdout, actionResult{Success: true, ID: sessionID, Name: sessionName})
		}
		fmt.Printf("Killed session: %s\n", sessionName)
		return nil
	},
}

var deleteCmd = &cobra.Command{
	Use:               "delete <session-name>",
	Aliases:           []string{"rm"},
	Short:             "Delete a session",
	Long:              `Delete a Claude Code session. This will kill the session if running. You can specify either session name or ID.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		removeWorktree, _ := cmd.Flags().GetBool("worktree")
		forceWorktree, _ := cmd.Flags().GetBool("force-worktree")

		if forceWorktree && !removeWorktree {
			return exitcode.Errorf(exitcode.GeneralError, "--force-worktree requires --worktree")
		}

		nameOrID := args[0]
		client := daemon.NewClient(getSocketPath())

		sessionID, sessionName, hostID, err := resolveSession(client, nameOrID)
		if err != nil {
			return err
		}

		if err := client.Delete(sessionID, hostID, removeWorktree, forceWorktree); err != nil {
			if errors.Is(err, session.ErrWorktreeDirty) {
				return exitcode.Wrap(err, exitcode.WorktreeDirty,
					"worktree has uncommitted changes (use --force-worktree to override)")
			}
			return err
		}
		if jsonOutput {
			return renderActionResultJSON(os.Stdout, actionResult{Success: true, ID: sessionID, Name: sessionName})
		}
		fmt.Printf("Deleted session: %s\n", sessionName)
		return nil
	},
}

// resolveSession resolves a session name or ID to the actual session ID, name, and host ID
func resolveSession(client *daemon.Client, nameOrID string) (id, name, hostID string, err error) {
	sessions, err := client.List()
	if err != nil {
		return "", "", "", err
	}

	return resolveSessionFromList(sessions, nameOrID)
}

// resolveSessionFromList resolves a session name or ID from a pre-fetched session list
func resolveSessionFromList(sessions []session.Info, nameOrID string) (id, name, hostID string, err error) {
	for _, s := range sessions {
		if s.Name == nameOrID || s.ID == nameOrID {
			return s.ID, s.Name, s.HostID, nil
		}
	}

	return "", "", "", exitcode.Errorf(exitcode.SessionNotFound, "session not found: %s", nameOrID)
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
