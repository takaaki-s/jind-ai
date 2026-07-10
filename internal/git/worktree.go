package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IsGitRoot returns true if the path contains a .git file or directory,
// indicating it is a git repository root or worktree root.
// Used to guard WorkDir updates: only paths that are project/worktree roots
// should update the persisted WorkDir. Subdirectory navigation (e.g., into
// .claude/workdir/) must not drift WorkDir away from the project root.
func IsGitRoot(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// IsGitWorktreeDir returns true when the directory at path is a git worktree
// (its .git entry is a regular file that points at the main repo). Returns
// false for the main repo (.git is a directory) or non-git directories.
func IsGitWorktreeDir(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Lstat(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular()
}

// IsClaudeWorktreePath returns true if the path is inside Claude Code's
// worktree directory (.claude/worktrees/). These paths should not overwrite
// the persisted WorkDir because the worktree may be deleted after
// ExitWorktree, making the session unable to restart.
//
// Note: this is specific to Claude Code's own worktree convention. It is
// unrelated to jind-ai's configurable worktree base directory.
func IsClaudeWorktreePath(path string) bool {
	return strings.Contains(path, "/.claude/worktrees/")
}

// dirtyOutputMarkers are substrings that indicate `git worktree remove`
// refused to run because the worktree has local changes. The exact wording
// varies by git version, so we match multiple known variants.
var dirtyOutputMarkers = []string{
	"modified or untracked",
	"is dirty",
	"contains untracked files",
}

// outputIndicatesDirty reports whether git's combined output contains any of
// the known "worktree has local changes" markers.
func outputIndicatesDirty(output string) bool {
	for _, marker := range dirtyOutputMarkers {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// ResolveWorktreeDir picks the best path to use when removing a session's
// git worktree. Prefers currentWorkDir (reflects most recent CWD) when it is
// a worktree, then workDir. Falls back to either even when neither is a
// worktree so RemoveWorktree can return ErrNotWorktree against the caller's
// intent rather than silently doing nothing.
func ResolveWorktreeDir(currentWorkDir, workDir string) string {
	if IsGitWorktreeDir(currentWorkDir) {
		return currentWorkDir
	}
	if IsGitWorktreeDir(workDir) {
		return workDir
	}
	if currentWorkDir != "" {
		return currentWorkDir
	}
	return workDir
}

// RemoveWorktree removes a git worktree at workDir.
// Returns ErrDirty if the worktree has uncommitted changes and force is
// false. Returns ErrNotWorktree if workDir is not a git worktree. A
// non-existent directory is treated as already-removed and returns nil so
// removal is idempotent.
func (c *Client) RemoveWorktree(workDir string, force bool) error {
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		return nil
	}

	gitPath := filepath.Join(workDir, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil || !fi.Mode().IsRegular() {
		return ErrNotWorktree
	}

	// .git file contents: "gitdir: /path/to/main/.git/worktrees/<name>"
	content, err := os.ReadFile(gitPath)
	if err != nil {
		return fmt.Errorf("reading .git file: %w", err)
	}
	raw := strings.TrimSpace(string(content))
	if !strings.HasPrefix(raw, "gitdir: ") {
		return ErrNotWorktree
	}
	gitdir := strings.TrimPrefix(raw, "gitdir: ")

	// .git/worktrees/<name> → .git → repo root
	mainGitDir := filepath.Dir(filepath.Dir(gitdir))
	mainRepoDir := filepath.Dir(mainGitDir)

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	// `--` separator so paths starting with `-` are not misparsed as flags.
	args = append(args, "--", workDir)

	output, err := c.r.Run(mainRepoDir, args...)
	if err != nil {
		outStr := string(output)
		if !force && outputIndicatesDirty(outStr) {
			return ErrDirty
		}
		return fmt.Errorf("git worktree remove: %s", strings.TrimSpace(outStr))
	}
	return nil
}
