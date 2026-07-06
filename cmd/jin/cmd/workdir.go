package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jindaiko/internal/daemon"
)

var workdirCmd = &cobra.Command{
	Use:   "workdir <selector>",
	Short: "Print the working directory of a session",
	Long: `Print the working directory path of a session.

The selector may be an ID prefix or a description substring (case-insensitive).

Useful for shell integration:
  cd $(jin session workdir <selector>)

Or define a shell function in your .bashrc/.zshrc:
  cc-cd() { cd "$(jin session workdir "$1")"; }`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeSessionNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		client := daemon.NewClient(getSocketPath())

		sess, err := resolveSelector(client, args[0])
		if err != nil {
			return err
		}
		if sess.WorkDir == "" {
			return fmt.Errorf("session %s has no working directory", sess.Description)
		}
		fmt.Println(sess.WorkDir)
		return nil
	},
}

func init() {
	sessionCmd.AddCommand(workdirCmd)
}
