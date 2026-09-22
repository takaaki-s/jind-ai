package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/tmux"
)

var adoptCmd = &cobra.Command{
	Use:   "adopt <tmux-pane>",
	Short: "Link an existing local tmux pane without restarting it",
	Long: `Inspect and adopt a pane that is already running in a local tmux server.

Adoption is two-phase and never changes the pane. First run --dry-run and
inspect the exact server, session, window, pane, PID, command, cwd, process
ancestry, current owner, and effective capabilities. Then repeat with
--confirm and the printed --confirmation-key. If the pane moved, exited, or
was reused between those commands, confirmation fails closed.

The agent kind must be supplied explicitly. Adoption does not inject hooks,
rewrite the command, or create resumable agent state. Deleting an adopted
session only removes the jin record; it never kills the foreign pane.

By default the command uses the current $TMUX socket when run inside tmux,
otherwise tmux's default server. Use --tmux-socket for a named -L server or
--tmux-socket-path for an exact -S socket path.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := adoptOptions(cmd, args[0])
		if err != nil {
			return err
		}
		out, err := daemon.NewClient(getSocketPath()).Adopt(opts)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), out)
		}
		if opts.DryRun {
			printAdoptionPreview(cmd, out)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Adopted session: %s (%s)\n", out.Session.Description, out.Session.ID)
		fmt.Fprintf(cmd.OutOrStdout(), "Pane: %s on %s (PID %d unchanged)\n",
			out.Session.TmuxPaneID, out.Session.TmuxBinding.Server.DisplayName(), out.Session.TmuxBinding.PanePID)
		fmt.Fprintf(cmd.OutOrStdout(), "To attach: jin session attach %s\n", out.Session.ID)
		return nil
	},
}

func adoptOptions(cmd *cobra.Command, target string) (daemon.AdoptOptions, error) {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	confirm, _ := cmd.Flags().GetBool("confirm")
	if dryRun == confirm {
		return daemon.AdoptOptions{}, fmt.Errorf("choose exactly one of --dry-run or --confirm")
	}
	agentKind, _ := cmd.Flags().GetString("agent")
	if agentKind == "" {
		return daemon.AdoptOptions{}, fmt.Errorf("--agent is required until agent detection is available")
	}
	key, _ := cmd.Flags().GetString("confirmation-key")
	if confirm && key == "" {
		return daemon.AdoptOptions{}, fmt.Errorf("--confirm requires --confirmation-key from --dry-run")
	}
	socketName, _ := cmd.Flags().GetString("tmux-socket")
	socketPath, _ := cmd.Flags().GetString("tmux-socket-path")
	if socketName != "" && socketPath != "" {
		return daemon.AdoptOptions{}, fmt.Errorf("choose only one of --tmux-socket or --tmux-socket-path")
	}
	server := tmux.ServerRef{Kind: tmux.ServerDefault}
	switch {
	case socketName != "":
		server = tmux.ServerRef{Kind: tmux.ServerName, Value: socketName}
	case socketPath != "":
		server = tmux.ServerRef{Kind: tmux.ServerPath, Value: socketPath}
	case tmux.SocketPathFromEnv(os.Getenv("TMUX")) != "":
		server = tmux.ServerRef{Kind: tmux.ServerPath, Value: tmux.SocketPathFromEnv(os.Getenv("TMUX"))}
	}
	description, _ := cmd.Flags().GetString("description")
	fleet, _ := cmd.Flags().GetString("fleet")
	return daemon.AdoptOptions{
		Server: server, Target: target, AgentKind: agentKind,
		Description: description, Fleet: fleet, ConfirmationKey: key,
		DryRun: dryRun, Confirm: confirm,
	}, nil
}

func printAdoptionPreview(cmd *cobra.Command, out *daemon.AdoptResponse) {
	p := out.Preview
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "Adoption preview (read-only)")
	fmt.Fprintf(w, "Server:  %s\n", p.Server.DisplayName())
	fmt.Fprintf(w, "Target:  %s  [%s / %s / %s]\n", p.Pane.CanonicalTarget(), p.Pane.SessionID, p.Pane.WindowID, p.Pane.PaneID)
	fmt.Fprintf(w, "Command: %s\n", p.Pane.CurrentCommand)
	if p.Pane.StartCommand != "" && p.Pane.StartCommand != p.Pane.CurrentCommand {
		fmt.Fprintf(w, "Started: %s\n", p.Pane.StartCommand)
	}
	fmt.Fprintf(w, "CWD:     %s\n", p.Pane.CurrentPath)
	fmt.Fprintf(w, "PID:     %d (started %s)\n", p.Pane.PanePID, p.Pane.PaneStarted)
	if p.Owner == nil {
		fmt.Fprintln(w, "Owner:   unowned")
	} else {
		fmt.Fprintf(w, "Owner:   %s (%s)\n", p.Owner.Description, p.Owner.ID)
	}
	fmt.Fprintln(w, "Process ancestry:")
	for _, proc := range p.Pane.ProcessAncestry {
		fmt.Fprintf(w, "  %d <- %d  %s\n", proc.PID, proc.PPID, proc.Command)
	}
	fmt.Fprintf(w, "Capabilities: liveness=%s send=%s respond=%s resume=%s hooks=%s transcript=%s needs-answer=%s\n",
		p.Capabilities.Liveness, p.Capabilities.Send, p.Capabilities.Respond,
		p.Capabilities.Resume, p.Capabilities.Hooks, p.Capabilities.Transcript,
		p.Capabilities.ReliableNeedsAnswer)
	fmt.Fprintf(w, "Confirmation key: %s\n", p.ConfirmationKey)
	fmt.Fprintf(w, "No changes made. Repeat with --confirm --confirmation-key %s\n", p.ConfirmationKey)
}

func init() {
	sessionCmd.AddCommand(adoptCmd)
	adoptCmd.Flags().String("agent", "", "Agent adapter kind for this pane (required)")
	adoptCmd.Flags().StringP("description", "d", "", "Session description (default: command and exact pane target)")
	adoptCmd.Flags().StringP("fleet", "f", "", "Fleet name (default: \"default\")")
	adoptCmd.Flags().String("tmux-socket", "", "Named tmux server used with tmux -L")
	adoptCmd.Flags().String("tmux-socket-path", "", "Exact tmux socket path used with tmux -S")
	adoptCmd.Flags().Bool("dry-run", false, "Inspect the pane without changing tmux or jin state")
	adoptCmd.Flags().Bool("confirm", false, "Adopt the exact pane inspected by --dry-run")
	adoptCmd.Flags().String("confirmation-key", "", "Exact key printed by --dry-run (required with --confirm)")
}
