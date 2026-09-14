package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/task"
)

var taskCommentCmd = &cobra.Command{
	Use:   "comment <task-selector>",
	Short: "Preview or post a comment to a Task's source GitHub Issue",
	Long: `Post one explicit comment to the GitHub Issue that sourced a Task.
Dry-run reads the target, actor, and reconciliation marker without changing
GitHub or Task state. Confirmation requires the exact idempotency key printed
by dry-run.

The comment body crosses IPC and provider stdin only for the live request. Task
state stores its SHA-256 digest and byte count, never the body or credentials.
If the provider response is lost, the outcome remains unknown and the same key
only reconciles the marker; jind-ai will not blindly submit a duplicate.`,
	Args: cobra.ExactArgs(1),
	RunE: runTaskComment,
}

func runTaskComment(cmd *cobra.Command, args []string) error {
	req, err := taskCommentRequest(cmd)
	if err != nil {
		return err
	}
	client := daemon.NewClient(getSocketPath())
	info, err := resolveTask(client, args[0])
	if err != nil {
		return err
	}
	req.TaskID = info.ID
	result, err := client.CommentOnTaskIssue(req)
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}
	if req.DryRun {
		fmt.Fprintf(cmd.OutOrStdout(), "Issue comment preflight passed: %s\n", result.Plan.Target.URL)
		fmt.Fprintf(cmd.OutOrStdout(), "Actor: %s\n", result.Plan.Actor)
		fmt.Fprintf(cmd.OutOrStdout(), "Body: %d bytes (sha256:%s)\n", result.Plan.Request.Bytes, result.Plan.Request.SHA256)
		fmt.Fprintf(cmd.OutOrStdout(), "Idempotency key: %s\n", result.Plan.IdempotencyKey)
		fmt.Fprintln(cmd.OutOrStdout(), "No comment was created. Repeat with --confirm and the idempotency key above.")
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Issue comment %s: %s", result.Mutation.Status, result.Mutation.IdempotencyKey)
	if result.Mutation.Result.URL != "" {
		fmt.Fprintf(cmd.OutOrStdout(), " — %s", result.Mutation.Result.URL)
	}
	if result.Mutation.Error != "" {
		fmt.Fprintf(cmd.OutOrStdout(), " — %s", result.Mutation.Error)
	}
	fmt.Fprintln(cmd.OutOrStdout())
	return nil
}

func taskCommentRequest(cmd *cobra.Command) (daemon.TaskCommentRequest, error) {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	confirm, _ := cmd.Flags().GetBool("confirm")
	if dryRun == confirm {
		return daemon.TaskCommentRequest{}, fmt.Errorf("choose exactly one of --dry-run or --confirm")
	}
	bodySet := cmd.Flags().Changed("body")
	bodyFileSet := cmd.Flags().Changed("body-file")
	if bodySet == bodyFileSet {
		return daemon.TaskCommentRequest{}, fmt.Errorf("choose exactly one of --body or --body-file")
	}
	body, _ := cmd.Flags().GetString("body")
	if bodyFileSet {
		path, _ := cmd.Flags().GetString("body-file")
		file, err := os.Open(path)
		if err != nil {
			return daemon.TaskCommentRequest{}, fmt.Errorf("read comment file: %w", err)
		}
		defer file.Close()
		content, err := io.ReadAll(io.LimitReader(file, task.MaxMutationBodyBytes+1))
		if err != nil {
			return daemon.TaskCommentRequest{}, fmt.Errorf("read comment file: %w", err)
		}
		body = string(content)
	}
	if len(body) == 0 || len(body) > task.MaxMutationBodyBytes {
		return daemon.TaskCommentRequest{}, fmt.Errorf("comment body must be 1-%d bytes", task.MaxMutationBodyBytes)
	}
	key, _ := cmd.Flags().GetString("idempotency-key")
	if confirm && key == "" {
		return daemon.TaskCommentRequest{}, fmt.Errorf("--confirm requires --idempotency-key from dry-run")
	}
	return daemon.TaskCommentRequest{Body: body, IdempotencyKey: key, DryRun: dryRun, Confirm: confirm}, nil
}

func init() {
	taskCmd.AddCommand(taskCommentCmd)
	addTaskCommentFlags(taskCommentCmd)
}

func addTaskCommentFlags(cmd *cobra.Command) {
	cmd.Flags().String("body", "", "Comment body (exclusive with --body-file)")
	cmd.Flags().String("body-file", "", "Read comment body from a file (exclusive with --body)")
	cmd.Flags().Bool("dry-run", false, "Inspect target and print the confirmation key without writing")
	cmd.Flags().Bool("confirm", false, "Explicitly create or reconcile the Issue comment")
	cmd.Flags().String("idempotency-key", "", "Exact key printed by dry-run (required with --confirm)")
}
