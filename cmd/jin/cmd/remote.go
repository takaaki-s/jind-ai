package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/remote"
)

var remoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Inspect and serve explicit remote execution targets",
}

var remotePreflightCmd = &cobra.Command{
	Use:   "preflight <target>",
	Short: "Verify SSH identity, capabilities, and a configured repository",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		repository, _ := cmd.Flags().GetString("repository")
		result, err := daemon.NewClient(getSocketPath()).PreflightRemoteTarget(daemon.RemoteTargetPreflightRequest{
			Target: args[0], Repository: repository,
		})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), result)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Target: %s\n", result.TargetID)
		fmt.Fprintf(cmd.OutOrStdout(), "Target revision: %s\n", result.TargetRevision)
		fmt.Fprintf(cmd.OutOrStdout(), "Server instance: %s\n", result.Server.InstanceID)
		fmt.Fprintf(cmd.OutOrStdout(), "Server boot: %s\n", result.Server.BootID)
		fmt.Fprintf(cmd.OutOrStdout(), "Remote jin: %s\n", result.Server.JinVersion)
		fmt.Fprintf(cmd.OutOrStdout(), "Repository: %s\n", result.Repository.RepositoryID)
		fmt.Fprintf(cmd.OutOrStdout(), "Repository identity: %s\n", result.Repository.RepositoryIdentity)
		fmt.Fprintf(cmd.OutOrStdout(), "Default branch: %s\n", result.Repository.DefaultBranch)
		fmt.Fprintf(cmd.OutOrStdout(), "Agents: %s\n", strings.Join(result.Repository.AvailableAgentKinds, ", "))
		fmt.Fprintf(cmd.OutOrStdout(), "Capabilities: %s\n", strings.Join(result.Capabilities, ", "))
		return nil
	},
}

var remoteServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the bounded remote protocol on stdin/stdout",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		stdio, _ := cmd.Flags().GetBool("stdio")
		if !stdio {
			return fmt.Errorf("--stdio is required")
		}
		if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return fmt.Errorf("remote serve --stdio refuses a TTY")
		}
		return remote.Serve(cmd.InOrStdin(), cmd.OutOrStdout(), daemon.NewClient(getSocketPath()))
	},
}

func init() {
	rootCmd.AddCommand(remoteCmd)
	remoteCmd.AddCommand(remotePreflightCmd, remoteServeCmd)
	remotePreflightCmd.Flags().String("repository", "", "Configured repository label on this target (required)")
	_ = remotePreflightCmd.MarkFlagRequired("repository")
	remoteServeCmd.Flags().Bool("stdio", false, "Read and write the framed protocol on standard streams")
}
