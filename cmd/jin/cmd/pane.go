package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/tmux"
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
			width, height = popupSizeWithEnvFallback(width, height)
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

// callerTmux resolves the caller's own tmux server and anchor pane for --here
// mode, bypassing the daemon. The server is discovered from $TMUX (human
// invocation) or JIN_CALLER_TMUX_SOCKET (plugin action invocation); the anchor
// pane likewise from $TMUX_PANE or JIN_CALLER_TMUX_PANE (may be empty; popup
// treats empty as "active client", so this variant does not require it).
func callerTmux() (*tmux.Client, string, error) {
	socketPath := envFallback(tmux.SocketPathFromEnv(os.Getenv("TMUX")), "JIN_CALLER_TMUX_SOCKET")
	if socketPath == "" {
		return nil, "", errors.New("--here requires a tmux client: not inside tmux and no JIN_CALLER_TMUX_SOCKET")
	}
	anchorPane := envFallback(os.Getenv("TMUX_PANE"), "JIN_CALLER_TMUX_PANE")
	tc, err := tmux.NewClientWithSocketPath(socketPath)
	if err != nil {
		return nil, "", err
	}
	return tc, anchorPane, nil
}

// callerTmuxWithPane is callerTmux plus a "must have a concrete anchor pane"
// guard, for --here operations (split, close) that address a specific pane
// rather than the client's active one.
func callerTmuxWithPane() (*tmux.Client, string, error) {
	tc, anchorPane, err := callerTmux()
	if err != nil {
		return nil, "", err
	}
	if anchorPane == "" {
		return nil, "", errors.New("--here requires a caller pane: no $TMUX_PANE and no JIN_CALLER_TMUX_PANE")
	}
	return tc, anchorPane, nil
}

// runPopupHere opens a popup over the caller's own tmux pane.
func runPopupHere(cmdStr, title, width, height string) error {
	tc, anchorPane, err := callerTmux()
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

// envFallback returns v when it's non-empty, otherwise the named env var's
// value. Kept small and single-purpose so the "flag beats env, env beats
// nothing" priority is obvious at each call site.
func envFallback(v, envKey string) string {
	if v != "" {
		return v
	}
	return os.Getenv(envKey)
}

// popupSizeWithEnvFallback fills in width/height from the JIN_PLUGIN_POPUP_*
// env vars (set by the plugin dispatcher, see internal/plugin/exec.go) when
// the corresponding flag was left empty. Explicit flags always win; if both
// the flag and the env var are empty, the value stays empty and tmux falls
// back to its own default.
func popupSizeWithEnvFallback(flagWidth, flagHeight string) (width, height string) {
	return envFallback(flagWidth, "JIN_PLUGIN_POPUP_WIDTH"),
		envFallback(flagHeight, "JIN_PLUGIN_POPUP_HEIGHT")
}

var paneSplitCmd = &cobra.Command{
	Use:   "split <selector> [-- <cmd...>]",
	Short: "Split a session's tmux pane and run a command in the new pane",
	Long: `Split the session's pane in its working directory and, if given, run a
command in the new pane. Without a command, the new pane just opens a shell.
On success the new pane's ID is printed.

With --name the split becomes idempotent (a "named slot"): when a pane with
that name already exists in the session's window, no new pane is created and
--if-exists decides what happens instead — noop reuses it as-is (default),
respawn restarts it with the given command, error fails. Close a named slot
with 'jin pane close'.

With --here the caller's own tmux pane is split instead of a session's pane,
and no selector is given:
  jin pane split --here --direction down --size 20% -- htop

Example:
  jin pane split my-session --direction right --size 30% --name monitor -- htop`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		here, _ := cmd.Flags().GetBool("here")
		full, _ := cmd.Flags().GetBool("full")
		noFocus, _ := cmd.Flags().GetBool("no-focus")
		name, _ := cmd.Flags().GetString("name")
		ifExists, _ := cmd.Flags().GetString("if-exists")

		direction, size, err := splitGeometryFromFlags(cmd)
		if err != nil {
			return err
		}
		opts := tmux.SplitOptions{
			Direction: direction,
			Size:      size,
			Full:      full,
			NoFocus:   noFocus,
		}
		// Validate everything locally so --here (no daemon round-trip) and the
		// selector path reject bad input with the same messages.
		if err := tmux.ValidateSlotOptions(name, ifExists, opts); err != nil {
			return err
		}

		if here {
			opts.Cmd = strings.Join(args, " ")
			paneID, err := runSplitHere(opts, name, ifExists)
			if err != nil {
				return err
			}
			if jsonOutput {
				return renderActionResultJSON(os.Stdout, actionResult{Success: true, PaneID: paneID})
			}
			fmt.Printf("Split pane: %s\n", paneID)
			return nil
		}

		if len(args) < 1 {
			return errors.New("selector is required (or use --here)")
		}
		selector := args[0]
		var cmdStr string
		if len(args) >= 2 {
			cmdStr = strings.Join(args[1:], " ")
		}

		client := daemon.NewClient(getSocketPath())
		sessionID, sessionDesc, err := resolveSession(client, selector)
		if err != nil {
			return err
		}
		paneID, err := client.PaneSplit(daemon.PaneSplitRequest{
			ID:        sessionID,
			Cmd:       cmdStr,
			Direction: direction,
			Size:      size,
			Full:      full,
			NoFocus:   noFocus,
			Name:      name,
			IfExists:  ifExists,
		})
		if err != nil {
			return err
		}

		if jsonOutput {
			return renderActionResultJSON(os.Stdout, actionResult{Success: true, ID: sessionID, Description: sessionDesc, PaneID: paneID})
		}
		fmt.Printf("Split pane %s for session: %s\n", paneID, sessionDesc)
		return nil
	},
}

