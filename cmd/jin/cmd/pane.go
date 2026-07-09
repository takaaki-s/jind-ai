package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jindaiko/internal/daemon"
	"github.com/takaaki-s/jindaiko/internal/tmux"
)

var paneCmd = &cobra.Command{
	Use:   "pane",
	Short: "Operate a session's tmux pane",
	Long: `Open a popup or split over a session's tmux pane, capture its visible
contents, or send it keystrokes. The selector may be an ID prefix or a
description substring (case-insensitive), same as 'jin session'.`,
}

var panePopupCmd = &cobra.Command{
	Use:   "popup <selector> -- <cmd...>",
	Short: "Open a tmux popup running a command over a session's pane",
	Long: `Open a tmux popup anchored to the session's pane, running the given command
in the session's working directory. The command runs standalone inside the
popup: it does not inherit JIN_* environment variables, so pass any data it
needs directly in the command line.

With --here the popup opens over the caller's own tmux pane instead of a
session's pane, and no selector is given:
  jin pane popup --here -- less /tmp/diff.txt

Example:
  jin pane popup my-session -- less /tmp/diff.txt`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		here, _ := cmd.Flags().GetBool("here")
		title, _ := cmd.Flags().GetString("title")
		width, _ := cmd.Flags().GetString("width")
		height, _ := cmd.Flags().GetString("height")

		if here {
			return runPopupHere(strings.Join(args, " "), title, width, height)
		}

		selector := args[0]
		if len(args) < 2 {
			return errors.New("cmd is required (use -- <cmd...>)")
		}
		cmdStr := strings.Join(args[1:], " ")

		client := daemon.NewClient(getSocketPath())
		sessionID, sessionDesc, err := resolveSession(client, selector)
		if err != nil {
			return err
		}
		if err := client.PanePopup(sessionID, cmdStr, title, width, height); err != nil {
			return err
		}

		if jsonOutput {
			return renderActionResultJSON(os.Stdout, actionResult{Success: true, ID: sessionID, Description: sessionDesc})
		}
		fmt.Printf("Opened popup for session: %s\n", sessionDesc)
		return nil
	},
}

// runPopupHere opens a popup over the caller's own tmux pane, bypassing the
// daemon. The target server is discovered from $TMUX (human invocation) or
// JIN_CALLER_TMUX_SOCKET (plugin action invocation); the anchor pane likewise
// from $TMUX_PANE or JIN_CALLER_TMUX_PANE.
func runPopupHere(cmdStr, title, width, height string) error {
	socketPath := tmux.SocketPathFromEnv(os.Getenv("TMUX"))
	if socketPath == "" {
		socketPath = os.Getenv("JIN_CALLER_TMUX_SOCKET")
	}
	if socketPath == "" {
		return errors.New("--here requires a tmux client: not inside tmux and no JIN_CALLER_TMUX_SOCKET")
	}

	anchorPane := os.Getenv("TMUX_PANE")
	if anchorPane == "" {
		anchorPane = os.Getenv("JIN_CALLER_TMUX_PANE")
	}

	tc, err := tmux.NewClientWithSocketPath(socketPath)
	if err != nil {
		return err
	}
	return tc.DisplayPopup(tmux.DisplayPopupOptions{
		Target: anchorPane,
		Width:  width,
		Height: height,
		Title:  title,
		Cmd:    cmdStr,
	})
}

var paneSplitCmd = &cobra.Command{
	Use:   "split <selector> [-- <cmd...>]",
	Short: "Split a session's tmux pane and run a command in the new pane",
	Long: `Split the session's pane and, if given, run a command in the new pane.
Without a command, the new pane just opens a shell.

Example:
  jin pane split my-session --horizontal --percent 30 -- htop`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		selector := args[0]
		var cmdStr string
		if len(args) >= 2 {
			cmdStr = strings.Join(args[1:], " ")
		}

		horizontal, _ := cmd.Flags().GetBool("horizontal")
		percent, _ := cmd.Flags().GetInt("percent")

		client := daemon.NewClient(getSocketPath())
		sessionID, sessionDesc, err := resolveSession(client, selector)
		if err != nil {
			return err
		}
		if err := client.PaneSplit(sessionID, cmdStr, horizontal, percent); err != nil {
			return err
		}

		if jsonOutput {
			return renderActionResultJSON(os.Stdout, actionResult{Success: true, ID: sessionID, Description: sessionDesc})
		}
		fmt.Printf("Split pane for session: %s\n", sessionDesc)
		return nil
	},
}

var paneCaptureCmd = &cobra.Command{
	Use:               "capture <selector>",
	Short:             "Capture the visible contents of a session's tmux pane",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		selector := args[0]
		ansi, _ := cmd.Flags().GetBool("ansi")

		client := daemon.NewClient(getSocketPath())
		sessionID, _, err := resolveSession(client, selector)
		if err != nil {
			return err
		}
		content, err := client.PaneCapture(sessionID, ansi)
		if err != nil {
			return err
		}
		fmt.Println(content)
		return nil
	},
}

var paneSendKeysCmd = &cobra.Command{
	Use:   "send-keys <selector> <keys...>",
	Short: "Send keys to a session's tmux pane",
	Long: `Send keys to the session's tmux pane. By default keys are typed verbatim
(--literal). Pass --literal=false to send tmux key names instead (e.g. Enter,
C-c) rather than the literal text.`,
	Args:              cobra.MinimumNArgs(2),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		selector := args[0]
		keys := strings.Join(args[1:], " ")
		literal, _ := cmd.Flags().GetBool("literal")

		client := daemon.NewClient(getSocketPath())
		sessionID, sessionDesc, err := resolveSession(client, selector)
		if err != nil {
			return err
		}
		if err := client.PaneSendKeys(sessionID, keys, literal); err != nil {
			return err
		}

		if jsonOutput {
			return renderActionResultJSON(os.Stdout, actionResult{Success: true, ID: sessionID, Description: sessionDesc})
		}
		fmt.Printf("Sent keys to session: %s\n", sessionDesc)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(paneCmd)
	paneCmd.AddCommand(panePopupCmd, paneSplitCmd, paneCaptureCmd, paneSendKeysCmd)

	panePopupCmd.Flags().Bool("here", false, "Open the popup over the caller's own tmux pane instead of a session's pane (no selector)")
	panePopupCmd.Flags().String("title", "", "Popup title (tmux 3.3+)")
	panePopupCmd.Flags().String("width", "", `Popup width (e.g. "80%")`)
	panePopupCmd.Flags().String("height", "", `Popup height (e.g. "80%")`)

	paneSplitCmd.Flags().BoolP("horizontal", "H", false, "Split left-right instead of top-bottom")
	paneSplitCmd.Flags().Int("percent", 0, "Size of the new pane as a percentage")

	paneCaptureCmd.Flags().Bool("ansi", false, "Include ANSI escape sequences in the captured output")

	paneSendKeysCmd.Flags().Bool("literal", true, "Type keys verbatim; set to false to send tmux key names")
}
