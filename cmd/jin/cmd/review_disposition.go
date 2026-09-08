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

		client := daemon.NewClient(getSocketPath())
		sess, err := resolveSelector(client, args[0])
		if err != nil {
			return err
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
	sessionCmd.AddCommand(reviewDispositionCmd)
}
