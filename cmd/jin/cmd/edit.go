package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"golang.org/x/term"
)

var editCmd = &cobra.Command{
	Use:   "edit <selector>",
	Short: "Open editor in session's working directory",
	Long: `Open the editor (specified by EDITOR environment variable) in the session's working directory.

If EDITOR is not set, defaults to 'vim'. The selector may be an ID prefix or a description substring (case-insensitive).

Examples:
  jin session edit my-session
  EDITOR=code jin session edit my-session`,
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
		workDir := sess.WorkDir

		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vim"
		}

		// Save terminal state (to restore after editor exits)
		oldState, err := term.GetState(int(os.Stdin.Fd()))
		if err != nil {
			oldState = nil
		}

		editorCmd := exec.Command(editor, ".")
		editorCmd.Dir = workDir
		editorCmd.Stdin = os.Stdin
		editorCmd.Stdout = os.Stdout
		editorCmd.Stderr = os.Stderr

		runErr := editorCmd.Run()

		// Restore terminal state
		if oldState != nil {
			_ = term.Restore(int(os.Stdin.Fd()), oldState)
		}

		return runErr
	},
}

func init() {
	sessionCmd.AddCommand(editCmd)
}
