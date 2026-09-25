package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/paths"
	"github.com/takaaki-s/jind-ai/internal/session"
)

var newCmd = &cobra.Command{
	Use:   "new",
	Short: "Create a new agent session",
	Long: `Create a new agent session and start it in background. Defaults to
Claude Code; use --agent to select a different adapter (once more are
registered).

Examples:
  jin session new --workdir ~/projects/myapp
  jin session new --workdir . --description myapp
  jin session new --workdir . --fleet backend
  jin session new --workdir . --agent claude
  jin session new --workdir . --model opus

--model is handed to the agent's CLI verbatim, so it is spelled the way that
CLI spells it: an alias or full name for Claude Code, provider/model for
opencode. It sticks to the session and is replayed on every resume.

For interactive session creation, use 'jin ui' (TUI).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := newSessionOptions(cmd)
		if err != nil {
			return err
		}

		client := daemon.NewClient(getSocketPath())
		s, warning, err := client.NewWithOptions(opts)
		if err != nil {
			return err
		}

		if jsonOutput {
			return renderNewSessionJSON(os.Stdout, s)
		}

		// Surface non-fatal warnings (e.g. hook skipped because the repo is
		// not allowlisted) before the "Created session" line so users notice
		// them in normal output rather than only in JIN_DEBUG=1 logs.
		if warning != "" {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
		}

		fmt.Printf("Created session: %s (%s)\n", s.Description, s.ID)
		fmt.Printf("Working directory: %s\n", s.WorkDir)
		fmt.Printf("Status: %s\n", s.Status)
		fmt.Printf("\nTo attach: jin session attach %s\n", s.ID)
		return nil
	},
}

// newSessionOptions reads the create flags off cmd and resolves the working
// directory, producing exactly what the daemon is sent.
//
// Split out of RunE so the mapping is reachable at all: RunE needs a running
// daemon, and a flag that parses correctly but never reaches NewOptions is
// invisible from a parse test — measured, dropping one left the whole suite
// green. Every flag rides the same hop, so they are all covered together.
func newSessionOptions(cmd *cobra.Command) (daemon.NewOptions, error) {
	workDir, _ := cmd.Flags().GetString("workdir")
	description, _ := cmd.Flags().GetString("description")
	fleet, _ := cmd.Flags().GetString("fleet")
	agentKind, _ := cmd.Flags().GetString("agent")
	model, _ := cmd.Flags().GetString("model")
	noStart, _ := cmd.Flags().GetBool("no-start")
	worktree, _ := cmd.Flags().GetBool("worktree")
	worktreeName, _ := cmd.Flags().GetString("worktree-name")
	worktreeBranch, _ := cmd.Flags().GetString("worktree-branch")
	worktreeBase, _ := cmd.Flags().GetString("worktree-base")
	noHook, _ := cmd.Flags().GetBool("no-hook")

	// Default WorkDir: current directory
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return daemon.NewOptions{}, fmt.Errorf("failed to get current directory: %w", err)
		}
	}
	resolvedWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return daemon.NewOptions{}, fmt.Errorf("failed to resolve work directory %q: %w", workDir, err)
	}
	workDir = resolvedWorkDir

	if info, err := os.Stat(workDir); err != nil {
		return daemon.NewOptions{}, fmt.Errorf("work directory does not exist: %s", workDir)
	} else if !info.IsDir() {
		return daemon.NewOptions{}, fmt.Errorf("not a directory: %s", workDir)
	}

	return daemon.NewOptions{
		Description:    description,
		WorkDir:        workDir,
		Start:          !noStart,
		Fleet:          fleet,
		AgentKind:      agentKind,
		Model:          model,
		Worktree:       worktree,
		WorktreeName:   worktreeName,
		WorktreeBranch: worktreeBranch,
		WorktreeBase:   worktreeBase,
		NoHook:         noHook,
	}, nil
}

func init() {
	sessionCmd.AddCommand(newCmd)

	newCmd.Flags().String("workdir", "", "Working directory (default: current directory)")
	newCmd.Flags().StringP("description", "d", "", "Human-readable session description (default: directory basename)")
	newCmd.Flags().StringP("fleet", "f", "", "Fleet name for session grouping (default: \"default\")")
	newCmd.Flags().String("agent", "", "Agent adapter kind (default: config's default_agent, fallback \"claude\")")
	newCmd.Flags().String("model", "", "Model for this session, spelled as the agent's own CLI takes it (default: the agent's default)")
	newCmd.Flags().Bool("no-start", false, "Don't start the session immediately")
	newCmd.Flags().Bool("worktree", false, "Create a git worktree for this session (from the repo's default branch)")
	newCmd.Flags().String("worktree-name", "", "Override the auto-generated worktree name (default: jin-<8hex>)")
	newCmd.Flags().String("worktree-branch", "", "Override the auto-generated branch name (default: <branch_prefix>jin-<8hex>)")
	newCmd.Flags().String("worktree-base", "", "Override the base branch (default: origin/HEAD)")
	newCmd.Flags().Bool("no-hook", false, "Skip the .jin/worktree-post-create.sh hook (worktree only)")
}

func renderNewSessionJSON(w io.Writer, info *session.Info) error {
	return writeJSON(w, info)
}

func getDataDir() string {
	return paths.Sessions()
}
