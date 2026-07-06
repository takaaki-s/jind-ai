package cmd

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jindaiko/internal/daemon"
)

// completeSessionNames returns session selectors for shell completion.
//
// Both the human-readable Description and the first 8 hex chars of the
// session ID are suggested so users can complete either dimension of a
// selector. Prefix filtering is left to the shell (cobra + noFileComp).
func completeSessionNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	client := daemon.NewClient(getSocketPath())
	sessions, err := client.List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	candidates := make([]string, 0, len(sessions)*2)
	for _, s := range sessions {
		if s.Description != "" && strings.HasPrefix(s.Description, toComplete) {
			candidates = append(candidates, s.Description)
		}
		id8 := s.ID
		if len(id8) > 8 {
			id8 = id8[:8]
		}
		if id8 != "" && strings.HasPrefix(id8, toComplete) {
			candidates = append(candidates, id8)
		}
	}
	return candidates, cobra.ShellCompDirectiveNoFileComp
}
