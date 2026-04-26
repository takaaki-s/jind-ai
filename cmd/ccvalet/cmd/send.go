package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/claude-code-valet/internal/daemon"
)

type sendResult struct {
	Success bool   `json:"success"`
	Session string `json:"session"`
}

var sendCmd = &cobra.Command{
	Use:     "send <session-name> <prompt>",
	Aliases: []string{"prompt"},
	Short:   "Send a prompt to a session",
	Long: `Send a prompt to a Claude Code session. The session must be in idle status.

Multiple arguments after the session name are joined with spaces:
  ccvalet session send my-session Fix the bug
  ccvalet session send my-session "Fix the bug"   # equivalent

Use "-" as the prompt to read from stdin:
  echo "Fix the bug" | ccvalet session send my-session -`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]

		var prompt string
		if len(args) >= 2 {
			if args[1] == "-" {
				// Read from stdin
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("failed to read from stdin: %w", err)
				}
				prompt = strings.TrimRight(string(data), "\n")
			} else {
				prompt = strings.Join(args[1:], " ")
			}
		} else {
			return fmt.Errorf("prompt is required")
		}

		if prompt == "" {
			return fmt.Errorf("prompt cannot be empty")
		}

		client := daemon.NewClient(getSocketPath())

		sessionID, sessionName, hostID, err := resolveSession(client, nameOrID)
		if err != nil {
			return err
		}

		if err := client.Send(sessionID, prompt, hostID); err != nil {
			return err
		}

		if jsonOutput {
			return renderSendResultJSON(os.Stdout, sendResult{Success: true, Session: sessionName})
		}
		fmt.Printf("Sent prompt to session: %s\n", sessionName)
		return nil
	},
}

func renderSendResultJSON(w io.Writer, result sendResult) error {
	return writeJSON(w, result)
}

func init() {
	sessionCmd.AddCommand(sendCmd)
}
