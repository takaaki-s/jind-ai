package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
)

var prHandoffCmd = &cobra.Command{
	Use:   "pr-handoff <selector> <plugin> [action]",
	Short: "Preflight or execute a structured pull-request handoff",
	Long: `Hand the current reviewed, committed workspace to a plugin action that
declares handoff: true. The dry run performs every local and capability check
without invoking the provider. Execution requires --confirm explicitly.

The request contains bounded commit/count evidence only: no patch, transcript,
prompt, environment, or credentials. A provider timeout or malformed response
is persisted as an unknown outcome. Repeating --confirm with the same
idempotency key safely asks the provider to reconcile or retry it.

Examples:
  jin session pr-handoff auth github-pr create --dry-run
  jin session pr-handoff auth github-pr create --confirm --idempotency-key pr-auth-v1`,
	Args: cobra.RangeArgs(2, 3),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			return completeSessionNames(cmd, args, toComplete)
		case 1:
			return completePluginNames(cmd, nil, toComplete)
		case 2:
			return completeHandoffActions(args[1], toComplete)
		default:
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	},
	RunE: runPRHandoff,
}

func runPRHandoff(cmd *cobra.Command, args []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	confirm, _ := cmd.Flags().GetBool("confirm")
	if dryRun == confirm {
		return fmt.Errorf("choose exactly one of --dry-run or --confirm")
	}
	actionID := ""
	if len(args) == 3 {
		actionID = args[2]
	}
	key, _ := cmd.Flags().GetString("idempotency-key")
	client := daemon.NewClient(getSocketPath())
	sess, err := resolveSelector(client, args[0])
	if err != nil {
		return err
	}
	result, err := client.PRHandoff(daemon.PRHandoffRequest{
		ID: sess.ID, Plugin: args[1], Action: actionID,
		IdempotencyKey: key, DryRun: dryRun, Confirm: confirm,
	})
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}
	target := result.Target
	if dryRun {
		fmt.Fprintf(cmd.OutOrStdout(), "PR handoff preflight passed: %s:%s\n", target.Plugin, target.Action)
		fmt.Fprintf(cmd.OutOrStdout(), "Idempotency key: %s\n", result.Payload.IdempotencyKey)
		fmt.Fprintf(cmd.OutOrStdout(), "Branch: %s (%s..%s, %d commits, %d files)\n",
			result.Payload.Review.Branch, shortSHA(result.Payload.Review.BaseCommit),
			shortSHA(result.Payload.Review.HeadCommit), result.Payload.Review.CommitCount,
			result.Payload.Review.ChangedFiles)
		fmt.Fprintln(cmd.OutOrStdout(), "No provider action was run. Repeat with --confirm and the idempotency key above.")
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "PR handoff %s: %s", result.Handoff.Status, result.Handoff.IdempotencyKey)
	if result.Handoff.Result.URL != "" {
		fmt.Fprintf(cmd.OutOrStdout(), " — %s", result.Handoff.Result.URL)
	} else if result.Handoff.Result.ID != "" {
		fmt.Fprintf(cmd.OutOrStdout(), " — %s", result.Handoff.Result.ID)
	}
	if result.Handoff.Error != "" {
		fmt.Fprintf(cmd.OutOrStdout(), " — %s", result.Handoff.Error)
	}
	fmt.Fprintln(cmd.OutOrStdout())
	return nil
}

func completeHandoffActions(pluginName, prefix string) ([]string, cobra.ShellCompDirective) {
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
			if action.Handoff && strings.HasPrefix(action.ID, prefix) {
				ids = append(ids, action.ID)
			}
		}
		return ids, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

func init() {
	sessionCmd.AddCommand(prHandoffCmd)
	prHandoffCmd.Flags().Bool("dry-run", false, "Run preflight only; do not invoke the provider")
	prHandoffCmd.Flags().Bool("confirm", false, "Explicitly invoke the handoff provider")
	prHandoffCmd.Flags().String("idempotency-key", "", "Stable provider idempotency key (generated from current evidence when omitted)")
}
