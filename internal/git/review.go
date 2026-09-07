package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// Each review command gets its own output ceiling. The file-count ceiling
	// below is normally reached first; this byte bound also covers one hostile
	// or accidental giant pathname without allocating until the process exits.
	reviewOutputLimit = 4 << 20
	maxReviewFiles    = 10_000
)

// ErrReviewFileLimit means the workspace has more paths than the bounded
// summary promises to count exactly.
var ErrReviewFileLimit = errors.New("review file limit exceeded")

// ReviewSummary is a point-in-time, local-only comparison of a worktree with
// the immutable base supplied by session.ReviewBase. It contains counts, not
// paths or patch contents, so it is small enough to persist with a session.
type ReviewSummary struct {
	BaseCommit     string
	HeadCommit     string
	Branch         string
	ChangedFiles   int
	Additions      int
	Deletions      int
	BinaryFiles    int
	UntrackedFiles int
	CommitCount    int
}

// InspectReview reads a bounded review summary without fetching, invoking a
// shell, running external diff drivers/textconv, or mutating the repository.
// Callers own the context deadline and concurrency bound.
func (c *Client) InspectReview(ctx context.Context, repoDir, baseCommit string) (ReviewSummary, error) {
	var out ReviewSummary
	if !isFullObjectID(baseCommit) {
		return out, fmt.Errorf("invalid review base object ID %q", baseCommit)
	}
	baseCommit = strings.ToLower(baseCommit)
	out.BaseCommit = baseCommit

	headRaw, err := c.runReviewCommand(ctx, repoDir,
		"rev-parse", "--verify", "--quiet", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return out, fmt.Errorf("resolve review head: %w", err)
	}
	head := strings.TrimSpace(string(headRaw))
	if !isFullObjectID(head) {
		return out, fmt.Errorf("review head is not a full object ID: %q", head)
	}
	out.HeadCommit = strings.ToLower(head)

	branchRaw, branchErr := c.runReviewCommand(ctx, repoDir,
		"symbolic-ref", "--quiet", "--short", "HEAD")
	if branchErr == nil {
		out.Branch = strings.TrimSpace(string(branchRaw))
	} else if len(bytes.TrimSpace(branchRaw)) != 0 {
		return out, fmt.Errorf("resolve review branch: %w", branchErr)
	}

	numstat, err := c.runReviewCommand(ctx, repoDir,
		"--no-pager", "diff", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--numstat", "-z", baseCommit, "--")
	if err != nil {
		return out, fmt.Errorf("read review diff: %w", err)
	}
	tracked, additions, deletions, binaries, err := parseNumstat(numstat)
	if err != nil {
		return out, err
	}

	untrackedRaw, err := c.runReviewCommand(ctx, repoDir,
		"--no-pager", "ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return out, fmt.Errorf("read untracked files: %w", err)
	}
	untracked, err := countNULRecords(untrackedRaw)
	if err != nil {
		return out, err
	}
	if tracked+untracked > maxReviewFiles {
		return out, fmt.Errorf("%w: more than %d changed paths", ErrReviewFileLimit, maxReviewFiles)
	}

	commitsRaw, err := c.runReviewCommand(ctx, repoDir,
		"--no-pager", "rev-list", "--count", baseCommit+".."+out.HeadCommit, "--")
	if err != nil {
		return out, fmt.Errorf("count review commits: %w", err)
	}
	commitCount, err := strconv.Atoi(strings.TrimSpace(string(commitsRaw)))
	if err != nil || commitCount < 0 {
		return out, fmt.Errorf("invalid review commit count %q", strings.TrimSpace(string(commitsRaw)))
	}

	out.ChangedFiles = tracked + untracked
	out.Additions = additions
	out.Deletions = deletions
	out.BinaryFiles = binaries
	out.UntrackedFiles = untracked
	out.CommitCount = commitCount
	return out, nil
}

func (c *Client) runReviewCommand(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r, ok := c.r.(contextRunner); ok {
		return r.RunContext(ctx, dir, reviewOutputLimit, args...)
	}
	out, err := c.r.Run(dir, args...)
	if len(out) > reviewOutputLimit {
		return out[:reviewOutputLimit], ErrOutputLimit
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return out, ctxErr
	}
	return out, err
}

func parseNumstat(data []byte) (files, additions, deletions, binaries int, err error) {
	records := bytes.Split(data, []byte{0})
	for _, record := range records {
		if len(record) == 0 {
			continue
		}
		files++
		if files > maxReviewFiles {
			return 0, 0, 0, 0, fmt.Errorf("%w: more than %d tracked paths", ErrReviewFileLimit, maxReviewFiles)
		}
		fields := bytes.SplitN(record, []byte{'\t'}, 3)
		if len(fields) != 3 {
			return 0, 0, 0, 0, fmt.Errorf("invalid git numstat record")
		}
		if bytes.Equal(fields[0], []byte("-")) || bytes.Equal(fields[1], []byte("-")) {
			binaries++
			continue
		}
		add, addErr := strconv.Atoi(string(fields[0]))
		del, delErr := strconv.Atoi(string(fields[1]))
		if addErr != nil || delErr != nil || add < 0 || del < 0 {
			return 0, 0, 0, 0, fmt.Errorf("invalid git numstat counts")
		}
		additions += add
		deletions += del
	}
	return files, additions, deletions, binaries, nil
}

func countNULRecords(data []byte) (int, error) {
	count := 0
	for _, record := range bytes.Split(data, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		count++
		if count > maxReviewFiles {
			return 0, fmt.Errorf("%w: more than %d untracked paths", ErrReviewFileLimit, maxReviewFiles)
		}
	}
	return count, nil
}
