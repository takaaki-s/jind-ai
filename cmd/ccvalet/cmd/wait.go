package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/claude-code-valet/internal/daemon"
	"github.com/takaaki-s/claude-code-valet/internal/exitcode"
	"github.com/takaaki-s/claude-code-valet/internal/session"
)

var waitCmd = &cobra.Command{
	Use:   "wait <session-name>",
	Short: "Wait for a session to reach a specific status",
	Long: `Wait for a Claude Code session to reach a specific status.
Polls the session status every 2 seconds until the target status is reached or timeout occurs.

Examples:
  ccvalet session wait my-session --status idle --timeout 300
  ccvalet session wait my-session --status idle --json`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]
		targetStatus, _ := cmd.Flags().GetString("status")
		timeout, _ := cmd.Flags().GetInt("timeout")

		client := daemon.NewClient(getSocketPath())

		sessionID, sessionName, hostID, err := resolveSession(client, nameOrID)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		info, err := pollSessionStatus(ctx, client, sessionID, hostID, session.Status(targetStatus), time.Duration(timeout)*time.Second)
		if err != nil {
			return err
		}

		if jsonOutput {
			return renderWaitResultJSON(os.Stdout, info)
		}
		fmt.Printf("Session %s is now %s\n", sessionName, info.Status)
		return nil
	},
}

func pollSessionStatus(ctx context.Context, client *daemon.Client, sessionID, hostID string, targetStatus session.Status, timeout time.Duration) (*session.Info, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Check immediately before first tick
	info, err := client.Get(sessionID, hostID)
	if err != nil {
		return nil, err
	}
	if info.Status == targetStatus {
		return info, nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("interrupted")
		case <-timer.C:
			return nil, exitcode.Errorf(exitcode.Timeout, "timeout waiting for session to reach status %q", targetStatus)
		case <-ticker.C:
			info, err := client.Get(sessionID, hostID)
			if err != nil {
				return nil, err
			}
			if info.Status == targetStatus {
				return info, nil
			}
		}
	}
}

func renderWaitResultJSON(w io.Writer, info *session.Info) error {
	return writeJSON(w, info)
}

func init() {
	sessionCmd.AddCommand(waitCmd)

	waitCmd.Flags().String("status", "idle", "Target status to wait for")
	waitCmd.Flags().Int("timeout", 300, "Timeout in seconds")
}
