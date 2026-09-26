package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/task"
)

var taskNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Create a task and start an isolated local or remote execution",
	Long: `Reserve a durable task and start an isolated execution. Local execution
creates a git worktree and agent session asynchronously. Remote execution first
persists the controller identity, then asks an explicitly configured target to
own its worktree and session.

Exactly one of --prompt, --prompt-file, and --issue is required. --issue reads
GitHub through the authenticated gh CLI without mutating it, and frames its
bounded content as untrusted prompt context. Prompt bodies cross IPC only for
the live attempt; task state stores only a SHA-256 digest and byte count. If an
outcome is uncertain, repeat the command with the printed --idempotency-key and
the identical request. Use either --repo for local execution, or --target with
--repository for remote execution.`,
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
	return taskNewRequestWithDefaultRepo(cmd, "")
}

func taskNewRequestWithDefaultRepo(cmd *cobra.Command, defaultRepo string) (daemon.TaskNewRequest, error) {
	promptSet := cmd.Flags().Changed("prompt")
	promptFileSet := cmd.Flags().Changed("prompt-file")
	issueSet := cmd.Flags().Changed("issue")
	selected := 0
	for _, set := range []bool{promptSet, promptFileSet, issueSet} {
		if set {
			selected++
		}
	}
	if selected != 1 {
		return daemon.TaskNewRequest{}, fmt.Errorf("exactly one of --prompt, --prompt-file, and --issue is required")
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
	if !issueSet && prompt == "" {
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
	issue, _ := cmd.Flags().GetString("issue")
	repo, _ := cmd.Flags().GetString("repo")
	target, _ := cmd.Flags().GetString("target")
	repository, _ := cmd.Flags().GetString("repository")
	if repo == "" && target == "" && repository == "" {
		repo = defaultRepo
	}
	localSelected := repo != ""
	remoteSelected := target != "" || repository != ""
	if localSelected == remoteSelected {
		return daemon.TaskNewRequest{}, fmt.Errorf("use either --repo, or both --target and --repository")
	}
	if remoteSelected && (target == "" || repository == "") {
		return daemon.TaskNewRequest{}, fmt.Errorf("--target and --repository must be used together")
	}
	if remoteSelected && issueSet {
		return daemon.TaskNewRequest{}, fmt.Errorf("remote task execution currently supports --prompt and --prompt-file only")
	}
	if localSelected {
		resolvedRepo, err := filepath.Abs(repo)
		if err != nil {
			return daemon.TaskNewRequest{}, fmt.Errorf("failed to resolve repository %q: %w", repo, err)
		}
		repo = resolvedRepo
	}
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
		Issue:           issue,
		Repo:            repo,
		Target:          target,
		Repository:      repository,
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
	if result.Execution.Backend == task.ExecutionBackendRemote && result.Execution.Remote != nil {
		link := result.Execution.Remote
		fmt.Fprintf(w, "Remote: %s/%s (%s)\n", link.TargetID, link.RepositoryLabel, link.SyncState)
		if link.RemoteExecutionID != "" {
			fmt.Fprintf(w, "Remote execution: %s\n", link.RemoteExecutionID)
		}
		if link.Error != "" {
			fmt.Fprintf(w, "Remote error: %s\n", link.Error)
		}
	} else {
		fmt.Fprintf(w, "Session: %s (%s)\n", result.Session.ID, result.Session.Status)
	}
	if run != nil {
		if result.Execution.Backend != task.ExecutionBackendRemote {
			fmt.Fprintf(w, "Worktree: %s\nBranch: %s\n", run.WorktreeName, run.WorktreeBranch)
		}
		fmt.Fprintf(w, "Idempotency key: %s\n", run.IdempotencyKey)
	}
	fmt.Fprintf(w, "\nFollow progress: jin task info %s\n", result.Task.ID)
}

func init() {
	taskCmd.AddCommand(taskNewCmd)
	addTaskNewFlags(taskNewCmd)
}

func addTaskNewFlags(cmd *cobra.Command) {
	cmd.Flags().String("prompt", "", "Prompt text (exclusive with --prompt-file and --issue)")
	cmd.Flags().String("prompt-file", "", "Read prompt text from a file (exclusive with --prompt and --issue)")
	cmd.Flags().String("issue", "", "Read a GitHub Issue URL, owner/repo#number, or number via gh")
	cmd.Flags().String("repo", "", "Local git repository root; relative paths use the caller's current directory (exclusive with --target)")
	cmd.Flags().String("target", "", "Configured remote target (requires --repository)")
	cmd.Flags().String("repository", "", "Repository mapping on the remote target")
	cmd.Flags().String("title", "", "Task and session title (default: Issue title or <repository> task)")
	cmd.Flags().String("workdir", "", "Initial directory relative to the managed worktree")
	cmd.Flags().String("base", "", "Base branch name (default: repository default; do not prefix origin/)")
	cmd.Flags().String("agent", "", "Agent adapter kind (default: config default_agent)")
	cmd.Flags().String("model", "", "Model in the selected agent's CLI spelling")
	cmd.Flags().StringP("fleet", "f", "", "Fleet name (default: default)")
	cmd.Flags().Bool("no-hook", false, "Skip the worktree post-create hook")
	cmd.Flags().String("idempotency-key", "", "Reuse this key only to retry the identical request")
}
