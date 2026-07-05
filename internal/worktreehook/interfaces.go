// Package worktreehook discovers, verifies, and executes the optional
// .jin/worktree-post-create.sh script that runs after a git worktree is
// created for a new session. Verification uses a direnv-style allowlist keyed
// by repository absolute path and content SHA256, so a repository must be
// explicitly trusted before its hook is executed.
package worktreehook

import (
	"context"
	"time"
)

// Verdict is the outcome of Runner.Verify (or the caller's Discover check).
// The zero value is VerdictOK to keep unit-test setup terse; callers must
// always inspect Verify's returned value explicitly.
type Verdict int

const (
	VerdictOK         Verdict = iota // trusted and unchanged; safe to run
	VerdictNoScript                  // no .jin/worktree-post-create.sh present
	VerdictNotAllowed                // script exists but repo is not in the allowlist
	VerdictChanged                   // repo is allowed but the script's SHA256 differs
	VerdictDisabled                  // disabled via config or --no-hook
)

// RunOptions carries the concrete parameters for a single hook invocation.
// Fields are populated by internal/session at call time; none are optional
// unless noted.
type RunOptions struct {
	ScriptPath   string
	WorktreePath string
	RepoRoot     string
	Branch       string
	Base         string
	SessionID    string
	SessionName  string // may be empty if the session name is unset at hook time
	LogPath      string
	// Timeout is used only for error message formatting ("hook timed out
	// after 5m0s"). The actual cancellation deadline must be set on ctx by
	// the caller — Runner does not derive one from this field. Zero omits
	// the duration from the error message.
	Timeout time.Duration
}

// Runner is the seam that session.Manager depends on. The concrete
// implementation returned by NewRunner shells out to bash; tests inject a
// fake to avoid spawning real processes.
type Runner interface {
	Discover(repoRoot string) (scriptPath string, exists bool)
	Verify(scriptPath, repoRoot string) (Verdict, error)
	Run(ctx context.Context, opts RunOptions) error
	HookLogPath(stateDir, sessionID string) string
}
