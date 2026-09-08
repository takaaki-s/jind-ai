package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

var checkReportCmd = &cobra.Command{
	Use:   "check-report <selector> <passed|failed>",
	Short: "Report aggregate checks for a session workspace",
	Long: `Record an aggregate result from checks run outside jind-ai. Before
accepting it, the daemon refreshes the bounded local review summary and binds
the report to that workspace fingerprint. jind-ai never discovers or runs
repository checks itself.

A failed report for the current fingerprint becomes checks-failed. Once newer
review evidence has a different fingerprint, the old report is shown as stale
and no longer blocks ready-for-review.

The selector may be an ID prefix or a description substring (case-insensitive).

Examples:
  jin session check-report abcd1234 passed
  jin session check-report auth failed --json`,
	Args: cobra.ExactArgs(2),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeSessionNames(cmd, args, toComplete)
		}
		if len(args) == 1 {
			return []string{"passed", "failed"}, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		status := session.CheckStatus(strings.ToLower(args[1]))
		if status != session.CheckStatusPassed && status != session.CheckStatusFailed {
			return fmt.Errorf("invalid check status %q (want passed or failed)", args[1])
		}

		client := daemon.NewClient(getSocketPath())
		sess, err := resolveSelector(client, args[0])
		if err != nil {
			return err
		}
		updated, err := client.ReportChecks(sess.ID, status)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(os.Stdout, updated)
		}
		fmt.Printf("Checks reported: %s — %s (%s)\n", status, updated.Description, shortID(updated.ID))
		return nil
	},
}

func init() {
	sessionCmd.AddCommand(checkReportCmd)
}
