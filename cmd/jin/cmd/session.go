package cmd

import (
	"github.com/spf13/cobra"
)

var sessionCmd = &cobra.Command{
	Use:     "session",
	Aliases: []string{"sess"},
	Short:   "Manage coding-agent sessions",
	Long:    `Create, list, attach, and manage coding-agent sessions.`,
}

func init() {
	rootCmd.AddCommand(sessionCmd)
}
