package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	// Register every known agent adapter with the process-global registry.
	// The blank import must fire before daemon.NewServer's agent.Lookup call
	// path executes; root.go is the earliest deterministic entry point in
	// the CLI.
	_ "github.com/takaaki-s/jind-ai/internal/agent/register"
	"github.com/takaaki-s/jind-ai/internal/exitcode"
	"github.com/takaaki-s/jind-ai/internal/version"
)

var jsonOutput bool

var rootCmd = &cobra.Command{
	Use:   "jin",
	Short: "Manage multiple coding-agent sessions in tmux",
	Long: `A CLI/TUI for managing multiple interactive coding-agent sessions in tmux.

Claude Code, Codex, and opencode can run side by side with attach/detach support.

Driving jin from an agent? Run 'jin docs list'.`,
	Version: version.Version,
	// Runtime failures must not print the usage block. A command that reaches
	// its RunE body was invoked correctly, so "no session matches selector"
	// followed by thirteen lines of flag documentation buries the one line
	// worth reading — fatally so for callers that pipe us into `tail`, which
	// is exactly how jin's own orchestration reads command output.
	//
	// Setting SilenceUsage on rootCmd directly would also silence the usage
	// errors, where the block is the whole point. Cobra runs a command as
	// ParseFlags -> ValidateArgs -> PersistentPreRunE -> RunE, so flipping the
	// flag here splits the two cases on exactly the right seam: unknown flags
	// and wrong argument counts still print usage, anything RunE returns does
	// not.
	//
	// Every subcommand inherits this. Cobra walks up to the nearest
	// PersistentPreRunE and runs only that one, so a subcommand defining its
	// own would shadow this and get the old behaviour back; none do today.
	//
	// One usage error does land on the silenced side: ValidateRequiredFlags
	// runs after this hook, so a missing required flag prints no usage. Only
	// `pane close --name` is marked required, and cobra's own message there
	// ("required flag(s) \"name\" not set") names the flag, so the block adds
	// little. Cobra offers no hook between that check and RunE — restoring it
	// would mean setting SilenceUsage at the top of all thirty RunE bodies.
	//
	// SilenceErrors is deliberately left off: Execute below prints nothing for
	// the non-JSON path and relies on cobra to write "Error: ...".
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		cmd.SilenceUsage = true
		return nil
	},
}

// usageError reports a malformed invocation from inside a RunE body, undoing
// the usage suppression that rootCmd's PersistentPreRunE applies to runtime
// failures.
//
// Cobra's Args validators catch most arity mistakes before RunE is reached,
// but a few commands can only decide arity themselves — send accepts a bare
// selector for shell completion's sake and rejects it once parsed, and the
// pane commands trade a positional selector against --here. Those are the
// user getting the command line wrong, not the command failing, so they want
// the usage block just as much as "unknown flag" does.
func usageError(cmd *cobra.Command, format string, a ...any) error {
	cmd.SilenceUsage = false
	return fmt.Errorf(format, a...)
}

// Execute runs the root command
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		code := exitcode.GeneralError
		var exitErr *exitcode.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.Code
		}
		if jsonOutput {
			printJSONError(err, code)
		}
		os.Exit(code)
	}
}

// printJSONError outputs a structured JSON error to stdout.
func printJSONError(err error, code int) {
	result := struct {
		Success  bool   `json:"success"`
		Error    string `json:"error"`
		ExitCode int    `json:"exit_code"`
	}{
		Success:  false,
		Error:    err.Error(),
		ExitCode: code,
	}
	// All fields are bool/string/int — json.Marshal cannot fail.
	data, _ := json.Marshal(result)
	fmt.Fprintln(os.Stdout, string(data))
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	rootCmd.SetVersionTemplate("jin " + version.Full() + "\n")
}
