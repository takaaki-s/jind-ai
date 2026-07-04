package git

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type mockRunner struct {
	lastDir  string
	lastArgs []string
	out      []byte
	err      error
}

func (m *mockRunner) Run(dir string, args ...string) ([]byte, error) {
	m.lastDir = dir
	m.lastArgs = args
	return m.out, m.err
}

func TestIsGitRoot(t *testing.T) {
	t.Run("missing .git returns false", func(t *testing.T) {
		if IsGitRoot(t.TempDir()) {
			t.Error("expected false for dir without .git")
		}
	})
	t.Run(".git as regular file returns true", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /x"), 0o644); err != nil {
			t.Fatalf("write .git: %v", err)
		}
		if !IsGitRoot(dir) {
			t.Error("expected true for .git file")
		}
	})
	t.Run(".git as directory returns true", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
		if !IsGitRoot(dir) {
			t.Error("expected true for .git directory")
		}
	})
}

func TestIsGitWorktreeDir(t *testing.T) {
	t.Run("empty path returns false", func(t *testing.T) {
		if IsGitWorktreeDir("") {
			t.Error("expected false for empty path")
		}
	})
	t.Run("missing .git returns false", func(t *testing.T) {
		if IsGitWorktreeDir(t.TempDir()) {
			t.Error("expected false for dir without .git")
		}
	})
	t.Run(".git as directory returns false", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
		if IsGitWorktreeDir(dir) {
			t.Error("expected false when .git is a directory (main repo)")
		}
	})
	t.Run(".git as regular file returns true", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /x"), 0o644); err != nil {
			t.Fatalf("write .git: %v", err)
		}
		if !IsGitWorktreeDir(dir) {
			t.Error("expected true when .git is a regular file (worktree)")
		}
	})
}

func TestIsClaudeWorktreePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/home/user/project", false},
		{"/home/user/project/.git", false},
		{"/home/user/project/src", false},
		{"/home/user/project/.claude/worktrees/feat-xyz", true},
		{"/home/user/project/.claude/worktrees/COR-24444", true},
		{"/tmp/.claude/worktrees/test", true},
		{"/home/user/project/.claude/workdir", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsClaudeWorktreePath(tt.path); got != tt.want {
			t.Errorf("IsClaudeWorktreePath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestResolveWorktreeDir(t *testing.T) {
	// Build a real worktree-shaped dir and a plain dir so we can exercise the
	// preference logic without mocking os.Lstat.
	makeWorktreeDir := func(t *testing.T) string {
		t.Helper()
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, ".git"), []byte("gitdir: /x"), 0o644); err != nil {
			t.Fatalf("write .git: %v", err)
		}
		return d
	}

	t.Run("both are worktrees prefers currentWorkDir", func(t *testing.T) {
		cur := makeWorktreeDir(t)
		wd := makeWorktreeDir(t)
		if got := ResolveWorktreeDir(cur, wd); got != cur {
			t.Errorf("got %q, want %q", got, cur)
		}
	})
	t.Run("only currentWorkDir is worktree", func(t *testing.T) {
		cur := makeWorktreeDir(t)
		wd := t.TempDir()
		if got := ResolveWorktreeDir(cur, wd); got != cur {
			t.Errorf("got %q, want %q", got, cur)
		}
	})
	t.Run("only workDir is worktree", func(t *testing.T) {
		cur := t.TempDir()
		wd := makeWorktreeDir(t)
		if got := ResolveWorktreeDir(cur, wd); got != wd {
			t.Errorf("got %q, want %q", got, wd)
		}
	})
	t.Run("neither is worktree returns currentWorkDir if non-empty", func(t *testing.T) {
		cur := t.TempDir()
		wd := t.TempDir()
		if got := ResolveWorktreeDir(cur, wd); got != cur {
			t.Errorf("got %q, want %q", got, cur)
		}
	})
	t.Run("both empty returns empty", func(t *testing.T) {
		if got := ResolveWorktreeDir("", ""); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("currentWorkDir empty falls back to workDir", func(t *testing.T) {
		wd := t.TempDir()
		if got := ResolveWorktreeDir("", wd); got != wd {
			t.Errorf("got %q, want %q", got, wd)
		}
	})
}

// setupWorktreeLayout creates a directory that looks like a git worktree,
// including a .git file whose gitdir line points at a plausible main repo
// path so RemoveWorktree can parse it.
func setupWorktreeLayout(t *testing.T) (mainRepo, worktreeDir string) {
	t.Helper()
	base := t.TempDir()
	mainRepo = filepath.Join(base, "main")
	worktreeDir = filepath.Join(base, "wt")

	if err := os.MkdirAll(filepath.Join(mainRepo, ".git", "worktrees", "wt"), 0o755); err != nil {
		t.Fatalf("mkdir main .git: %v", err)
	}
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	gitdirLine := "gitdir: " + filepath.Join(mainRepo, ".git", "worktrees", "wt")
	if err := os.WriteFile(filepath.Join(worktreeDir, ".git"), []byte(gitdirLine+"\n"), 0o644); err != nil {
		t.Fatalf("write worktree .git: %v", err)
	}
	return mainRepo, worktreeDir
}

func TestClient_RemoveWorktree_Success(t *testing.T) {
	mainRepo, worktreeDir := setupWorktreeLayout(t)
	mock := &mockRunner{}
	c := NewClientWithRunner(mock)

	if err := c.RemoveWorktree(worktreeDir, false); err != nil {
		t.Fatalf("RemoveWorktree failed: %v", err)
	}

	if mock.lastDir != mainRepo {
		t.Errorf("Runner dir = %q, want %q", mock.lastDir, mainRepo)
	}
	wantArgs := []string{"worktree", "remove", "--", worktreeDir}
	if !reflect.DeepEqual(mock.lastArgs, wantArgs) {
		t.Errorf("Runner args = %v, want %v", mock.lastArgs, wantArgs)
	}
}

func TestClient_RemoveWorktree_NotWorktree_DotGitIsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	c := NewClientWithRunner(&mockRunner{})

	err := c.RemoveWorktree(dir, false)
	if !errors.Is(err, ErrNotWorktree) {
		t.Fatalf("expected ErrNotWorktree, got %v", err)
	}
}

func TestClient_RemoveWorktree_NotWorktree_MissingDotGit(t *testing.T) {
	dir := t.TempDir()
	c := NewClientWithRunner(&mockRunner{})

	err := c.RemoveWorktree(dir, false)
	if !errors.Is(err, ErrNotWorktree) {
		t.Fatalf("expected ErrNotWorktree, got %v", err)
	}
}

func TestClient_RemoveWorktree_NonexistentIsIdempotent(t *testing.T) {
	c := NewClientWithRunner(&mockRunner{})

	err := c.RemoveWorktree(filepath.Join(t.TempDir(), "does-not-exist"), false)
	if err != nil {
		t.Fatalf("expected nil for nonexistent path, got %v", err)
	}
}

func TestClient_RemoveWorktree_DirtyReturnsErrDirty(t *testing.T) {
	tests := []struct {
		name string
		out  string
	}{
		{"modified or untracked", "fatal: 'wt' contains modified or untracked files, use --force to delete it\n"},
		{"is dirty", "fatal: 'wt' is dirty (untracked files), use --force\n"},
		{"contains untracked files", "fatal: 'wt' contains untracked files, use --force to delete it\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, worktreeDir := setupWorktreeLayout(t)
			mock := &mockRunner{
				out: []byte(tt.out),
				err: errors.New("exit status 128"),
			}
			c := NewClientWithRunner(mock)

			err := c.RemoveWorktree(worktreeDir, false)
			if !errors.Is(err, ErrDirty) {
				t.Fatalf("expected ErrDirty for %q, got %v", tt.name, err)
			}
		})
	}
}

func TestClient_RemoveWorktree_ForcePropagatesFlag(t *testing.T) {
	mainRepo, worktreeDir := setupWorktreeLayout(t)
	mock := &mockRunner{}
	c := NewClientWithRunner(mock)

	if err := c.RemoveWorktree(worktreeDir, true); err != nil {
		t.Fatalf("RemoveWorktree(force=true) failed: %v", err)
	}

	if mock.lastDir != mainRepo {
		t.Errorf("Runner dir = %q, want %q", mock.lastDir, mainRepo)
	}
	wantArgs := []string{"worktree", "remove", "--force", "--", worktreeDir}
	if !reflect.DeepEqual(mock.lastArgs, wantArgs) {
		t.Errorf("Runner args = %v, want %v", mock.lastArgs, wantArgs)
	}
}

func TestClient_RemoveWorktree_OtherErrorIsWrapped(t *testing.T) {
	_, worktreeDir := setupWorktreeLayout(t)
	mock := &mockRunner{
		out: []byte("fatal: something else went wrong\n"),
		err: errors.New("exit status 1"),
	}
	c := NewClientWithRunner(mock)

	err := c.RemoveWorktree(worktreeDir, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrDirty) || errors.Is(err, ErrNotWorktree) {
		t.Errorf("unexpected sentinel error match: %v", err)
	}
}
