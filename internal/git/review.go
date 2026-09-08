package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	BaseCommit string
	HeadCommit string
	Branch     string
	// WorkspaceFingerprint identifies the exact checked-out contents covered by
	// this observation. It includes the immutable base, HEAD, changed path
	// records, and the contents of regular/symlink files in the delta. Callers
	// can bind externally-run check reports to it without running those checks.
	WorkspaceFingerprint string
	ChangedFiles         int
	Additions            int
	Deletions            int
	BinaryFiles          int
	UntrackedFiles       int
	CommitCount          int
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
	tracked, additions, deletions, binaries, trackedPaths, err := parseNumstatPaths(numstat)
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

	fingerprint, err := fingerprintWorkspace(ctx, repoDir, baseCommit, out.HeadCommit, numstat, untrackedRaw, trackedPaths)
	if err != nil {
		return out, fmt.Errorf("fingerprint review workspace: %w", err)
	}
	out.WorkspaceFingerprint = fingerprint

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
	files, additions, deletions, binaries, _, err = parseNumstatPaths(data)
	return
}

func parseNumstatPaths(data []byte) (files, additions, deletions, binaries int, paths [][]byte, err error) {
	records := bytes.Split(data, []byte{0})
	for _, record := range records {
		if len(record) == 0 {
			continue
		}
		files++
		if files > maxReviewFiles {
			return 0, 0, 0, 0, nil, fmt.Errorf("%w: more than %d tracked paths", ErrReviewFileLimit, maxReviewFiles)
		}
		fields := bytes.SplitN(record, []byte{'\t'}, 3)
		if len(fields) != 3 {
			return 0, 0, 0, 0, nil, fmt.Errorf("invalid git numstat record")
		}
		paths = append(paths, fields[2])
		if bytes.Equal(fields[0], []byte("-")) || bytes.Equal(fields[1], []byte("-")) {
			binaries++
			continue
		}
		add, addErr := strconv.Atoi(string(fields[0]))
		del, delErr := strconv.Atoi(string(fields[1]))
		if addErr != nil || delErr != nil || add < 0 || del < 0 {
			return 0, 0, 0, 0, nil, fmt.Errorf("invalid git numstat counts")
		}
		additions += add
		deletions += del
	}
	return files, additions, deletions, binaries, paths, nil
}

// fingerprintWorkspace hashes the repository evidence already collected by
// InspectReview plus the current bytes behind every changed/untracked path.
// Reading files directly keeps the operation local and read-only, supports NUL
// and newline-bearing Git paths, and avoids writing blobs through git
// hash-object. The caller's timeout bounds large-file reads.
func fingerprintWorkspace(
	ctx context.Context,
	repoDir, baseCommit, headCommit string,
	numstat, untrackedRaw []byte,
	trackedPaths [][]byte,
) (string, error) {
	h := sha256.New()
	writeFingerprintField(h, []byte("jind-ai-review-workspace-v1"))
	writeFingerprintField(h, []byte(baseCommit))
	writeFingerprintField(h, []byte(headCommit))
	writeFingerprintField(h, numstat)
	writeFingerprintField(h, untrackedRaw)

	paths := make([][]byte, 0, len(trackedPaths)+bytes.Count(untrackedRaw, []byte{0}))
	paths = append(paths, trackedPaths...)
	for _, path := range bytes.Split(untrackedRaw, []byte{0}) {
		if len(path) != 0 {
			paths = append(paths, path)
		}
	}
	for _, rawPath := range paths {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		rel := filepath.Clean(string(rawPath))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("changed path escapes worktree: %q", rawPath)
		}
		writeFingerprintField(h, rawPath)
		if err := hashWorkspacePath(ctx, h, filepath.Join(repoDir, rel)); err != nil {
			return "", fmt.Errorf("hash %q: %w", rawPath, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeFingerprintField(dst io.Writer, data []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(data)))
	_, _ = dst.Write(size[:])
	_, _ = dst.Write(data)
}

func hashWorkspacePath(ctx context.Context, dst io.Writer, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeFingerprintField(dst, []byte("missing"))
			return nil
		}
		return err
	}
	writeFingerprintField(dst, []byte(fmt.Sprintf("mode:%d:size:%d", info.Mode(), info.Size())))
	switch {
	case info.Mode().IsRegular():
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		buf := make([]byte, 64<<10)
		var total int64
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, readErr := f.Read(buf)
			if n > 0 {
				total += int64(n)
				if _, err := dst.Write(buf[:n]); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				if total != info.Size() {
					return fmt.Errorf("file changed size while fingerprinting")
				}
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		writeFingerprintField(dst, []byte(target))
		return nil
	case info.IsDir():
		return hashWorkspaceDirectory(ctx, dst, path)
	default:
		// Device/FIFO/socket contents are deliberately not opened. Git cannot
		// track them as ordinary blobs; their mode and path still distinguish
		// the entry.
		return nil
	}
}

// hashWorkspaceDirectory handles a changed gitlink conservatively by hashing
// its checked-out tree while excluding Git's own metadata. WalkDir is lexical,
// does not follow symlinks, and shares the outer timeout/path cap.
func hashWorkspaceDirectory(ctx context.Context, dst io.Writer, root string) error {
	entries := 0
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries++
		if entries > maxReviewFiles {
			return fmt.Errorf("%w: more than %d gitlink paths", ErrReviewFileLimit, maxReviewFiles)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		writeFingerprintField(dst, []byte(rel))
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			writeFingerprintField(dst, []byte(fmt.Sprintf("mode:%d", info.Mode())))
			return nil
		}
		return hashWorkspacePath(ctx, dst, path)
	})
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
