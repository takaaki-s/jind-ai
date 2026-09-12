package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectCleanupWorktreeAndDeleteExactBranch(t *testing.T) {
	repo, worktree, head := cleanupTestRepo(t)
	client := NewClient()

	got, err := client.InspectCleanupWorktree(context.Background(), worktree, head)
	if err != nil {
		t.Fatal(err)
	}
	if got.RepositoryPath != repo || got.WorktreePath != worktree || got.Branch != "feature" || got.HeadCommit != head || got.UnpushedCommits != 0 {
		t.Fatalf("inspection = %+v", got)
	}
	if err := client.RemoveWorktree(worktree, false); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteExactBranch(repo, "feature", head); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := client.LocalBranchHead(repo, "feature"); err != nil || exists {
		t.Fatalf("deleted branch exists=%t err=%v", exists, err)
	}
}

func TestInspectCleanupWorktreeRejectsBroadAndForeignTargets(t *testing.T) {
	repo, worktree, head := cleanupTestRepo(t)
	client := NewClient()
	if _, err := client.InspectCleanupWorktree(context.Background(), string(filepath.Separator), head); err == nil {
		t.Fatal("filesystem root was accepted")
	}
	if _, err := client.InspectCleanupWorktree(context.Background(), repo, head); err == nil {
		t.Fatal("primary repository was accepted as a linked worktree")
	}
	if _, err := client.InspectCleanupWorktree(context.Background(), worktree, strings.Repeat("x", 40)); err == nil {
		t.Fatal("non-hex verified head was accepted")
	}
}

func cleanupTestRepo(t *testing.T) (repo, worktree, head string) {
	t.Helper()
	repo = t.TempDir()
	runCleanupGit(t, repo, "init", "-b", "main")
	runCleanupGit(t, repo, "config", "user.email", "test@example.com")
	runCleanupGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCleanupGit(t, repo, "add", "--", "base.txt")
	runCleanupGit(t, repo, "commit", "-m", "base")
	worktree = filepath.Join(t.TempDir(), "worktree")
	runCleanupGit(t, repo, "worktree", "add", "-b", "feature", worktree)
	if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCleanupGit(t, worktree, "add", "--", "feature.txt")
	runCleanupGit(t, worktree, "commit", "-m", "feature")
	head = runCleanupGit(t, worktree, "rev-parse", "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", worktree).Run() })
	return repo, worktree, head
}

func runCleanupGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