// splitGeometryFromFlags resolves --direction/--size together with the
// deprecated --horizontal/--percent aliases. Combining a new flag with its
// deprecated counterpart is an error instead of one silently winning.
func splitGeometryFromFlags(cmd *cobra.Command) (direction, size string, err error) {
	direction, _ = cmd.Flags().GetString("direction")
	size, _ = cmd.Flags().GetString("size")
	if cmd.Flags().Changed("horizontal") {
		if cmd.Flags().Changed("direction") {
			return "", "", errors.New("--horizontal conflicts with --direction (use --direction right)")
		}
		if horizontal, _ := cmd.Flags().GetBool("horizontal"); horizontal {
			direction = "right"
		}
	}
	if cmd.Flags().Changed("percent") {
		if cmd.Flags().Changed("size") {
			return "", "", errors.New(`--percent conflicts with --size (use --size "N%")`)
		}
		percent, _ := cmd.Flags().GetInt("percent")
		size = fmt.Sprintf("%d%%", percent)
	}
	return direction, size, nil
}

// runSplitHere splits the caller's own tmux pane. Unlike the selector path,
// which the daemon serializes, --here runs unarbitrated: concurrent calls for
// the same slot name from the same window are not guaranteed idempotent.
func runSplitHere(opts tmux.SplitOptions, name, ifExists string) (string, error) {
	tc, anchorPane, err := callerTmuxWithPane()
	if err != nil {
		return "", err
	}
	return tmux.EnsureNamedPane(tc, anchorPane, name, ifExists, opts)
}

var paneCloseCmd = &cobra.Command{
	Use:   "close <selector> --name <name>",
	Short: "Close a named pane created by 'jin pane split --name'",
	Long: `Close (kill) the pane with the given slot name in the session's window.

With --here the pane is looked up in the caller's own tmux window instead,
and no selector is given:
  jin pane close --here --name monitor`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		here, _ := cmd.Flags().GetBool("here")
		name, _ := cmd.Flags().GetString("name")
		if err := tmux.ValidatePaneName(name); err != nil {
			return err
		}

		if here {
			if err := runCloseHere(name); err != nil {
				return err
			}
			if jsonOutput {
				return renderActionResultJSON(os.Stdout, actionResult{Success: true})
			}
			fmt.Printf("Closed pane: %s\n", name)
			return nil
		}

		if len(args) != 1 {
			return errors.New("selector is required (or use --here)")
		}
		client := daemon.NewClient(getSocketPath())
		sessionID, sessionDesc, err := resolveSession(client, args[0])
		if err != nil {
			return err
		}
		if err := client.PaneClose(sessionID, name); err != nil {
			return err
		}

		if jsonOutput {
			return renderActionResultJSON(os.Stdout, actionResult{Success: true, ID: sessionID, Description: sessionDesc})
		}
		fmt.Printf("Closed pane %q for session: %s\n", name, sessionDesc)
		return nil
	},
}

// runCloseHere kills the named pane in the caller's own tmux window.
func runCloseHere(name string) error {
	tc, anchorPane, err := callerTmuxWithPane()
	if err != nil {
		return err
	}
	return tmux.CloseNamedPane(tc, anchorPane, name, anchorPane)
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

// registerSplitFlags wires the split subcommand's flag set. Extracted so tests
// can exercise splitGeometryFromFlags against a fresh command.
func registerSplitFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("here", false, "Split the caller's own tmux pane instead of a session's pane (no selector)")
	cmd.Flags().StringP("direction", "d", "down", "Where the new pane opens: down, up, left or right")
	cmd.Flags().String("size", "", `New pane size: a percentage ("30%") or lines/columns ("15")`)
	cmd.Flags().Bool("full", false, "Span the full window width/height instead of splitting the current pane")
	cmd.Flags().Bool("no-focus", false, "Keep focus on the current pane")
	cmd.Flags().String("name", "", "Named slot: reuse the pane with this name instead of splitting again")
	cmd.Flags().String("if-exists", "", "Named-slot policy when the pane already exists: noop (default), respawn or error")
	cmd.Flags().BoolP("horizontal", "H", false, "Split left-right instead of top-bottom")
	cmd.Flags().Int("percent", 0, "Size of the new pane as a percentage")
	_ = cmd.Flags().MarkDeprecated("horizontal", "use --direction right")
	_ = cmd.Flags().MarkDeprecated("percent", `use --size "N%"`)
}

func init() {
	rootCmd.AddCommand(paneCmd)
	paneCmd.AddCommand(panePopupCmd, paneSplitCmd, paneCloseCmd, paneCaptureCmd, paneSendKeysCmd)

	panePopupCmd.Flags().Bool("here", false, "Open the popup over the caller's own tmux pane instead of a session's pane (no selector)")
	panePopupCmd.Flags().String("title", "", "Popup title (tmux 3.3+)")
	panePopupCmd.Flags().String("width", "", `Popup width (e.g. "80%")`)
	panePopupCmd.Flags().String("height", "", `Popup height (e.g. "80%")`)

	registerSplitFlags(paneSplitCmd)

	paneCloseCmd.Flags().Bool("here", false, "Close a pane in the caller's own tmux window instead of a session's window (no selector)")
	paneCloseCmd.Flags().String("name", "", "Name of the pane to close")
	_ = paneCloseCmd.MarkFlagRequired("name")

	paneCaptureCmd.Flags().Bool("ansi", false, "Include ANSI escape sequences in the captured output")

	paneSendKeysCmd.Flags().Bool("literal", true, "Type keys verbatim; set to false to send tmux key names")
}
