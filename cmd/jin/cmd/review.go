package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

var reviewCmd = &cobra.Command{
	Use:   "review <selector>",
	Short: "Refresh a session's local review summary",
	Long: `Compare a managed worktree with the exact base commit recorded when it
was created. The operation is local and read-only: it does not fetch, run tests,
or render/store patch contents.

The selector may be an ID prefix or a description substring (case-insensitive).`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		client := daemon.NewClient(getSocketPath())
		sess, err := resolveSelector(client, args[0])
		if err != nil {
			return err
		}
		updated, err := client.RefreshReview(sess.ID)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(os.Stdout, updated)
		}
		renderReviewSummary(os.Stdout, updated.ReviewFacts)
		return nil
	},
}

func renderReviewSummary(w io.Writer, facts session.ReviewFacts) {
	switch facts.Status {
	case session.ReviewFactsAvailable:
		fmt.Fprintf(w, "Review: %d files, +%d -%d, %d commits",
			facts.ChangedFiles, facts.Additions, facts.Deletions, facts.CommitCount)
		if facts.BinaryFiles > 0 {
			fmt.Fprintf(w, ", %d binary", facts.BinaryFiles)
		}
		if facts.UntrackedFiles > 0 {
			fmt.Fprintf(w, ", %d untracked", facts.UntrackedFiles)
		}
		fmt.Fprintln(w)
	case session.ReviewFactsUnavailable:
		fmt.Fprintf(w, "Review unavailable: %s\n", facts.UnavailableReason)
	default:
		fmt.Fprintln(w, "Review pending")
	}
}

func init() {
	sessionCmd.AddCommand(reviewCmd)
}
