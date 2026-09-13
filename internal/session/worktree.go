package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxWorktreeNameAttempts caps the suffixes (-2, -3, ...) tried during
// collision resolution. Beyond ten something is structurally wrong and the
// caller should hear about it instead of silently spinning.
const maxWorktreeNameAttempts = 10

// WorktreePlacement is the resolved worktree name, branch, and filesystem
// path chosen for a new session.
type WorktreePlacement struct {
	Name   string
	Branch string
	Path   string
}

// DefaultWorktreeIdentity returns the deterministic name and branch used for
// a session before filesystem placement is attempted. Path remains empty.
// Prompt orchestration persists this identity before provisioning so a retry
// cannot silently choose a second branch.
func DefaultWorktreeIdentity(sessionID, branchPrefix string) WorktreePlacement {
	name := deriveWorktreeName(sessionID, "")
	return WorktreePlacement{Name: name, Branch: deriveBranchName(name, branchPrefix, "")}
}

// deriveWorktreeName picks the worktree name. An explicit override wins;
// otherwise the name is "jin-<first 8 hex of sessionID>".
func deriveWorktreeName(sessionID, override string) string {
	if override != "" {
		return override
	}
	if len(sessionID) < 8 {
		return "jin-" + sessionID
	}
	return "jin-" + sessionID[:8]
}

// deriveBranchName picks the branch name. An explicit override wins; otherwise
// it is prefix + worktreeName with the auto "jin-" prefix stripped, so a
// "jin-abcd1234" worktree pairs with "jin/abcd1234" and not "jin/jin-abcd1234".
func deriveBranchName(worktreeName, prefix, override string) string {
	if override != "" {
		return override
	}
	return prefix + strings.TrimPrefix(worktreeName, "jin-")
}

// worktreeTemplate returns the placement template for a new worktree: the
// user's worktree.base_dir when set, otherwise worktrees/{name} under the state
// dir the Manager was built over.
//
// The state dir is threaded in rather than read from paths.State(), because a
// Manager honours the one it was given for every other artifact it writes.
// Reading the global instead left worktrees outside that set, silently, for
// every Manager not built over the process's own state dir. An empty stateDir
// yields a relative template, which expandBaseDir rejects — the intended
// failure, the alternative being a worktree in whatever the cwd happens to be.
func worktreeTemplate(cfgBaseDir, stateDir string) string {
	if cfgBaseDir != "" {
		return cfgBaseDir
	}
	return filepath.Join(stateDir, "worktrees", "{name}")
}

// expandBaseDir expands {name}, {repo}, and ${ENV} in a base_dir template.
// Returns an absolute path, or an error if the template contains an unknown
// {xxx} variable or does not resolve to an absolute path. worktreeTemplate owns
// the default, so an empty template arriving here is a caller bug.
func expandBaseDir(template, worktreeName, repoBasename string) (string, error) {
	expanded := os.ExpandEnv(template)
	replaced := strings.ReplaceAll(expanded, "{name}", worktreeName)
	replaced = strings.ReplaceAll(replaced, "{repo}", repoBasename)

	if idx := strings.Index(replaced, "{"); idx >= 0 {
		if end := strings.Index(replaced[idx:], "}"); end > 0 {
			return "", fmt.Errorf("unknown template variable %q in worktree.base_dir",
				replaced[idx:idx+end+1])
		}
	}

	if !filepath.IsAbs(replaced) {
		return "", fmt.Errorf("worktree.base_dir must resolve to an absolute path, got %q", replaced)
	}
	return replaced, nil
}

// findAvailableWorktreeName tries baseName, baseName-2, baseName-3, ... up to
// maxWorktreeNameAttempts times. collides is called once per candidate and
// reports whether the worktree directory or the branch would clash.
func findAvailableWorktreeName(baseName string, collides func(candidate string) bool) (string, error) {
	for i := 1; i <= maxWorktreeNameAttempts; i++ {
		candidate := baseName
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", baseName, i)
		}
		if !collides(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf(
		"could not find an available worktree name after %d attempts (base: %q)",
		maxWorktreeNameAttempts, baseName,
	)
}
