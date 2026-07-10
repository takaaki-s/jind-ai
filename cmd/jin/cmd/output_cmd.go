package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/transcript"
)

var outputCmd = &cobra.Command{
	Use:   "output <selector>",
	Short: "Get the output of a session",
	Long: `Get the conversation output from a Claude Code session.

By default, shows the last assistant message. Use --last N to get the last N conversation pairs.
The selector may be an ID prefix or a description substring (case-insensitive).

Examples:
  jin session output my-session
  jin session output my-session --last 3
  jin session output my-session --last 3 --json`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]
		lastN, _ := cmd.Flags().GetInt("last")

		client := daemon.NewClient(getSocketPath())

		sessionID, _, err := resolveSession(client, nameOrID)
		if err != nil {
			return err
		}

		info, err := client.Get(sessionID)
		if err != nil {
			return err
		}

		if info.AgentSessionID == "" {
			return fmt.Errorf("session has no agent session ID (session may not have started yet)")
		}

		workDir := transcriptWorkDir(info)
		reader := transcript.NewReader()

		if lastN > 0 {
			msgs, err := reader.GetConversation(workDir, info.AgentSessionID, lastN)
			if err != nil {
				return fmt.Errorf("failed to read conversation: %w", err)
			}

			if jsonOutput {
				// Ensure JSON outputs [] instead of null for empty results
				if msgs == nil {
					msgs = []transcript.Message{}
				}
				return renderOutputJSON(os.Stdout, msgs)
			}

			for _, msg := range msgs {
				fmt.Fprintf(os.Stdout, "[%s] %s\n", msg.Type, msg.Content)
			}
			return nil
		}

		// Default: last assistant/user message with plain text.
		msg, err := reader.GetLastMessage(workDir, info.AgentSessionID)
		if err != nil {
			return fmt.Errorf("failed to read transcript: %w", err)
		}
		if msg == nil {
			// GetLastMessage returns nil either because no transcript file exists
			// yet, or because the file exists but every user/assistant entry so
			// far consists only of tool_use / thinking blocks with no plain text.
			// The old error ("no messages found in transcript") conflated the two
			// and misled callers when the session had, in fact, done real work.
			return fmt.Errorf("no plain-text messages found; the session may not have produced any text yet — try 'jin session result --json' to inspect tool_use / tool_result activity")
		}

		if jsonOutput {
			return renderOutputJSON(os.Stdout, msg)
		}

		fmt.Println(msg.Content)
		return nil
	},
}

// transcriptWorkDir picks the directory to feed to the transcript reader.
// CurrentWorkDir (tracked by daemon polling) reflects where the agent
// actually is right now — after `cd` into a subdir or worktree — while
// WorkDir is the directory the session was launched in. Claude Code writes
// its JSONL under a projects/<encoded-workdir>/ tree, so the two can point
// at different files. We prefer CurrentWorkDir; the transcript reader's
// glob fallback covers the rest.
func transcriptWorkDir(info *session.Info) string {
	if info.CurrentWorkDir != "" {
		return info.CurrentWorkDir
	}
	return info.WorkDir
}

func renderOutputJSON(w io.Writer, v any) error {
	return writeJSON(w, v)
}

func init() {
	sessionCmd.AddCommand(outputCmd)

	outputCmd.Flags().Int("last", 0, "Number of conversation pairs to show")
}
