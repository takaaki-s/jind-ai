package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/honjin/internal/daemon"
	"github.com/takaaki-s/honjin/internal/paths"
	"github.com/takaaki-s/honjin/internal/session"
)

var newCmd = &cobra.Command{
	Use:   "new",
	Short: "Create a new Claude Code session",
	Long: `Create a new Claude Code session and start it in background.

Examples:
  jin session new --workdir ~/projects/myapp
  jin session new --workdir . --name myapp
  jin session new --workdir ~/projects/myapp --host ec2
  jin session new --workdir . --fleet backend

For interactive session creation, use 'jin ui' (TUI).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		workDir, _ := cmd.Flags().GetString("workdir")
		name, _ := cmd.Flags().GetString("name")
		hostID, _ := cmd.Flags().GetString("host")
		fleet, _ := cmd.Flags().GetString("fleet")
		noStart, _ := cmd.Flags().GetBool("no-start")
		worktree, _ := cmd.Flags().GetBool("worktree")
		worktreeName, _ := cmd.Flags().GetString("worktree-name")
		worktreeBranch, _ := cmd.Flags().GetString("worktree-branch")
		worktreeBase, _ := cmd.Flags().GetString("worktree-base")

		if worktree && hostID != "" && hostID != "local" {
			return fmt.Errorf("--worktree is not supported for remote hosts yet")
		}

		// Default WorkDir: current directory
		if workDir == "" {
			var err error
			workDir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get current directory: %w", err)
			}
		}

		// Check WorkDir existence (skip for remote hosts)
		if hostID == "" || hostID == "local" {
			if info, err := os.Stat(workDir); err != nil {
				return fmt.Errorf("work directory does not exist: %s", workDir)
			} else if !info.IsDir() {
				return fmt.Errorf("not a directory: %s", workDir)
			}
		}

		// For remote hosts, convert local home prefix to ~
		// (because the shell expands ~/path to /Users/xxx/path)
		if hostID != "" && hostID != "local" {
			if home, err := os.UserHomeDir(); err == nil {
				if workDir == home {
					workDir = "~"
				} else if len(workDir) > len(home) && workDir[:len(home)+1] == home+"/" {
					workDir = "~/" + workDir[len(home)+1:]
				}
			}
		}

		client := daemon.NewClient(getSocketPath())
		s, err := client.NewWithOptions(daemon.NewOptions{
			Name:           name,
			WorkDir:        workDir,
			Start:          !noStart,
			HostID:         hostID,
			Fleet:          fleet,
			Worktree:       worktree,
			WorktreeName:   worktreeName,
			WorktreeBranch: worktreeBranch,
			WorktreeBase:   worktreeBase,
		})
		if err != nil {
			return err
		}

		if jsonOutput {
			return renderNewSessionJSON(os.Stdout, s)
		}

		fmt.Printf("Created session: %s (%s)\n", s.Name, s.ID)
		fmt.Printf("Working directory: %s\n", s.WorkDir)
		fmt.Printf("Status: %s\n", s.Status)
		fmt.Printf("\nTo attach: jin session attach %s\n", s.ID)
		return nil
	},
}

func init() {
	sessionCmd.AddCommand(newCmd)

	newCmd.Flags().StringP("workdir", "d", "", "Working directory (default: current directory)")
	newCmd.Flags().StringP("name", "n", "", "Session name (default: directory basename)")
	newCmd.Flags().StringP("host", "H", "", "Target host (default: local)")
	newCmd.Flags().StringP("fleet", "f", "", "Fleet name for session grouping (default: \"default\")")
	newCmd.Flags().Bool("no-start", false, "Don't start the session immediately")
	newCmd.Flags().Bool("worktree", false, "Create a git worktree for this session (from the repo's default branch)")
	newCmd.Flags().String("worktree-name", "", "Override the auto-generated worktree name (default: jin-<8hex>)")
	newCmd.Flags().String("worktree-branch", "", "Override the auto-generated branch name (default: <branch_prefix>jin-<8hex>)")
	newCmd.Flags().String("worktree-base", "", "Override the base branch (default: origin/HEAD)")
}

func renderNewSessionJSON(w io.Writer, info *session.Info) error {
	return writeJSON(w, info)
}

func getDataDir() string {
	return paths.Sessions()
}
