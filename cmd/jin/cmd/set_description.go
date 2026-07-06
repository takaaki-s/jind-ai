package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jindaiko/internal/daemon"
)

var setDescriptionCmd = &cobra.Command{
	Use:   "set-description <selector> <description>",
	Short: "Update a session's description (empty value resets to auto-generated)",
	Long: `Update a session's description. The selector may be an ID prefix or a
description substring (case-insensitive).

Setting a non-empty description locks it (it will no longer be updated
automatically). Passing an empty string unlocks the session and regenerates
the auto-generated baseline description.

Examples:
  jin session set-description abcd1234 "auth refactor"
  jin session set-description abcd1234 ""`,
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		selector, desc := args[0], args[1]
		client := daemon.NewClient(getSocketPath())

		sess, err := resolveSelector(client, selector)
		if err != nil {
			return err
		}

		if err := client.SetDescription(sess.ID, desc); err != nil {
			return err
		}

		// Re-fetch to report the resulting description, since an empty desc
		// triggers server-side baseline regeneration rather than echoing back
		// what was passed in.
		updated, err := client.Get(sess.ID)
		if err != nil {
			return err
		}
		fmt.Printf("Updated description: %s\n", updated.Description)
		return nil
	},
}

func init() {
	sessionCmd.AddCommand(setDescriptionCmd)
}
