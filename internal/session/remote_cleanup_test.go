package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupRemoteOwnedRemovesOnlyProvenStoppedSessionResources(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	repo, worktree := remoteCleanupRepo(t)
	sess := Session{
		ID: "remote-session", WorkDir: worktree, Status: StatusStopped, AgentKind: "claude", Fleet: DefaultFleet,
		ReviewBase: ReviewBase{RequestedRef: "origin/main", CommitOID: remoteCleanupGit(t, worktree, "rev-parse", "HEAD"), WorktreePath: worktree},
	}
	mgr.mu.Lock()
	mgr.sessions[sess.ID] = &sess
	mgr.mu.Unlock()
	if err := mgr.store.Save(sess); err != nil {
		t.Fatal(err)
	}

	got, err := mgr.CleanupRemoteOwned(RemoteCleanupOwnership{
		SessionID: sess.ID, IdempotencyKey: "cleanup-key", RepositoryPath: repo,
		WorktreeName: filepath.Base(worktree), Branch: "remote-feature",
	})
	if err != nil || !got.Session || !got.Worktree || !got.Branch {
		t.Fatalf("cleanup = %+v, %v", got, err)
	}
	if _, ok := mgr.GetInfo(sess.ID); ok {
		t.Fatal("session remained")
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree remained: %v", err)
	}
	if mgr.gitClient.BranchExists(repo, "remote-feature") {
		t.Fatal("branch remained")
	}
}

func TestCleanupRemoteOwnedRejectsDirtyAndOwnershipMismatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Session, string)
	}{
		{name: "dirty", mutate: func(_ *Session, worktree string) {
			if err := os.WriteFile(filepath.Join(worktree, "untracked.txt"), []byte("keep\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "worktree name", mutate: func(sess *Session, _ string) { sess.Description = "mismatch" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			mgr, _, _ := newTestManager(t)
			repo, worktree := remoteCleanupRepo(t)
			sess := Session{ID: "remote-session", WorkDir: worktree, Status: StatusStopped, AgentKind: "claude", Fleet: DefaultFleet,
				ReviewBase: ReviewBase{CommitOID: remoteCleanupGit(t, worktree, "rev-parse", "HEAD"), WorktreePath: worktree}}
			test.mutate(&sess, worktree)
			mgr.mu.Lock()
			mgr.sessions[sess.ID] = &sess
			mgr.mu.Unlock()
			name := filepath.Base(worktree)
			if test.name == "worktree name" {
				name = "another-worktree"
			}
			if _, err := mgr.CleanupRemoteOwned(RemoteCleanupOwnership{
				SessionID: sess.ID, IdempotencyKey: "cleanup-key", RepositoryPath: repo,
				WorktreeName: name, Branch: "remote-feature",
			}); err == nil {
				t.Fatal("unsafe cleanup was accepted")
			}
			if _, err := os.Stat(worktree); err != nil {
				t.Fatalf("worktree changed: %v", err)
			}
		})
	}
}

func remoteCleanupRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	remoteCleanupGit(t, repo, "init", "-b", "main")
	remoteCleanupGit(t, repo, "config", "user.email", "test@example.com")
	remoteCleanupGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	remoteCleanupGit(t, repo, "add", "--", "base.txt")
	remoteCleanupGit(t, repo, "commit", "-m", "base")
	worktree := filepath.Join(t.TempDir(), "owned-worktree")
	remoteCleanupGit(t, repo, "worktree", "add", "-b", "remote-feature", worktree)
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", worktree).Run() })
	return repo, worktree
}

func remoteCleanupGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
