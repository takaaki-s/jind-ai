package session

import (
	"os"
	"path/filepath"
	"strings"
)

// maxRepoRootWalk caps the walk-up loop in findRepoRoot to avoid pathological
// deeply nested paths pinning CPU inside the create hot path.
const maxRepoRootWalk = 100

// GenerateBaselineDescription assembles a repo-derived label for a session.
//
// Format:
//   - main repo:     "<repo>[:<branch>][:<subpath>]"
//   - worktree:      "<main-repo>:<worktree-name>[:<branch>][:<subpath>]"
//   - non-git dir:   "<dir-basename>"
//   - empty input:   "session"
//
// The result is always non-empty.
//
// This function only depends on filepath + os.Lstat / os.ReadFile; it never
// invokes the git subprocess. That matters because CreateWithOptions calls
// this on the hot path, and shelling out to git would add tens of milliseconds
// per create.
//
// isWorktree and tmuxHint are accepted for signature stability across the
// three call sites documented in the F001/F004 review notes; the actual
// worktree detection is done by inspecting workDir on disk here so the three
// sites can pass isWorktree=false without silently disabling the branch.
func GenerateBaselineDescription(workDir, currentBranch string, isWorktree bool, tmuxHint string) string {
	_ = isWorktree
	_ = tmuxHint

	if workDir == "" {
		return "session"
	}

	cleanWorkDir := filepath.Clean(workDir)

	localRoot, ok := findRepoRoot(cleanWorkDir)
	if !ok {
		return filepath.Base(cleanWorkDir)
	}
	localRoot = filepath.Clean(localRoot)

	parts := make([]string, 0, 4)

	// A worktree's ".git" is a regular file pointing at the main repo. Resolve
	// it so the baseline uses the original repo dir name rather than the
	// worktree directory basename (e.g. "jin-da43e8da"), which is meaningless
	// to a human reader.
	if mainRoot, isWt := resolveMainRepoIfWorktree(localRoot); isWt {
		parts = append(parts, filepath.Base(mainRoot))
		parts = append(parts, filepath.Base(localRoot))
	} else {
		parts = append(parts, filepath.Base(localRoot))
	}

	if currentBranch != "" {
		parts = append(parts, currentBranch)
	}

	if rel, err := filepath.Rel(localRoot, cleanWorkDir); err == nil && rel != "." && rel != "" {
		parts = append(parts, rel)
	}

	return strings.Join(parts, ":")
}

// resolveMainRepoIfWorktree inspects the ".git" entry at localRoot. When it is
// a regular file containing "gitdir: /path/to/main/.git/worktrees/<name>",
// returns the main repo root plus isWorktree=true. Otherwise (".git" absent,
// a directory, or a malformed pointer) returns (localRoot, false) so the
// caller can treat localRoot as the repo root.
func resolveMainRepoIfWorktree(localRoot string) (mainRoot string, isWorktree bool) {
	gitPath := filepath.Join(localRoot, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil || !fi.Mode().IsRegular() {
		return localRoot, false
	}
	content, err := os.ReadFile(gitPath)
	if err != nil {
		return localRoot, false
	}
	raw := strings.TrimSpace(string(content))
	if !strings.HasPrefix(raw, "gitdir: ") {
		return localRoot, false
	}
	gitdir := strings.TrimPrefix(raw, "gitdir: ")
	// Require the canonical worktree marker layout so a garbled or truncated
	// gitdir (e.g. "/nowhere") does not resolve to filesystem root.
	if !strings.Contains(gitdir, "/.git/worktrees/") {
		return localRoot, false
	}
	// gitdir layout: <main-repo>/.git/worktrees/<name> — three Dir() calls to
	// reach the main repo root.
	mainRepo := filepath.Dir(filepath.Dir(filepath.Dir(gitdir)))
	if mainRepo == "" || mainRepo == "." || mainRepo == "/" {
		return localRoot, false
	}
	return mainRepo, true
}

// findRepoRoot walks up from dir looking for a directory that contains a .git
// entry (either a directory in the main repo, or a regular file in a
// worktree). Bounded by maxRepoRootWalk as a safety net against symlink loops
// or unexpectedly deep paths.
func findRepoRoot(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	cur := dir
	for i := 0; i < maxRepoRootWalk; i++ {
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil {
			return cur, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		cur = parent
	}
	return "", false
}

// DescriptionLayer classifies the source that last wrote a session's
// Description. Larger values represent higher-quality, more informative
// sources. Manager.TryUpgradeDescription only accepts a candidate whose layer
// is strictly greater than the session's current layer, so promotion is
// monotonic within a daemon lifetime.
//
// The zero value (DescriptionLayerBaseline) is what freshly-created sessions
// carry and what daemon restart resets in-memory sessions to, since the layer
// is runtime-only (see Session.DescriptionLayer).
type DescriptionLayer int

const (
	// DescriptionLayerBaseline is Layer A: the repo:branch label produced by
	// GenerateBaselineDescription. Always present, never informative on its own.
	DescriptionLayerBaseline DescriptionLayer = 0
	// DescriptionLayerAgentName is Layer C-name: an agent-supplied session
	// name (e.g. Claude Code's ~/.claude/sessions/<PID>.json "name" field),
	// available as early as the SessionStart hook.
	DescriptionLayerAgentName DescriptionLayer = 1
	// DescriptionLayerTranscript is Layer C-transcript: the first meaningful
	// user prompt mined from the agent transcript, only available after the
	// first user turn has been flushed to disk.
	DescriptionLayerTranscript DescriptionLayer = 2
)

// DescriptionEnhancer produces an agent-specific "Layer C" description upgrade
// from live session state (e.g., the first user prompt in a transcript, or a
// session name Claude Code writes to disk at start-up).
// Implementations must be side-effect free and safe to call concurrently.
type DescriptionEnhancer interface {
	// TryGenerate returns a candidate description together with the layer it
	// belongs to. Returns ("", 0, false) when no useful signal is available
	// yet (e.g., the transcript has no meaningful first user turn and the
	// agent has not yet named the session). Must not mutate sess.
	TryGenerate(sess *Session) (string, DescriptionLayer, bool)
}
