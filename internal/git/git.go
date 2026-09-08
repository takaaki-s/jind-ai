// Package git wraps the git CLI for the operations jind-ai needs
// (worktree management, branch detection). The Runner interface exists so
// tests can drive Client without spawning real git processes.
package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"

	"github.com/takaaki-s/jind-ai/internal/procgroup"
)

// Runner executes a git subcommand in the given working directory and returns
// the combined stdout+stderr. Combined output is important because git prints
// user-visible failure reasons (e.g. "contains modified or untracked files")
// on stderr, and callers parse that to distinguish error kinds.
type Runner interface {
	Run(dir string, args ...string) ([]byte, error)
}

// contextRunner is the stronger runner used by review probes. It is kept
// optional so the small Runner test doubles used by the worktree code do not
// all have to grow cancellation machinery; Client.runReviewCommand applies
// the same result-size check to those doubles after Run returns.
type contextRunner interface {
	RunContext(ctx context.Context, dir string, maxOutput int, args ...string) ([]byte, error)
}

// ErrOutputLimit means a git probe produced more data than its caller agreed
// to hold. Review inspection treats this as unavailable evidence rather than
// continuing with a truncated, and therefore potentially misleading, count.
var ErrOutputLimit = errors.New("git output limit exceeded")

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

// RunContext bounds both the process lifetime and everything it writes. Git
// may launch helpers, so cancellation kills the process group rather than only
// the direct child. jind-ai supports Unix platforms and already depends on
// Unix sockets/tmux, making Setpgid the appropriate process boundary here.
func (execRunner) RunContext(ctx context.Context, dir string, maxOutput int, args ...string) ([]byte, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := procgroup.CommandContext(runCtx, "git", args...)
	cmd.Dir = dir
	// A read against a partial clone can otherwise perform a lazy network fetch.
	// Review assessment is promised to be local-only and non-interactive.
	cmd.Env = append(os.Environ(),
		"GIT_NO_LAZY_FETCH=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
	)
	out := &boundedBuffer{limit: maxOutput, cancel: cancel}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	data, exceeded := out.result()
	if exceeded {
		return data, ErrOutputLimit
	}
	if ctx.Err() != nil {
		return data, ctx.Err()
	}
	return data, err
}

// boundedBuffer accepts full writes so os/exec can drain its pipes, but keeps
// only limit bytes and cancels the command as soon as that boundary is crossed.
// Stdout and stderr may write concurrently, hence the mutex.
type boundedBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		_, _ = b.buf.Write(p[:min(remaining, n)])
	}
	if n > remaining {
		b.exceeded = true
		b.cancel()
	}
	return n, nil
}

func (b *boundedBuffer) result() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes()), b.exceeded
}
