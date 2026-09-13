package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/task"
)

var taskNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Create a task and asynchronously start its isolated execution",
	Long: `Reserve a durable task, an isolated git worktree, and an agent session,
then submit the prompt once the session is ready. The command returns as soon as
the stable task, execution, session, branch, and worktree identities exist.

Exactly one of --prompt and --prompt-file is required. Prompt bodies cross IPC
only for the live attempt; task state stores only a SHA-256 digest and byte
count. If an outcome is uncertain, repeat the command with the printed
--idempotency-key and the identical request.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		req, err := taskNewRequest(cmd)
		if err != nil {
			return err
		}
		result, err := daemon.NewClient(getSocketPath()).NewTask(req)
		if err != nil {
			return fmt.Errorf("task request %s: %w", req.IdempotencyKey, err)
		}
		if jsonOutput {
			return writeJSON(os.Stdout, result)
		}
		renderTaskNewText(os.Stdout, result)
		return nil
	},
}

func taskNewRequest(cmd *cobra.Command) (daemon.TaskNewRequest, error) {
	promptSet := cmd.Flags().Changed("prompt")
	promptFileSet := cmd.Flags().Changed("prompt-file")
	if promptSet == promptFileSet {
		return daemon.TaskNewRequest{}, fmt.Errorf("exactly one of --prompt and --prompt-file is required")
	}

	prompt, _ := cmd.Flags().GetString("prompt")
	if promptFileSet {
		path, _ := cmd.Flags().GetString("prompt-file")
		file, err := os.Open(path)
		if err != nil {
			return daemon.TaskNewRequest{}, fmt.Errorf("read prompt file: %w", err)
		}
		defer file.Close()
		body, err := io.ReadAll(io.LimitReader(file, task.MaxPromptBytes+1))
		if err != nil {
			return daemon.TaskNewRequest{}, fmt.Errorf("read prompt file: %w", err)
		}
		if len(body) > task.MaxPromptBytes {
			return daemon.TaskNewRequest{}, fmt.Errorf("prompt exceeds %d bytes", task.MaxPromptBytes)
		}
		prompt = string(body)
	}
	if prompt == "" {
		return daemon.TaskNewRequest{}, fmt.Errorf("prompt is required")
	}
	if len(prompt) > task.MaxPromptBytes {
		return daemon.TaskNewRequest{}, fmt.Errorf("prompt exceeds %d bytes", task.MaxPromptBytes)
	}

	idempotencyKey, _ := cmd.Flags().GetString("idempotency-key")
	if idempotencyKey == "" {
		idempotencyKey = daemon.NewTaskIdempotencyKey()
	}
	title, _ := cmd.Flags().GetString("title")
	repo, _ := cmd.Flags().GetString("repo")
	workdir, _ := cmd.Flags().GetString("workdir")
	base, _ := cmd.Flags().GetString("base")
	agentKind, _ := cmd.Flags().GetString("agent")
	model, _ := cmd.Flags().GetString("model")
	fleet, _ := cmd.Flags().GetString("fleet")
	noHook, _ := cmd.Flags().GetBool("no-hook")
	return daemon.TaskNewRequest{
		IdempotencyKey:  idempotencyKey,
		Title:           title,
		Prompt:          prompt,
		Repo:            repo,
		RelativeWorkDir: workdir,
		RequestedBase:   base,
		AgentKind:       agentKind,
		Model:           model,
		Fleet:           fleet,
		NoHook:          noHook,
	}, nil
}

func renderTaskNewText(w io.Writer, result *daemon.TaskNewResponse) {
	run := result.Execution.Run
	fmt.Fprintf(w, "Created task: %s (%s)\n", result.Task.Title, result.Task.ID)
	fmt.Fprintf(w, "Execution: %s", result.Execution.ID)
	if run != nil {
		fmt.Fprintf(w, " (%s)", run.Phase)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Session: %s (%s)\n", result.Session.ID, result.Session.Status)
	if run != nil {
		fmt.Fprintf(w, "Worktree: %s\nBranch: %s\nIdempotency key: %s\n",
			run.WorktreeName, run.WorktreeBranch, run.IdempotencyKey)
	}
	fmt.Fprintf(w, "\nFollow progress: jin task info %s\n", result.Task.ID)
}

func init() {
	taskCmd.AddCommand(taskNewCmd)
	addTaskNewFlags(taskNewCmd)
	_ = taskNewCmd.MarkFlagRequired("repo")
}

func addTaskNewFlags(cmd *cobra.Command) {
	cmd.Flags().String("prompt", "", "Prompt text (exclusive with --prompt-file)")
	cmd.Flags().String("prompt-file", "", "Read prompt text from a file (exclusive with --prompt)")
	cmd.Flags().String("repo", "", "Git repository root (required)")
	cmd.Flags().String("title", "", "Task and session title (default: <repository> task)")
	cmd.Flags().String("workdir", "", "Initial directory relative to the managed worktree")
	cmd.Flags().String("base", "", "Base branch name (default: repository default; do not prefix origin/)")
	cmd.Flags().String("agent", "", "Agent adapter kind (default: config default_agent)")
	cmd.Flags().String("model", "", "Model in the selected agent's CLI spelling")
	cmd.Flags().StringP("fleet", "f", "", "Fleet name (default: default)")
	cmd.Flags().Bool("no-hook", false, "Skip the worktree post-create hook")
	cmd.Flags().String("idempotency-key", "", "Reuse this key only to retry the identical request")
}
