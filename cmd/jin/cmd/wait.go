package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/exitcode"
	"github.com/takaaki-s/jind-ai/internal/session"
)

var waitCmd = &cobra.Command{
	Use:   "wait <selector>",
	Short: "Wait for a session to reach a specific status",
	Long: `Wait for a Claude Code session to reach one of the target statuses.
Polls the session status every 2 seconds until a target is reached or timeout occurs.
The selector may be an ID prefix or a description substring (case-insensitive).

By default, waits for "idle". Pass --until to wait for any of several statuses
(useful for orchestration where "permission" is also an acceptable terminal state).

Examples:
  jin session wait my-session --status idle --timeout 300
  jin session wait my-session --until idle,permission --json
  jin session wait my-session --until idle,permission --timeout 600`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]
		targetStatus, _ := cmd.Flags().GetString("status")
		untilFlag, _ := cmd.Flags().GetString("until")
		timeout, _ := cmd.Flags().GetInt("timeout")

		targets, err := parseWaitTargets(targetStatus, untilFlag)
		if err != nil {
			return err
		}

		client := daemon.NewClient(getSocketPath())

		sessionID, sessionName, err := resolveSession(client, nameOrID)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		info, err := pollSessionStatusAny(ctx, client, sessionID, targets, time.Duration(timeout)*time.Second)
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

// parseWaitTargets resolves the effective target status set.
// If --until is set, it takes priority and overrides --status.
// Otherwise, --status is treated as the single target.
func parseWaitTargets(status, until string) ([]session.Status, error) {
	source := status
	if until != "" {
		source = until
	}
	parts := strings.Split(source, ",")
	out := make([]session.Status, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, session.Status(s))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no target status specified")
	}
	return out, nil
}

// pollSessionStatusAny polls until the session's status matches any of the given targets,
// or until ctx is done or timeout elapses.
func pollSessionStatusAny(ctx context.Context, client *daemon.Client, sessionID string, targets []session.Status, timeout time.Duration) (*session.Info, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	matches := func(s session.Status) bool {
		for _, t := range targets {
			if s == t {
				return true
			}
		}
		return false
	}

	// Check immediately before first tick
	info, err := client.Get(sessionID)
	if err != nil {
		return nil, err
	}
	if matches(info.Status) {
		return info, nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("interrupted")
		case <-timer.C:
			return nil, exitcode.Errorf(exitcode.Timeout, "timeout waiting for session to reach status %s", formatTargets(targets))
		case <-ticker.C:
			info, err := client.Get(sessionID)
			if err != nil {
				return nil, err
			}
			if matches(info.Status) {
				return info, nil
			}
		}
	}
}

func formatTargets(targets []session.Status) string {
	parts := make([]string, len(targets))
	for i, t := range targets {
		parts[i] = string(t)
	}
	return strings.Join(parts, "|")
}

func renderWaitResultJSON(w io.Writer, info *session.Info) error {
	return writeJSON(w, info)
}

func init() {
	sessionCmd.AddCommand(waitCmd)

	waitCmd.Flags().String("status", "idle", "Target status to wait for (single value; ignored if --until is set)")
	waitCmd.Flags().String("until", "", "Comma-separated target statuses (e.g. \"idle,permission\"); takes priority over --status")
	waitCmd.Flags().Int("timeout", 300, "Timeout in seconds")
}
