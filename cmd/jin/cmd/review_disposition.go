package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

var reviewDispositionCmd = &cobra.Command{
	Use:   "review-disposition <selector> <reviewed|changes-requested>",
	Short: "Record a human review decision for a session's current workspace",
	Long: `Record an explicit human review decision for the current non-empty local
workspace. The daemon refreshes the bounded review summary before accepting the
decision and binds it to that exact workspace fingerprint.

A later review refresh that observes different contents makes the decision
stale. This operation is independent of seen/unseen attention and does not
merge, delete, or clean up anything.

For retryable receipts, pass --idempotency-key, --actor and --fingerprint
(obtained with session review --json before inspecting the changes).
--note is optional single-line text. Actor is self-reported, not authenticated.

Examples:
  jin session review-disposition auth reviewed
  jin session review-disposition abcd1234 changes-requested --json`,
	Args: cobra.ExactArgs(2),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeSessionNames(cmd, args, toComplete)
		}
		if len(args) == 1 {
			return []string{"reviewed", "changes-requested"}, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		decision := session.ReviewDecision(strings.ToLower(args[1]))
		if decision != session.ReviewDecisionReviewed && decision != session.ReviewDecisionChangesRequested {
			return fmt.Errorf("invalid review decision %q (want reviewed or changes-requested)", args[1])
		}

		submission, recorded, err := reviewDispositionSubmission(cmd, decision)
		if err != nil {
			return err
		}
		client := daemon.NewClient(getSocketPath())
		sess, err := resolveSelector(client, args[0])
		if err != nil {
			return err
		}
		if recorded {
			result, err := client.RecordReviewDisposition(sess.ID, submission)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(os.Stdout, result)
			}
			fmt.Printf("Review receipt: %s (%s, reused=%t, stale=%t)\n", result.Receipt.IdempotencyKey, result.Receipt.Decision, result.Reused, result.Stale)
			return nil
		}
		updated, err := client.ReportReviewDisposition(sess.ID, decision)
		if err != nil {
			return err
		}

		if jsonOutput {
			return writeJSON(os.Stdout, updated)
		}
		fmt.Printf("Review decision recorded: %s — %s (%s)\n", decision, updated.Description, shortID(updated.ID))
		return nil
	},
}

func init() {
	reviewDispositionCmd.Flags().String("idempotency-key", "", "Stable key for this decision; reuse with identical content")
	reviewDispositionCmd.Flags().String("actor", "", "Self-reported reviewer (at most 128 bytes)")
	reviewDispositionCmd.Flags().String("fingerprint", "", "Workspace fingerprint inspected by the reviewer")
	reviewDispositionCmd.Flags().String("note", "", "Single-line note without secrets (at most 2048 bytes)")
	sessionCmd.AddCommand(reviewDispositionCmd)
}

func reviewDispositionSubmission(cmd *cobra.Command, decision session.ReviewDecision) (session.ReviewDispositionSubmission, bool, error) {
	var submission session.ReviewDispositionSubmission
	recorded := false
	for _, flag := range []string{"idempotency-key", "actor", "fingerprint", "note"} {
		recorded = recorded || cmd.Flags().Changed(flag)
	}
	if !recorded {
		return submission, false, nil
	}
	get := func(name string) string { value, _ := cmd.Flags().GetString(name); return value }
	submission.Decision = decision
	submission.IdempotencyKey = get("idempotency-key")
	submission.Actor = get("actor")
	submission.Note = get("note")
	submission.WorkspaceFingerprint = get("fingerprint")
	return submission, true, submission.Validate()
}
