package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/claude-code-valet/internal/daemon"
	"github.com/takaaki-s/claude-code-valet/internal/exitcode"
	"github.com/takaaki-s/claude-code-valet/internal/session"
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
		nameOrID := args[0]
		client := daemon.NewClient(getSocketPath())

		sessionID, sessionName, hostID, err := resolveSession(client, nameOrID)
		if err != nil {
			return err
		}

		if err := client.Delete(sessionID, hostID); err != nil {
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
}
