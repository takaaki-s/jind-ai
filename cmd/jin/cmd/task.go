package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/task"
)

var taskCmd = &cobra.Command{
	Use:   "task",
	Short: "Manage durable tasks and their session executions",
}

var taskCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create task metadata without starting an agent",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		title, _ := cmd.Flags().GetString("title")
		sourceKind, _ := cmd.Flags().GetString("source-kind")
		sourceRef, _ := cmd.Flags().GetString("source-ref")
		requestedBase, _ := cmd.Flags().GetString("requested-base")
		promptSummary, _ := cmd.Flags().GetString("prompt-summary")

		client := daemon.NewClient(getSocketPath())
		info, err := client.CreateTask(daemon.TaskCreateRequest{
			Title: title, Source: task.Source{Kind: sourceKind, Ref: sourceRef},
			RequestedBase: requestedBase, PromptSummary: promptSummary,
		})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(os.Stdout, info)
		}
		fmt.Printf("Created task: %s (%s)\n", info.Title, info.ID)
		return nil
	},
}

var taskListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List tasks",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		infos, err := daemon.NewClient(getSocketPath()).ListTasks()
		if err != nil {
			return err
		}
		if jsonOutput {
			return renderTaskListJSON(os.Stdout, infos)
		}
		if len(infos) == 0 {
			fmt.Fprintln(os.Stdout, "No tasks found. Create one with: jin task create --title <title>")
			return nil
		}
		return renderTaskTable(os.Stdout, infos)
	},
}

var taskInfoCmd = &cobra.Command{
	Use:   "info <selector>",
	Short: "Show a task and its execution history",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := daemon.NewClient(getSocketPath())
		info, err := resolveTask(client, args[0])
		if err != nil {
			return err
		}
		info, err = client.GetTask(info.ID)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(os.Stdout, info)
		}
		renderTaskInfoText(os.Stdout, info)
		return nil
	},
}

var taskExecutionCmd = &cobra.Command{
	Use:   "execution",
	Short: "Manage a task's execution attempts",
}

var taskExecutionAddCmd = &cobra.Command{
	Use:   "add <task-selector>",
	Short: "Append an existing session as a new execution",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sessionSelector, _ := cmd.Flags().GetString("session")
		client := daemon.NewClient(getSocketPath())
		taskInfo, err := resolveTask(client, args[0])
		if err != nil {
			return err
		}
		sess, err := resolveSelector(client, sessionSelector)
		if err != nil {
			return err
		}
		updated, err := client.AddTaskExecution(taskInfo.ID, sess.ID)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(os.Stdout, updated)
		}
		latest := updated.Executions[len(updated.Executions)-1]
		fmt.Printf("Added execution %d (%s) to task %s\n", latest.Sequence, latest.ID, updated.Title)
		return nil
	},
}

func resolveTask(client *daemon.Client, selector string) (*task.Info, error) {
	if selector == "" {
		return nil, fmt.Errorf("task selector is required")
	}
	infos, err := client.ListTasks()
	if err != nil {
		return nil, err
	}
	return resolveTaskFromList(infos, selector)
}

func resolveTaskFromList(infos []task.Info, selector string) (*task.Info, error) {
	for i := range infos {
		if infos[i].ID == selector {
			return &infos[i], nil
		}
	}
	if len(selector) >= idPrefixMinLen {
		var matches []int
		for i := range infos {
			if strings.HasPrefix(infos[i].ID, selector) {
				matches = append(matches, i)
			}
		}
		if len(matches) == 1 {
			return &infos[matches[0]], nil
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("ambiguous task selector %q", selector)
		}
	}
	var exact []int
	for i := range infos {
		if infos[i].Title == selector {
			exact = append(exact, i)
		}
	}
	if len(exact) == 1 {
		return &infos[exact[0]], nil
	}
	if len(exact) > 1 {
		return nil, fmt.Errorf("ambiguous task selector %q", selector)
	}
	needle := strings.ToLower(selector)
	var matches []int
	for i := range infos {
		if strings.Contains(strings.ToLower(infos[i].Title), needle) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 1 {
		return &infos[matches[0]], nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("ambiguous task selector %q", selector)
	}
	return nil, fmt.Errorf("no task matches selector: %s", selector)
}

func renderTaskListJSON(w io.Writer, infos []task.Info) error {
	if infos == nil {
		infos = []task.Info{}
	}
	return writeJSON(w, infos)
}

func renderTaskTable(w io.Writer, infos []task.Info) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TITLE\tEXECUTIONS\tLATEST\tSOURCE\tUPDATED")
	for _, info := range infos {
		latest := "-"
		if info.LatestAttention != nil {
			latest = string(info.LatestAttention.ReferenceState)
			if info.LatestAttention.ReferenceState == task.ReferencePresent {
				latest = string(info.LatestAttention.SessionStatus)
			}
			if info.LatestAttention.Attention.Unseen {
				latest = string(info.LatestAttention.Attention.State)
			}
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", truncateStr(info.Title, 48), len(info.Executions), latest,
			info.Source.Kind, info.UpdatedAt.Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

func renderTaskInfoText(w io.Writer, info *task.Info) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Title:\t%s\nID:\t%s\nSource:\t%s", info.Title, info.ID, info.Source.Kind)
	if info.Source.Ref != "" {
		fmt.Fprintf(tw, " (%s)", info.Source.Ref)
	}
	fmt.Fprintln(tw)
	if info.RequestedBase != "" {
		fmt.Fprintf(tw, "RequestedBase:\t%s\n", info.RequestedBase)
	}
	if info.PromptSummary != "" {
		fmt.Fprintf(tw, "PromptSummary:\t%s\n", info.PromptSummary)
	}
	fmt.Fprintf(tw, "Created:\t%s\nUpdated:\t%s\nExecutions:\t%d\n", info.CreatedAt.Format("2006-01-02 15:04:05"), info.UpdatedAt.Format("2006-01-02 15:04:05"), len(info.Executions))
	for _, execution := range info.Executions {
		state := string(execution.ReferenceState)
		if execution.ReferenceState == task.ReferencePresent {
			state = string(execution.SessionStatus)
		}
		if execution.Attention.Unseen {
			state += "/" + string(execution.Attention.State)
		}
		fmt.Fprintf(tw, "  #%d:\t%s  session=%s  %s\n", execution.Sequence, execution.ID, execution.SessionID, state)
	}
	_ = tw.Flush()
}

func init() {
	rootCmd.AddCommand(taskCmd)
	taskCmd.AddCommand(taskCreateCmd, taskListCmd, taskInfoCmd, taskExecutionCmd)
	taskExecutionCmd.AddCommand(taskExecutionAddCmd)
	taskCreateCmd.Flags().String("title", "", "Task title (required)")
	taskCreateCmd.Flags().String("source-kind", "manual", "Origin kind, such as manual or issue")
	taskCreateCmd.Flags().String("source-ref", "", "Bounded origin reference; provider content is not copied")
	taskCreateCmd.Flags().String("requested-base", "", "Requested base ref for future executions")
	taskCreateCmd.Flags().String("prompt-summary", "", "Bounded summary metadata; never a full prompt or transcript")
	_ = taskCreateCmd.MarkFlagRequired("title")
	taskExecutionAddCmd.Flags().String("session", "", "Existing session selector (required)")
	_ = taskExecutionAddCmd.MarkFlagRequired("session")
}
