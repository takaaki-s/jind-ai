package git

import (
	"fmt"
	"strings"
)

// DetectDefaultBranch returns the branch name that origin/HEAD points at
// (typically "main" or "master"). Depends on the local clone having a
// symbolic ref for origin/HEAD; if it does not, git prints an error and this
// returns an error so the caller can decide whether to fall back to a config
// value or fail.
func (c *Client) DetectDefaultBranch(repoDir string) (string, error) {
	output, err := c.r.Run(repoDir, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("git symbolic-ref: %s", strings.TrimSpace(string(output)))
	}
	raw := strings.TrimSpace(string(output))
	const prefix = "refs/remotes/origin/"
	if !strings.HasPrefix(raw, prefix) {
		return "", fmt.Errorf("unexpected symbolic-ref output: %q", raw)
	}
	branch := strings.TrimPrefix(raw, prefix)
	if branch == "" {
		return "", fmt.Errorf("empty branch name from symbolic-ref: %q", raw)
	}
	return branch, nil
}

// Fetch runs `git fetch <remote> <ref>` in repoDir. Failures surface with the
// combined output so callers can log or decide based on remote-side messages.
func (c *Client) Fetch(repoDir, remote, ref string) error {
	output, err := c.r.Run(repoDir, "fetch", remote, ref)
	if err != nil {
		return fmt.Errorf("git fetch %s %s: %s", remote, ref, strings.TrimSpace(string(output)))
	}
	return nil
}

// BranchExists returns true iff a local branch with the given name exists.
// Any git error (including "no such branch") is treated as non-existence — the
// caller only needs a yes/no signal for collision detection.
func (c *Client) BranchExists(repoDir, branch string) bool {
	_, err := c.r.Run(repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// AddWorktree runs `git worktree add -b <branch> <worktreePath> <baseRef>`.
// baseRef is usually "origin/<default-branch>" so the new branch starts from
// a freshly fetched remote tip.
func (c *Client) AddWorktree(repoDir, branch, worktreePath, baseRef string) error {
	output, err := c.r.Run(repoDir, "worktree", "add", "-b", branch, worktreePath, baseRef)
	if err != nil {
		return fmt.Errorf("git worktree add: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

// DeleteBranch runs `git branch -D -- <branch>`. Used during rollback when a
// worktree add succeeded but a later step failed, leaving an orphan branch.
// The `--` separator ensures branch names starting with `-` are not parsed
// as flags.
func (c *Client) DeleteBranch(repoDir, branch string) error {
	output, err := c.r.Run(repoDir, "branch", "-D", "--", branch)
	if err != nil {
		return fmt.Errorf("git branch -D %s: %s", branch, strings.TrimSpace(string(output)))
	}
	return nil
}

// PruneWorktrees runs `git worktree prune` in repoDir, clearing metadata for
// worktree registrations whose directories have been removed out-of-band.
// Callers use this before collision detection so orphan `.git/worktrees/<name>/`
// entries don't cause spurious "already registered" failures.
func (c *Client) PruneWorktrees(repoDir string) error {
	output, err := c.r.Run(repoDir, "worktree", "prune")
	if err != nil {
		return fmt.Errorf("git worktree prune: %s", strings.TrimSpace(string(output)))
	}
	return nil
}
