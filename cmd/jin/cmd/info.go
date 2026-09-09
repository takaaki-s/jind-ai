package cmd

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

var infoCmd = &cobra.Command{
	Use:               "info <selector>",
	Short:             "Show detailed information about a session",
	Long:              `Show detailed information about a Claude Code session. The selector may be an ID prefix or a description substring (case-insensitive).`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]
		client := daemon.NewClient(getSocketPath())

		sessionID, _, err := resolveSession(client, nameOrID)
		if err != nil {
			return err
		}

		info, err := client.Get(sessionID)
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
	fmt.Fprintf(tw, "Description:\t%s\n", info.Description)
	fmt.Fprintf(tw, "ID:\t%s\n", info.ID)
	fmt.Fprintf(tw, "Status:\t%s\n", info.Status)
	fmt.Fprintf(tw, "WorkDir:\t%s\n", info.WorkDir)

	if info.CurrentWorkDir != "" {
		fmt.Fprintf(tw, "CurrentWorkDir:\t%s\n", info.CurrentWorkDir)
	}
	if info.RepoName != "" {
		fmt.Fprintf(tw, "Repo:\t%s\n", info.RepoName)
	}
	if info.CurrentBranch != "" {
		fmt.Fprintf(tw, "Branch:\t%s\n", info.CurrentBranch)
	}
	if info.Attention.State != session.AttentionNone {
		fmt.Fprintf(tw, "Attention:\t%s", info.Attention.State)
		if info.Attention.Unseen {
			fmt.Fprint(tw, " (unseen)")
		}
		fmt.Fprintln(tw)
	}
	if info.ReviewFacts.Status != "" {
		fmt.Fprintf(tw, "Review:\t%s", info.ReviewFacts.Status)
		if info.ReviewFacts.Status == session.ReviewFactsAvailable {
			fmt.Fprintf(tw, " — %d files, +%d -%d, %d commits",
				info.ReviewFacts.ChangedFiles, info.ReviewFacts.Additions,
				info.ReviewFacts.Deletions, info.ReviewFacts.CommitCount)
		} else if info.ReviewFacts.UnavailableReason != "" {
			fmt.Fprintf(tw, " — %s", info.ReviewFacts.UnavailableReason)
		}
		fmt.Fprintln(tw)
	}
	if info.CheckReport.Status != "" {
		fmt.Fprintf(tw, "Checks:\t%s (%s)", info.CheckReport.Status, info.CheckReport.Source)
		if info.CheckReport.Stale {
			fmt.Fprint(tw, " — stale")
		}
		fmt.Fprintln(tw)
	}
	if info.ReviewDisposition.Decision != "" {
		fmt.Fprintf(tw, "Disposition:\t%s", info.ReviewDisposition.Decision)
		if info.ReviewDisposition.Stale {
			fmt.Fprint(tw, " — stale")
		}
		fmt.Fprintln(tw)
	}
	if info.PRHandoff.Status != "" {
		fmt.Fprintf(tw, "PRHandoff:\t%s (%s:%s)", info.PRHandoff.Status,
			info.PRHandoff.Target.Plugin, info.PRHandoff.Target.Action)
		if info.PRHandoff.Stale {
			fmt.Fprint(tw, " — stale")
		}
		if info.PRHandoff.Result.URL != "" {
			fmt.Fprintf(tw, " — %s", info.PRHandoff.Result.URL)
		}
		fmt.Fprintln(tw)
	}
	if info.MergeHandoff.Status != "" {
		fmt.Fprintf(tw, "MergeHandoff:\t%s (%s:%s)", info.MergeHandoff.Status,
			info.MergeHandoff.Target.Plugin, info.MergeHandoff.Target.Action)
		if info.MergeHandoff.Stale {
			fmt.Fprint(tw, " — stale")
		}
		if info.MergeHandoff.Result.TargetCommit != "" {
			fmt.Fprintf(tw, " — %s", info.MergeHandoff.Result.TargetCommit)
		}
		fmt.Fprintln(tw)
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
