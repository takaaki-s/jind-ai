package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

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

For an idempotent audit receipt, pass --idempotency-key, --fingerprint, --name,
--reporter, --started-at and --finished-at (RFC3339). Obtain the fingerprint via
session review --json before running checks. New receipts reject changed
workspaces; retry the same key and content after an uncertain response.
--summary is optional single-line text. Reporter is a self-reported identity.

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
		submission, recorded, err := checkReportSubmission(cmd, status)
		if err != nil {
			return err
		}

		client := daemon.NewClient(getSocketPath())
		sess, err := resolveSelector(client, args[0])
		if err != nil {
			return err
		}
		if recorded {
			result, err := client.RecordChecks(sess.ID, submission)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(os.Stdout, result)
			}
			fmt.Printf("Check receipt: %s (%s, reused=%t, stale=%t)\n", result.Receipt.IdempotencyKey, result.Receipt.Status, result.Reused, result.Stale)
			return nil
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
	for _, flag := range []struct{ name, help string }{
		{"idempotency-key", "Stable key for this check run; reuse only with identical content"},
		{"fingerprint", "Workspace fingerprint observed before running checks"},
		{"name", "Check suite name (at most 128 bytes)"},
		{"reporter", "Self-reported check runner (at most 128 bytes)"},
		{"summary", "Single-line summary without secrets (at most 2048 bytes)"},
		{"started-at", "Check start time in RFC3339 format"},
		{"finished-at", "Check finish time in RFC3339 format"},
	} {
		checkReportCmd.Flags().String(flag.name, "", flag.help)
	}
	sessionCmd.AddCommand(checkReportCmd)
}

func checkReportSubmission(cmd *cobra.Command, status session.CheckStatus) (session.CheckReportSubmission, bool, error) {
	var submission session.CheckReportSubmission
	recorded := false
	for _, flag := range []string{"idempotency-key", "fingerprint", "name", "reporter", "summary", "started-at", "finished-at"} {
		recorded = recorded || cmd.Flags().Changed(flag)
	}
	if !recorded {
		return submission, false, nil
	}
	get := func(name string) string { value, _ := cmd.Flags().GetString(name); return value }
	submission.Status = status
	submission.IdempotencyKey = get("idempotency-key")
	submission.WorkspaceFingerprint = get("fingerprint")
	submission.Name = get("name")
	submission.Reporter = get("reporter")
	submission.Summary = get("summary")
	var err error
	submission.StartedAt, err = time.Parse(time.RFC3339Nano, get("started-at"))
	if err != nil {
		return submission, true, fmt.Errorf("started-at must be RFC3339")
	}
	submission.FinishedAt, err = time.Parse(time.RFC3339Nano, get("finished-at"))
	if err != nil {
		return submission, true, fmt.Errorf("finished-at must be RFC3339")
	}
	return submission, true, submission.Validate()
}
