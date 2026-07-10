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
	Use:     "jin",
	Short:   "LLM session manager for Claude Code",
	Long:    `A CLI tool to manage multiple Claude Code sessions with attach/detach support.`,
	Version: version.Version,
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
