package cmd

import (
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

var reviewCleanupCmd = &cobra.Command{
	Use:   "cleanup <selector>",
	Short: "Safely remove a verified merged session and its local assets",
	Long: `Preview or execute local cleanup after a verified merge handoff.

The dry-run names the exact session, managed worktree, repository, and local
branch and reports every blocker. Confirm requires the generated idempotency
key, repeats safety checks, and journals session stop, worktree removal, local
branch deletion, and session deletion independently. A partial failure can be
retried with the same full session ID and key after the session record is gone.

Remote branches and provider resources are never deleted.

Examples:
  jin session cleanup auth --dry-run
  jin session cleanup auth --confirm --idempotency-key cln_...`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE:              runReviewCleanup,
}

func runReviewCleanup(cmd *cobra.Command, args []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	confirm, _ := cmd.Flags().GetBool("confirm")
	if dryRun == confirm {
		return fmt.Errorf("choose exactly one of --dry-run or --confirm")
	}
	key, _ := cmd.Flags().GetString("idempotency-key")
	if confirm && key == "" {
		return fmt.Errorf("--idempotency-key is required with --confirm")
	}
	client := daemon.NewClient(getSocketPath())
	id := args[0]
	if _, err := uuid.Parse(id); err != nil {
		sess, err := resolveSelector(client, id)
		if err != nil {
			return err
		}
		id = sess.ID
	}
	result, err := client.ReviewCleanup(daemon.ReviewCleanupRequest{
		ID: id, IdempotencyKey: key, DryRun: dryRun, Confirm: confirm,
	})
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}
	if dryRun {
		printReviewCleanupPlan(cmd, result.Plan)
		if !result.Journal.IsZero() {
			printReviewCleanupJournal(cmd, result.Journal)
			fmt.Fprintln(cmd.OutOrStdout(), "No cleanup step was run by this dry-run; the persisted journal above is authoritative.")
			return nil
		}
		if result.Plan.Ready {
			fmt.Fprintln(cmd.OutOrStdout(), "No cleanup was run. Confirm with the idempotency key above.")
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "No cleanup was run. Resolve every blocker, then run dry-run again.")
		}
		return nil
	}
	printReviewCleanupJournal(cmd, result.Journal)
	if result.Journal.Status == session.ReviewCleanupFailed {
		return fmt.Errorf("cleanup incomplete; retry session %s with idempotency key %s", result.Plan.SessionID, result.Plan.IdempotencyKey)
	}
	return nil
}

func printReviewCleanupPlan(cmd *cobra.Command, plan session.ReviewCleanupPlan) {
	state := "ready"
	if !plan.Ready {
		state = "blocked"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Cleanup plan %s\n", state)
	fmt.Fprintf(cmd.OutOrStdout(), "Idempotency key: %s\n", plan.IdempotencyKey)
	fmt.Fprintf(cmd.OutOrStdout(), "Session: %s (%s)\n", plan.SessionID, firstNonEmpty(plan.SessionDescription, "-"))
	fmt.Fprintf(cmd.OutOrStdout(), "Worktree: %s\n", firstNonEmpty(plan.WorktreePath, "unresolved"))
	fmt.Fprintf(cmd.OutOrStdout(), "Repository: %s\n", firstNonEmpty(plan.RepositoryPath, "unresolved"))
	fmt.Fprintf(cmd.OutOrStdout(), "Local branch: %s @ %s\n", firstNonEmpty(plan.Branch, "unresolved"), shortSHA(plan.HeadCommit))
	fmt.Fprintf(cmd.OutOrStdout(), "Verified merge target: %s\n", shortSHA(plan.MergeTargetCommit))
	fmt.Fprintf(cmd.OutOrStdout(), "Commits after verified head: %d\n", plan.UnpushedCommits)
	fmt.Fprintln(cmd.OutOrStdout(), "Steps: session_stop -> worktree_remove -> local_branch_delete -> session_delete")
	for _, blocker := range plan.Blockers {
		fmt.Fprintf(cmd.OutOrStdout(), "Blocked: %s\n", blocker)
	}
}

func printReviewCleanupJournal(cmd *cobra.Command, journal session.ReviewCleanupJournal) {
	fmt.Fprintf(cmd.OutOrStdout(), "Cleanup %s: %s\n", journal.Status, journal.Plan.SessionID)
	for _, step := range journal.Steps {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s", step.Name, step.Status)
		if step.Error != "" {
			fmt.Fprintf(cmd.OutOrStdout(), " — %s", step.Error)
		}
		fmt.Fprintln(cmd.OutOrStdout())
	}
	if journal.Error != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Remaining cleanup starts at: %s\n", journal.Error)
	}
}

func init() {
	sessionCmd.AddCommand(reviewCleanupCmd)
	reviewCleanupCmd.Flags().Bool("dry-run", false, "Preview exact local cleanup targets and blockers")
	reviewCleanupCmd.Flags().Bool("confirm", false, "Explicitly execute the persisted local cleanup plan")
	reviewCleanupCmd.Flags().String("idempotency-key", "", "Stable cleanup idempotency key (required with --confirm)")
}
