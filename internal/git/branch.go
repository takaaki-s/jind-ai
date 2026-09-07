package git

import (
	"fmt"
	"strings"
)

// ResolveCommit resolves ref to a full commit object ID. The ^{commit} peel
// rejects tags or objects that do not ultimately name a commit, while
// --end-of-options prevents a user-controlled ref from being parsed as a git
// option. No fetch is performed: callers only resolve repository state they
// already have.
func (c *Client) ResolveCommit(repoDir, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("commit ref is required")
	}
	output, err := c.r.Run(repoDir,
		"rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git rev-parse %q: %s", ref, detail)
	}

	oid := strings.TrimSpace(string(output))
	if !isFullObjectID(oid) {
		return "", fmt.Errorf("git rev-parse %q returned non-full object ID %q", ref, oid)
	}
	return strings.ToLower(oid), nil
}

// Git repositories may use SHA-1 (40 hexadecimal characters) or SHA-256 (64).
func isFullObjectID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, r := range oid {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

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

// BranchExists returns true iff a local branch with the given name exists.
// Any git error (including "no such branch") is treated as non-existence — the
// caller only needs a yes/no signal for collision detection.
func (c *Client) BranchExists(repoDir, branch string) bool {
	_, err := c.r.Run(repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// AddWorktree runs `git worktree add -b <branch> <worktreePath> <baseRef>`.
// Callers pass the full commit OID they resolved from the requested base, so
// the checkout cannot race with a moving branch ref.
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
