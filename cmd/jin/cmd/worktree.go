package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/worktreehook"
)

var worktreeCmd = &cobra.Command{
	Use:   "worktree",
	Short: "Manage worktree post-create hook allowlist",
	Long: `Trust, revoke, and inspect the .jin/worktree-post-create.sh allowlist.

The hook is executed after 'jin session new --worktree' creates a git worktree.
For security, each repository must be trusted (via 'jin worktree allow') before
its hook script will run. Editing the script requires re-allowing.`,
}

var worktreeAllowCmd = &cobra.Command{
	Use:   "allow [path]",
	Short: "Trust the .jin/worktree-post-create.sh of a repository",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runWorktreeAllow,
}

var worktreeRevokeCmd = &cobra.Command{
	Use:   "revoke [path]",
	Short: "Revoke the trust of a repository",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runWorktreeRevoke,
}

var worktreeStatusCmd = &cobra.Command{
	Use:   "status [path]",
	Short: "Show the allow status of a repository",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runWorktreeStatus,
}

var worktreeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all trusted repositories",
	Args:  cobra.NoArgs,
	RunE:  runWorktreeList,
}

func init() {
	rootCmd.AddCommand(worktreeCmd)
	worktreeCmd.AddCommand(worktreeAllowCmd, worktreeRevokeCmd, worktreeStatusCmd, worktreeListCmd)
	worktreeAllowCmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
}

func resolveRepoRoot(args []string) (string, error) {
	var raw string
	if len(args) == 0 {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get cwd: %w", err)
		}
		raw = wd
	} else {
		raw = args[0]
	}
	return filepath.Abs(raw)
}

func scriptPathFor(repoRoot string) string {
	return filepath.Join(repoRoot, ".jin", "worktree-post-create.sh")
}

func loadAllowlist() (*worktreehook.Allowlist, error) {
	return worktreehook.LoadAllowlist(getStateDir())
}

func runWorktreeAllow(cmd *cobra.Command, args []string) error {
	repoRoot, err := resolveRepoRoot(args)
	if err != nil {
		return err
	}
	scriptPath := scriptPathFor(repoRoot)

	content, err := os.ReadFile(scriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no hook script at %s", scriptPath)
		}
		return fmt.Errorf("read hook script: %w", err)
	}

	sha, err := worktreehook.ComputeSHA256(scriptPath)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		fmt.Fprintln(out, "=== .jin/worktree-post-create.sh ===")
		if _, err := out.Write(content); err != nil {
			return err
		}
		if len(content) == 0 || content[len(content)-1] != '\n' {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "=== SHA256: %s ===\n\n", sha)
		fmt.Fprint(out, "Trust this hook? [y/N]: ")

		reader := bufio.NewReader(cmd.InOrStdin())
		line, _ := reader.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	al, err := loadAllowlist()
	if err != nil {
		return err
	}
	if err := al.Allow(repoRoot, sha); err != nil {
		return err
	}
	fmt.Fprintf(out, "Allowed: %s\n", repoRoot)
	return nil
}

func runWorktreeRevoke(cmd *cobra.Command, args []string) error {
	repoRoot, err := resolveRepoRoot(args)
	if err != nil {
		return err
	}
	al, err := loadAllowlist()
	if err != nil {
		return err
	}
	if err := al.Revoke(repoRoot); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Revoked: %s\n", repoRoot)
	return nil
}

func runWorktreeStatus(cmd *cobra.Command, args []string) error {
	repoRoot, err := resolveRepoRoot(args)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	scriptPath := scriptPathFor(repoRoot)
	if _, err := os.Stat(scriptPath); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(out, "No hook script.")
			return nil
		}
		return fmt.Errorf("stat hook script: %w", err)
	}

	al, err := loadAllowlist()
	if err != nil {
		return err
	}
	entry, ok := al.Get(repoRoot)
	if !ok {
		fmt.Fprintln(out, "Not allowed. Run `jin worktree allow` to trust.")
		return nil
	}

	sha, err := worktreehook.ComputeSHA256(scriptPath)
	if err != nil {
		return err
	}
	if sha != entry.SHA256 {
		fmt.Fprintf(out, "Script changed since %s. Run `jin worktree allow` to re-trust.\n", entry.AllowedAt.Format(time.RFC3339))
		return nil
	}
	fmt.Fprintf(out, "Allowed (since %s).\n", entry.AllowedAt.Format(time.RFC3339))
	return nil
}

func runWorktreeList(cmd *cobra.Command, args []string) error {
	al, err := loadAllowlist()
	if err != nil {
		return err
	}
	entries := al.All()
	out := cmd.OutOrStdout()
	if len(entries) == 0 {
		fmt.Fprintln(out, "No repositories in allowlist.")
		return nil
	}

	keys := make([]string, 0, len(entries))
	for p := range entries {
		keys = append(keys, p)
	}
	sort.Strings(keys)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tALLOWED_AT")
	for _, p := range keys {
		fmt.Fprintf(tw, "%s\t%s\n", p, entries[p].AllowedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}
