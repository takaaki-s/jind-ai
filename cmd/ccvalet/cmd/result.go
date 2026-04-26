package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/claude-code-valet/internal/daemon"
	"github.com/takaaki-s/claude-code-valet/internal/transcript"
)

var resultCmd = &cobra.Command{
	Use:   "result <session-name>",
	Short: "Fetch structured transcript entries for a session (orchestration)",
	Long: `Fetch structured transcript entries (text/thinking/tool_use/tool_result)
for a Claude Code session. Designed for orchestration scripts that need to
inspect what a child session actually did, not just the final assistant text.

Examples:
  # Show a summary of recent activity
  ccvalet session result my-session

  # Get only Bash tool calls and their results, as JSON
  ccvalet session result my-session --tool Bash --json

  # Incremental: only entries after a previous checkpoint
  ccvalet session result my-session --since "2026-04-26T10:00:00Z" --json

  # Show only failed tool calls
  ccvalet session result my-session --errors-only`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]
		since, _ := cmd.Flags().GetString("since")
		last, _ := cmd.Flags().GetInt("last")
		tool, _ := cmd.Flags().GetString("tool")
		errorsOnly, _ := cmd.Flags().GetBool("errors-only")
		hostFlag, _ := cmd.Flags().GetString("host")

		if last < 0 {
			return fmt.Errorf("--last must be >= 0")
		}

		client := daemon.NewClient(getSocketPath())

		sessionID, sessionName, hostID, err := resolveSession(client, nameOrID)
		if err != nil {
			return err
		}
		if hostFlag != "" {
			hostID = hostFlag
		}

		resp, err := client.Result(daemon.ResultRequest{
			ID:         sessionID,
			HostID:     hostID,
			Since:      since,
			Last:       last,
			Tool:       tool,
			ErrorsOnly: errorsOnly,
		})
		if err != nil {
			return err
		}

		if jsonOutput {
			return writeJSON(os.Stdout, resp)
		}
		return renderResultText(os.Stdout, sessionName, resp)
	},
}

func renderResultText(w io.Writer, sessionName string, resp *daemon.ResultResponse) error {
	if len(resp.Entries) == 0 {
		fmt.Fprintf(w, "%s: no entries\n", sessionName)
		return nil
	}
	for _, e := range resp.Entries {
		fmt.Fprintf(w, "[%s] %s\n", e.Timestamp, e.Type)
		for _, b := range e.Blocks {
			fmt.Fprintln(w, "  "+formatBlockSummary(b))
		}
	}
	if resp.Truncated {
		fmt.Fprintf(w, "(truncated to last %d entries)\n", len(resp.Entries))
	}
	return nil
}

func formatBlockSummary(b transcript.Block) string {
	switch b.Kind {
	case "text":
		return "text: " + firstLineSummary(b.Text, 200)
	case "thinking":
		return "thinking: " + firstLineSummary(b.Text, 120)
	case "tool_use":
		input := compactInputSummary(b.Input)
		return fmt.Sprintf("tool_use %s [%s] %s", b.ToolName, shortID(b.ToolUseID), input)
	case "tool_result":
		marker := ""
		if b.IsError {
			marker = " ERROR"
		}
		return fmt.Sprintf("tool_result%s [%s] %s", marker, shortID(b.ToolUseID), firstLineSummary(b.Output, 200))
	default:
		return b.Kind
	}
}

func firstLineSummary(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\r", "")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

func compactInputSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	s := string(out)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func init() {
	sessionCmd.AddCommand(resultCmd)

	resultCmd.Flags().String("since", "", "Return only entries with timestamp strictly greater than this ISO8601 value (entries with an exactly matching timestamp are excluded — pass the last entry's timestamp to fetch only what came after)")
	resultCmd.Flags().Int("last", 0, "Truncate to last N entries (0 = no truncation)")
	resultCmd.Flags().String("tool", "", "Keep only entries containing tool_use/tool_result for this tool name")
	resultCmd.Flags().Bool("errors-only", false, "Keep only entries with at least one tool_result.is_error=true")
	resultCmd.Flags().StringP("host", "H", "", "Target host (default: resolved from session)")
}
