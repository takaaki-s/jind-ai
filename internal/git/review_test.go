package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectReview_CountsCommittedWorkingBinaryAndUntrackedChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runReviewGit(t, repo, "init")
	runReviewGit(t, repo, "config", "user.email", "test@example.com")
	runReviewGit(t, repo, "config", "user.name", "Test User")
	runReviewGit(t, repo, "branch", "-M", "main")

	writeReviewFile(t, repo, "tracked.txt", []byte("before\n"))
	writeReviewFile(t, repo, "binary.dat", []byte{0, 1, 2})
	runReviewGit(t, repo, "add", "--", "tracked.txt", "binary.dat")
	runReviewGit(t, repo, "commit", "-m", "base")
	base := runReviewGit(t, repo, "rev-parse", "HEAD")

	writeReviewFile(t, repo, "committed.txt", []byte("committed\n"))
	runReviewGit(t, repo, "add", "--", "committed.txt")
	runReviewGit(t, repo, "commit", "-m", "one commit")
	writeReviewFile(t, repo, "tracked.txt", []byte("after one\nafter two\n"))
	writeReviewFile(t, repo, "binary.dat", []byte{0, 3, 4})
	writeReviewFile(t, repo, "odd\tname\n.txt", []byte("untracked\n"))

	got, err := NewClient().InspectReview(context.Background(), repo, base)
	if err != nil {
		t.Fatalf("InspectReview: %v", err)
	}
	if got.BaseCommit != base {
		t.Errorf("BaseCommit = %q, want %q", got.BaseCommit, base)
	}
	if got.HeadCommit != runReviewGit(t, repo, "rev-parse", "HEAD") {
		t.Errorf("HeadCommit = %q", got.HeadCommit)
	}
	if got.Branch != "main" {
		t.Errorf("Branch = %q, want main", got.Branch)
	}
	if got.ChangedFiles != 4 || got.UntrackedFiles != 1 || got.BinaryFiles != 1 {
		t.Errorf("file counts = changed:%d untracked:%d binary:%d, want 4/1/1",
			got.ChangedFiles, got.UntrackedFiles, got.BinaryFiles)
	}
	if got.Additions != 3 || got.Deletions != 1 {
		t.Errorf("line counts = +%d -%d, want +3 -1", got.Additions, got.Deletions)
	}
	if got.CommitCount != 1 {
		t.Errorf("CommitCount = %d, want 1", got.CommitCount)
	}
}

func TestInspectReview_RejectsInvalidBaseBeforeRunningGit(t *testing.T) {
	runner := &mockRunner{}
	_, err := NewClientWithRunner(runner).InspectReview(context.Background(), "/repo", "main")
	if err == nil {
		t.Fatal("InspectReview accepted a non-OID base")
	}
	if runner.lastArgs != nil {
		t.Fatalf("git ran with args %v", runner.lastArgs)
	}
}

func TestRunReviewCommand_EnforcesOutputLimitForSimpleRunners(t *testing.T) {
	runner := &mockRunner{out: []byte(strings.Repeat("x", reviewOutputLimit+1))}
	_, err := NewClientWithRunner(runner).runReviewCommand(context.Background(), "/repo", "status")
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("error = %v, want ErrOutputLimit", err)
	}
}

func TestParseNumstat_HandlesTabsInPathAndBinary(t *testing.T) {
	data := []byte("2\t1\tpath\twith-tab\x00-\t-\tbinary\x00")
	files, add, del, binary, err := parseNumstat(data)
	if err != nil {
		t.Fatalf("parseNumstat: %v", err)
	}
	if files != 2 || add != 2 || del != 1 || binary != 1 {
		t.Fatalf("got files/add/del/binary %d/%d/%d/%d", files, add, del, binary)
	}
}

func runReviewGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeReviewFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("write %q: %v", name, err)
	}
}
