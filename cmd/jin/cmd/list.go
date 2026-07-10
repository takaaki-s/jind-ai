package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all sessions",
	Long:    `List all agent sessions.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client := daemon.NewClient(getSocketPath())
		sessions, err := client.List()
		if err != nil {
			return err
		}

		if jsonOutput {
			return renderSessionListJSON(os.Stdout, sessions)
		}

		if len(sessions) == 0 {
			fmt.Println("No sessions found. Create one with: jin session new")
			return nil
		}

		return renderSessionTable(os.Stdout, sessions)
	},
}

// renderSessionTable writes the tabwriter-formatted session list to w. The
// caller is responsible for handling the "no sessions" case; this function
// always emits at least a header row.
func renderSessionTable(w io.Writer, sessions []session.Info) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DESCRIPTION\tSTATUS\tAGENT\tWORKDIR\tBRANCH\tLAST_ACTIVE")
	for _, s := range sessions {
		statusStr := string(s.Status)
		if s.Status == "error" && s.ErrorMessage != "" {
			statusStr = fmt.Sprintf("error: %s", truncateStr(s.ErrorMessage, 30))
		}

		// Use CurrentWorkDir if available, fall back to WorkDir
		displayDir := s.CurrentWorkDir
		if displayDir == "" {
			displayDir = s.WorkDir
		}
		// Shorten home directory
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(displayDir, home) {
			displayDir = "~" + displayDir[len(home):]
		}

		branch := s.CurrentBranch
		if branch == "" {
			branch = "-"
		}

		agentKind := s.AgentKind
		if agentKind == "" {
			agentKind = "-"
		}

		var lastActive string
		if !s.LastActiveAt.IsZero() {
			lastActive = s.LastActiveAt.Format("2006-01-02 15:04")
		} else {
			lastActive = s.CreatedAt.Format("2006-01-02 15:04")
		}

		desc := s.Description
		if s.DescriptionLocked {
			desc += "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			truncateStr(desc, 40),
			statusStr,
			agentKind,
			truncatePath(displayDir, 40),
			branch,
			lastActive,
		)
	}
	return tw.Flush()
}

func init() {
	sessionCmd.AddCommand(listCmd)
}

func renderSessionListJSON(w io.Writer, sessions []session.Info) error {
	if sessions == nil {
		sessions = []session.Info{}
	}
	return writeJSON(w, sessions)
}

func truncatePath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}
	return "..." + path[len(path)-maxLen+3:]
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}
