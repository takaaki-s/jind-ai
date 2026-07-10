package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
)

var (
	cleanupDryRun bool
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Clean up sessions",
	Long: `Clean up stopped sessions.

Examples:
  jin cleanup stopped              # Delete all stopped sessions
  jin cleanup stopped --dry-run    # Show what would be deleted`,
}

var cleanupStoppedCmd = &cobra.Command{
	Use:   "stopped",
	Short: "Delete all stopped sessions",
	Long:  `Delete all stopped sessions.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client := daemon.NewClient(getSocketPath())

		sessions, err := client.List()
		if err != nil {
			return fmt.Errorf("failed to list sessions: %w", err)
		}

		var stoppedSessions []session.Info
		for _, s := range sessions {
			if s.Status == session.StatusStopped {
				stoppedSessions = append(stoppedSessions, s)
			}
		}

		if len(stoppedSessions) == 0 {
			fmt.Println("No stopped sessions found.")
			return nil
		}

		fmt.Printf("Found %d stopped session(s):\n", len(stoppedSessions))
		for _, s := range stoppedSessions {
			fmt.Printf("  - %s (%s)\n", s.Description, s.ID[:8])
		}

		if cleanupDryRun {
			fmt.Println("\nDry run mode - no changes made.")
			return nil
		}

		fmt.Println()

		deletedSessions := 0
		for _, s := range stoppedSessions {
			if err := client.Delete(s.ID, false, false); err != nil {
				fmt.Printf("Warning: failed to delete session %s: %v\n", s.Description, err)
			} else {
				fmt.Printf("Deleted session: %s (%s)\n", s.Description, s.ID[:8])
				deletedSessions++
			}
		}

		fmt.Printf("\nCleanup complete: %d session(s) deleted\n", deletedSessions)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(cleanupCmd)
	cleanupCmd.AddCommand(cleanupStoppedCmd)

	cleanupStoppedCmd.Flags().BoolVar(&cleanupDryRun, "dry-run", false, "Show what would be deleted without actually deleting")
}
