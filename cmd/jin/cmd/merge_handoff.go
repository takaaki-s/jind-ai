package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
)

var mergeHandoffCmd = &cobra.Command{
	Use:   "merge-handoff <selector> <plugin> [action]",
	Short: "Preflight or explicitly execute a provider merge",
	Long: `Ask a merge_handoff provider to inspect the current pull request, then
optionally merge it. Core first requires current reviewed evidence and a
successful PR handoff for the same workspace. Provider preflight must echo the
reviewed head and report mergeable=true with required_checks=passed.

Dry-run invokes only the provider's read-only preflight operation. Confirmed
execution requires the exact idempotency key explicitly; timeout or ambiguous
provider output is persisted as unknown and must be reconciled with that same
key. Merge success records the provider target commit but never cleans up the
branch, worktree, or session.

Examples:
  jin session merge-handoff auth github-merge merge --dry-run
  jin session merge-handoff auth github-merge merge --confirm --idempotency-key mrg_...`,
	Args: cobra.RangeArgs(2, 3),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			return completeSessionNames(cmd, args, toComplete)
		case 1:
			return completePluginNames(cmd, nil, toComplete)
		case 2:
			return completeMergeHandoffActions(args[1], toComplete)
		default:
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	},
	RunE: runMergeHandoff,
}

func runMergeHandoff(cmd *cobra.Command, args []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	confirm, _ := cmd.Flags().GetBool("confirm")
	if dryRun == confirm {
		return fmt.Errorf("choose exactly one of --dry-run or --confirm")
	}
	key, _ := cmd.Flags().GetString("idempotency-key")
	if confirm && key == "" {
		return fmt.Errorf("--idempotency-key is required with --confirm")
	}
	actionID := ""
	if len(args) == 3 {
		actionID = args[2]
	}
	client := daemon.NewClient(getSocketPath())
	sess, err := resolveSelector(client, args[0])
	if err != nil {
		return err
	}
	result, err := client.MergeHandoff(daemon.MergeHandoffRequest{
		ID: sess.ID, Plugin: args[1], Action: actionID,
		IdempotencyKey: key, DryRun: dryRun, Confirm: confirm,
	})
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}
	if dryRun {
		status := "blocked"
		if result.Preflight.Status == "ready" {
			status = "passed"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Merge preflight %s: %s:%s\n", status, result.Target.Plugin, result.Target.Action)
		fmt.Fprintf(cmd.OutOrStdout(), "Idempotency key: %s\n", result.Payload.IdempotencyKey)
		fmt.Fprintf(cmd.OutOrStdout(), "Target: %s %s (%s)\n", result.Preflight.Target.Provider,
			firstNonEmpty(result.Preflight.Target.ID, result.Preflight.Target.URL), result.Preflight.Target.BaseRef)
		fmt.Fprintf(cmd.OutOrStdout(), "Base/head: %s..%s\n", shortSHA(result.Preflight.Target.BaseCommit), shortSHA(result.Preflight.Target.HeadCommit))
		fmt.Fprintf(cmd.OutOrStdout(), "Mergeable: %t; required checks: %s\n", result.Preflight.Mergeable, result.Preflight.RequiredChecks)
		if result.Preflight.Message != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "Provider: %s\n", result.Preflight.Message)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "No merge was run. A confirmed call must pass the idempotency key above explicitly.")
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Merge handoff %s: %s", result.Handoff.Status, result.Handoff.IdempotencyKey)
	if result.Handoff.Result.TargetCommit != "" {
		fmt.Fprintf(cmd.OutOrStdout(), " — %s", result.Handoff.Result.TargetCommit)
	}
	if result.Handoff.Error != "" {
		fmt.Fprintf(cmd.OutOrStdout(), " — %s", result.Handoff.Error)
	}
	fmt.Fprintln(cmd.OutOrStdout())
	return nil
}

func completeMergeHandoffActions(pluginName, prefix string) ([]string, cobra.ShellCompDirective) {
	entries, err := loadPluginEntries()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	for _, entry := range entries {
		if entry.Name != pluginName || entry.Manifest == nil {
			continue
		}
		var ids []string
		for _, action := range entry.Manifest.Actions {
			if action.MergeHandoff && strings.HasPrefix(action.ID, prefix) {
				ids = append(ids, action.ID)
			}
		}
		return ids, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "-"
}

func init() {
	sessionCmd.AddCommand(mergeHandoffCmd)
	mergeHandoffCmd.Flags().Bool("dry-run", false, "Run provider preflight only; do not merge")
	mergeHandoffCmd.Flags().Bool("confirm", false, "Explicitly invoke the provider merge operation")
	mergeHandoffCmd.Flags().String("idempotency-key", "", "Stable provider idempotency key (required with --confirm)")
}
