package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CleanupWorktree is the local identity core must prove before a review
// cleanup may remove anything. RepositoryPath is the primary checkout that
// owns the linked worktree; WorktreePath is canonical and absolute.
type CleanupWorktree struct {
	RepositoryPath  string
	WorktreePath    string
	Branch          string
	HeadCommit      string
	UnpushedCommits int
}

// InspectCleanupWorktree verifies that worktreePath is an exact linked
// worktree owned by a primary repository and returns the branch identity that
// would be deleted. verifiedHead is the provider-observed PR head; commits
// after it are local-only work and therefore block cleanup.
func (c *Client) InspectCleanupWorktree(ctx context.Context, worktreePath, verifiedHead string) (CleanupWorktree, error) {
	var out CleanupWorktree
	if !isFullObjectID(verifiedHead) {
		return out, fmt.Errorf("invalid verified merge head %q", verifiedHead)
	}
	if worktreePath == "" || !filepath.IsAbs(worktreePath) || filepath.Clean(worktreePath) != worktreePath || isFilesystemRoot(worktreePath) {
		return out, fmt.Errorf("cleanup worktree path must be an exact absolute non-root path")
	}
	fi, err := os.Lstat(filepath.Join(worktreePath, ".git"))
	if err != nil || !fi.Mode().IsRegular() {
		return out, ErrNotWorktree
	}

	topRaw, err := c.runReviewCommand(ctx, worktreePath, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return out, fmt.Errorf("resolve cleanup worktree root: %w", err)
	}
	top := filepath.Clean(strings.TrimSpace(string(topRaw)))
	if !samePath(top, worktreePath) {
		return out, fmt.Errorf("cleanup worktree ownership mismatch: requested %q, git reports %q", worktreePath, top)
	}

	commonRaw, err := c.runReviewCommand(ctx, worktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return out, fmt.Errorf("resolve cleanup repository: %w", err)
	}
	commonDir := filepath.Clean(strings.TrimSpace(string(commonRaw)))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Clean(filepath.Join(worktreePath, commonDir))
	}
	if filepath.Base(commonDir) != ".git" || isFilesystemRoot(filepath.Dir(commonDir)) {
		return out, fmt.Errorf("cleanup repository ownership is unresolved: %q", commonDir)
	}
	if fi, err := os.Stat(commonDir); err != nil || !fi.IsDir() {
		return out, fmt.Errorf("cleanup repository metadata is unavailable: %q", commonDir)
	}

	gitDir, err := linkedGitDir(worktreePath)
	if err != nil {
		return out, err
	}
	rel, err := filepath.Rel(commonDir, gitDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		!strings.HasPrefix(rel, "worktrees"+string(filepath.Separator)) {
		return out, fmt.Errorf("cleanup worktree is not owned by repository %q", filepath.Dir(commonDir))
	}

	branchRaw, err := c.runReviewCommand(ctx, worktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(string(branchRaw)) == "" {
		return out, fmt.Errorf("cleanup requires an attached local branch")
	}
	branch := strings.TrimSpace(string(branchRaw))
	head, err := c.ResolveCommit(worktreePath, "HEAD")
	if err != nil {
		return out, err
	}
	repoDir := filepath.Dir(commonDir)
	branchHead, err := c.ResolveCommit(repoDir, "refs/heads/"+branch)
	if err != nil || branchHead != head {
		return out, fmt.Errorf("cleanup branch ownership mismatch for %q", branch)
	}

	commitsRaw, err := c.runReviewCommand(ctx, worktreePath, "rev-list", "--count", strings.ToLower(verifiedHead)+"..HEAD", "--")
	if err != nil {
		return out, fmt.Errorf("count commits after verified merge head: %w", err)
	}
	unpushed, err := strconv.Atoi(strings.TrimSpace(string(commitsRaw)))
	if err != nil || unpushed < 0 {
		return out, fmt.Errorf("invalid unpushed commit count %q", strings.TrimSpace(string(commitsRaw)))
	}

	out = CleanupWorktree{
		RepositoryPath: repoDir, WorktreePath: top, Branch: branch,
		HeadCommit: head, UnpushedCommits: unpushed,
	}
	return out, nil
}

func linkedGitDir(worktreePath string) (string, error) {
	data, err := os.ReadFile(filepath.Join(worktreePath, ".git"))
	if err != nil {
		return "", fmt.Errorf("read cleanup worktree ownership: %w", err)
	}
	const prefix = "gitdir: "
	raw := strings.TrimSpace(string(data))
	if !strings.HasPrefix(raw, prefix) {
		return "", ErrNotWorktree
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(raw, prefix))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreePath, gitDir)
	}
	return filepath.Clean(gitDir), nil
}

func samePath(a, b string) bool {
	aEval, aErr := filepath.EvalSymlinks(a)
	bEval, bErr := filepath.EvalSymlinks(b)
	if aErr == nil && bErr == nil {
		return filepath.Clean(aEval) == filepath.Clean(bEval)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func isFilesystemRoot(path string) bool {
	clean := filepath.Clean(path)
	return filepath.Dir(clean) == clean
}

// DeleteExactBranch removes branch only when it still names expectedHead.
// The hard delete is deliberate: squash/rebase merges do not make the source
// branch an ancestor of the target branch, while the verified provider merge
// receipt supplies the authority. The exact-head compare prevents deleting a
// branch that was reused after the worktree was removed.
func (c *Client) DeleteExactBranch(repoDir, branch, expectedHead string) error {
	if branch == "" || !isFullObjectID(expectedHead) {
		return fmt.Errorf("exact branch and head are required")
	}
	head, err := c.ResolveCommit(repoDir, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if head != strings.ToLower(expectedHead) {
		return fmt.Errorf("branch %q moved from %s to %s", branch, expectedHead, head)
	}
	return c.DeleteBranch(repoDir, branch)
}

// LocalBranchHead distinguishes an already-deleted branch from a git probe
// failure so cleanup retries may skip only the former.
func (c *Client) LocalBranchHead(repoDir, branch string) (string, bool, error) {
	if strings.TrimSpace(branch) == "" {
		return "", false, fmt.Errorf("local branch is required")
	}
	if !filepath.IsAbs(repoDir) || isFilesystemRoot(repoDir) {
		return "", false, fmt.Errorf("local branch repository must be an absolute non-root path")
	}
	if fi, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil || !fi.IsDir() {
		return "", false, fmt.Errorf("local branch repository metadata is unavailable: %q", repoDir)
	}
	out, err := c.r.Run(repoDir, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		if len(strings.TrimSpace(string(out))) == 0 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect local branch %q: %s", branch, strings.TrimSpace(string(out)))
	}
	head := strings.ToLower(strings.TrimSpace(string(out)))
	if !isFullObjectID(head) {
		return "", false, fmt.Errorf("local branch %q returned invalid object ID %q", branch, head)
	}
	return head, true, nil
}
