// Package git wraps the git CLI for the operations jind-ai needs
// (worktree management, branch detection). The Runner interface exists so
// tests can drive Client without spawning real git processes.
package git

import "os/exec"

// Runner executes a git subcommand in the given working directory and returns
// the combined stdout+stderr. Combined output is important because git prints
// user-visible failure reasons (e.g. "contains modified or untracked files")
// on stderr, and callers parse that to distinguish error kinds.
type Runner interface {
	Run(dir string, args ...string) ([]byte, error)
}

// Client is a thin, testable wrapper over the git CLI.
type Client struct {
	r Runner
}

// NewClient returns a Client that shells out to the real git binary.
func NewClient() *Client {
	return &Client{r: &execRunner{}}
}

// NewClientWithRunner returns a Client backed by the given Runner. Intended
// for tests.
func NewClientWithRunner(r Runner) *Client {
	return &Client{r: r}
}

type execRunner struct{}

func (execRunner) Run(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
