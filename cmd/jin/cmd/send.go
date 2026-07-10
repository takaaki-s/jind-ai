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

type sendResult struct {
	Success bool   `json:"success"`
	Session string `json:"session"`
	// Verified is true when --wait-running was set and the session was
	// observed leaving idle within the wait window.
	Verified bool `json:"verified,omitempty"`
	// Status is the last observed status when --wait-running was set.
	Status string `json:"status,omitempty"`
}

// sendPollInterval controls how often we poll the daemon for status
// after send when --wait-running is on. Kept as a var so tests can override.
var sendPollInterval = 200 * time.Millisecond

var sendCmd = &cobra.Command{
	Use:     "send <selector> <prompt>",
	Aliases: []string{"prompt"},
	Short:   "Send a prompt to a session",
	Long: `Send a prompt to a Claude Code session. The session must be in idle status.
The selector may be an ID prefix or a description substring (case-insensitive).

Multiple arguments after the selector are joined with spaces:
  jin session send my-session Fix the bug
  jin session send my-session "Fix the bug"   # equivalent

Use "-" as the prompt to read from stdin:
  echo "Fix the bug" | jin session send my-session -

Use --wait-running to verify the prompt was picked up. Without it, send only
tells you the keystrokes were injected — a freshly started or busy TUI may
buffer them and never actually submit. With it, send polls the session status
until it leaves idle for running / thinking / permission (up to --wait-timeout
seconds), and exits non-zero on timeout so callers can retry or escalate.`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		nameOrID := args[0]

		var prompt string
		if len(args) >= 2 {
			if args[1] == "-" {
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("failed to read from stdin: %w", err)
				}
				prompt = strings.TrimRight(string(data), "\n")
			} else {
				prompt = strings.Join(args[1:], " ")
			}
		} else {
			return fmt.Errorf("prompt is required")
		}

		if prompt == "" {
			return fmt.Errorf("prompt cannot be empty")
		}

		waitRunning, _ := cmd.Flags().GetBool("wait-running")
		waitTimeout, _ := cmd.Flags().GetInt("wait-timeout")
		if waitTimeout <= 0 {
			return fmt.Errorf("--wait-timeout must be > 0")
		}

		client := daemon.NewClient(getSocketPath())

		sessionID, sessionName, err := resolveSession(client, nameOrID)
		if err != nil {
			return err
		}

		if err := client.Send(sessionID, prompt); err != nil {
			return err
		}

		result := sendResult{Success: true, Session: sessionName}

		if waitRunning {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			info, err := pollSendAccepted(ctx, clientStatusGetter{client}, sessionID, time.Duration(waitTimeout)*time.Second)
			if err != nil {
				return err
			}
			result.Verified = true
			result.Status = string(info.Status)
		}

		if jsonOutput {
			return renderSendResultJSON(os.Stdout, result)
		}
		if waitRunning {
			fmt.Printf("Sent prompt to session: %s (status: %s)\n", sessionName, result.Status)
		} else {
			fmt.Printf("Sent prompt to session: %s\n", sessionName)
		}
		return nil
	},
}

// statusGetter abstracts daemon.Client for testability of the poll loop.
type statusGetter interface {
	Get(id string) (*session.Info, error)
}

type clientStatusGetter struct{ c *daemon.Client }

func (g clientStatusGetter) Get(id string) (*session.Info, error) { return g.c.Get(id) }

// isPromptAcceptedStatus reports whether the observed status indicates the
// agent picked up the prompt and started processing.
func isPromptAcceptedStatus(s session.Status) bool {
	return s == session.StatusRunning || s == session.StatusThinking || s == session.StatusPermission
}

// pollSendAccepted polls until the session leaves idle for a status that
// indicates the agent accepted the prompt, or until ctx is done or timeout
// elapses. A "stopped" observation short-circuits with a distinct error so
// callers do not spin for the full window on a dead session.
func pollSendAccepted(ctx context.Context, g statusGetter, sessionID string, timeout time.Duration) (*session.Info, error) {
	deadline := time.Now().Add(timeout)

	check := func() (*session.Info, bool, error) {
		info, err := g.Get(sessionID)
		if err != nil {
			return nil, false, err
		}
		if isPromptAcceptedStatus(info.Status) {
			return info, true, nil
		}
		if info.Status == session.StatusStopped {
			return info, false, fmt.Errorf("session stopped before prompt could be accepted")
		}
		return info, false, nil
	}

	if info, done, err := check(); err != nil {
		return nil, err
	} else if done {
		return info, nil
	}

	ticker := time.NewTicker(sendPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("interrupted")
		case <-ticker.C:
			info, done, err := check()
			if err != nil {
				return nil, err
			}
			if done {
				return info, nil
			}
			if time.Now().After(deadline) {
				return nil, exitcode.Errorf(exitcode.Timeout,
					"timeout waiting for session to leave idle after send (last status: %s); the prompt keystrokes may have been buffered without being submitted — try again or attach the session to inspect",
					info.Status)
			}
		}
	}
}

func renderSendResultJSON(w io.Writer, result sendResult) error {
	return writeJSON(w, result)
}

func init() {
	sessionCmd.AddCommand(sendCmd)
	sendCmd.Flags().Bool("wait-running", false,
		"After send, poll status until the session leaves idle (running/thinking/permission); exits with timeout code on failure.")
	sendCmd.Flags().Int("wait-timeout", 10,
		"Seconds to wait for the session to leave idle when --wait-running is set.")
}
