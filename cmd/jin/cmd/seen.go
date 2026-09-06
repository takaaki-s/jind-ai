package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
)

var seenCmd = &cobra.Command{
	Use:   "seen <selector>",
	Short: "Acknowledge a session's completed turn",
	Long: `Acknowledge the completion a session is holding, so it stops standing out
in the TUI. The selector may be an ID prefix or a description substring
(case-insensitive).

Acknowledging says nothing about the session's process status, and nothing
about whether its work is any good — it only clears the receipt left by the
last completed turn. A later completion raises a new one.

Calling it on a session with nothing to acknowledge succeeds and changes
nothing, so it is safe to retry.

Examples:
  jin session seen abcd1234
  jin session seen auth --json`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		client := daemon.NewClient(getSocketPath())

		sess, err := resolveSelector(client, args[0])
		if err != nil {
			return err
		}

		updated, err := client.MarkSeen(sess.ID)
		if err != nil {
			return err
		}

		if jsonOutput {
			return writeJSON(os.Stdout, updated)
		}
		fmt.Printf("Marked completion seen: %s (%s)\n", updated.Description, shortID(updated.ID))
		return nil
	},
}

func init() {
	sessionCmd.AddCommand(seenCmd)
}
