package git

import "errors"

// ErrDirty is returned when a worktree has uncommitted changes and force
// removal was not requested.
var ErrDirty = errors.New("worktree has uncommitted changes")

// ErrNotWorktree is returned when a path is not a git worktree (e.g. the main
// repository or a non-git directory).
var ErrNotWorktree = errors.New("path is not a git worktree")
