package cmd

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/claude-code-valet/internal/daemon"
	"github.com/takaaki-s/claude-code-valet/internal/session"
)

var infoCmd = &cobra.Command{
	Use:               "info <session-name>",
	Short:             "Show detailed information about a session",
	Long:              `Show detailed information about a Claude Code session. You can specify either session name or ID.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]
		client := daemon.NewClient(getSocketPath())

		sessionID, _, hostID, err := resolveSession(client, nameOrID)
		if err != nil {
			return err
		}

		info, err := client.Get(sessionID, hostID)
		if err != nil {
			return err
		}

		if jsonOutput {
			return renderSessionInfoJSON(os.Stdout, info)
		}

		renderSessionInfoText(os.Stdout, info)
		return nil
	},
}

func renderSessionInfoJSON(w io.Writer, info *session.Info) error {
	return writeJSON(w, info)
}

func renderSessionInfoText(w io.Writer, info *session.Info) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Name:\t%s\n", info.Name)
	fmt.Fprintf(tw, "ID:\t%s\n", info.ID)
	fmt.Fprintf(tw, "Status:\t%s\n", info.Status)
	fmt.Fprintf(tw, "WorkDir:\t%s\n", info.WorkDir)

	if info.CurrentWorkDir != "" {
		fmt.Fprintf(tw, "CurrentWorkDir:\t%s\n", info.CurrentWorkDir)
	}
	if info.CurrentBranch != "" {
		fmt.Fprintf(tw, "Branch:\t%s\n", info.CurrentBranch)
	}
	if info.HostID != "" {
		fmt.Fprintf(tw, "Host:\t%s\n", info.HostID)
	}

	fmt.Fprintf(tw, "Created:\t%s\n", info.CreatedAt.Format("2006-01-02 15:04:05"))
	if !info.LastActiveAt.IsZero() {
		fmt.Fprintf(tw, "LastActive:\t%s\n", info.LastActiveAt.Format("2006-01-02 15:04:05"))
	}

	if info.LastUserMessage != "" {
		fmt.Fprintf(tw, "LastUserMsg:\t%s\n", truncateStr(info.LastUserMessage, 80))
	}
	if info.LastAssistantMessage != "" {
		fmt.Fprintf(tw, "LastAssistantMsg:\t%s\n", truncateStr(info.LastAssistantMessage, 80))
	}
	tw.Flush()
}

func init() {
	sessionCmd.AddCommand(infoCmd)
}
