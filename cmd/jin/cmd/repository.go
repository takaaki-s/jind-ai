package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/takaaki-s/jind-ai/internal/procgroup"
)

func resolveLocalRepoRoot(raw string) (string, error) {
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve repository: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	gitCmd := procgroup.CommandContext(ctx, "git", "-C", abs, "rev-parse", "--show-toplevel")
	gitCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat")
	out, err := gitCmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("inspect repository: %w", ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s (%s)", abs, strings.TrimSpace(string(out)))
	}
	root := strings.TrimSpace(string(out))
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("git returned a non-absolute repository root %q", root)
	}
	return filepath.Clean(root), nil
}
