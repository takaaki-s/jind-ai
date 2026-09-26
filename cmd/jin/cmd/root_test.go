package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newUsageProbe builds a two-level command tree wired with the real rootCmd's
// PersistentPreRunE. Exercising the actual hook (rather than a copy of it)
// means deleting it from rootCmd fails these tests. A throwaway tree is used
// instead of rootCmd itself because Execute mutates SilenceUsage on the
// command it runs, and the package's other tests share the global.
func newUsageProbe(args cobra.PositionalArgs, runErr error) (*cobra.Command, *strings.Builder) {
	var out strings.Builder

	root := &cobra.Command{
		Use:               "jin",
		PersistentPreRunE: rootCmd.PersistentPreRunE,
	}
	sub := &cobra.Command{
		Use:  "send",
		Args: args,
		RunE: func(*cobra.Command, []string) error { return runErr },
	}
	root.AddCommand(sub)
	root.SetOut(&out)
	root.SetErr(&out)

	return root, &out
}

// A failure returned from RunE means the invocation itself was well formed, so
// the usage block is noise that pushes the real message out of a `| tail`.
func TestRunEErrorOmitsUsage(t *testing.T) {
	root, out := newUsageProbe(cobra.MinimumNArgs(1), errors.New("no session matches selector: abc"))
	root.SetArgs([]string{"send", "abc"})

	if err := root.Execute(); err == nil {
		t.Fatal("Execute() = nil, want the RunE error")
	}

	got := out.String()
	if strings.Contains(got, "Usage:") {
		t.Errorf("runtime error printed the usage block:\n%s", got)
	}
	if !strings.Contains(got, "no session matches selector: abc") {
		t.Errorf("runtime error message missing from output:\n%s", got)
	}
}

// The counterpart: a genuinely malformed invocation still gets the usage
// block, which is the case SilenceUsage on rootCmd would have broken.
func TestUsageErrorsKeepUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "too few args", args: []string{"send"}},
		{name: "unknown flag", args: []string{"send", "abc", "--nope"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, out := newUsageProbe(cobra.MinimumNArgs(1), nil)
			root.SetArgs(tt.args)

			if err := root.Execute(); err == nil {
				t.Fatal("Execute() = nil, want a usage error")
			}

			if got := out.String(); !strings.Contains(got, "Usage:") {
				t.Errorf("usage error dropped the usage block:\n%s", got)
			}
		})
	}
}

// Commands that check arity themselves report through usageError, which has
// to put the usage block back that PersistentPreRunE took away.
func TestUsageErrorRestoresUsage(t *testing.T) {
	var out strings.Builder

	root := &cobra.Command{
		Use:               "jin",
		PersistentPreRunE: rootCmd.PersistentPreRunE,
	}
	sub := &cobra.Command{
		Use:  "send",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return usageError(cmd, "prompt is required")
		},
	}
	root.AddCommand(sub)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"send", "abc"})

	if err := root.Execute(); err == nil {
		t.Fatal("Execute() = nil, want the usage error")
	}

	got := out.String()
	if !strings.Contains(got, "prompt is required") {
		t.Errorf("usageError message missing from output:\n%s", got)
	}
	if !strings.Contains(got, "Usage:") {
		t.Errorf("usageError did not restore the usage block:\n%s", got)
	}
}

// SilenceErrors must stay off: Execute prints nothing itself on the non-JSON
// path, so turning it on anywhere above a command would swallow the message
// entirely rather than merely trimming the usage block.
func TestRootDoesNotSilenceErrors(t *testing.T) {
	if rootCmd.SilenceErrors {
		t.Error("rootCmd.SilenceErrors = true; Execute relies on cobra to print the error")
	}
	if rootCmd.SilenceUsage {
		t.Error("rootCmd.SilenceUsage = true; that silences usage errors too, use the PersistentPreRunE hook")
	}
}

func TestRootHelpDescribesTheMultiAgentProduct(t *testing.T) {
	help := rootCmd.Short + "\n" + rootCmd.Long
	for _, want := range []string{"coding-agent sessions", "Claude Code", "Codex", "opencode"} {
		if !strings.Contains(help, want) {
			t.Errorf("root help does not mention %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "multiple Claude Code sessions") {
		t.Errorf("root help still presents jin as Claude Code-only:\n%s", help)
	}
}

func TestGenericSessionCommandsDoNotPresentAsClaudeOnly(t *testing.T) {
	for _, command := range []*cobra.Command{sessionCmd, attachCmd, killCmd, deleteCmd, infoCmd, tuiCmd} {
		help := command.Short + "\n" + command.Long
		if strings.Contains(help, "Claude Code") {
			t.Errorf("%s help presents a generic command as Claude Code-only:\n%s", command.CommandPath(), help)
		}
	}
}
