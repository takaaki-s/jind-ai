package git

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestClient_DetectDefaultBranch_ParsesOriginHEAD(t *testing.T) {
	mock := &mockRunner{out: []byte("refs/remotes/origin/main\n")}
	c := NewClientWithRunner(mock)

	branch, err := c.DetectDefaultBranch("/repo")
	if err != nil {
		t.Fatalf("DetectDefaultBranch failed: %v", err)
	}
	if branch != "main" {
		t.Errorf("branch = %q, want %q", branch, "main")
	}
	if mock.lastDir != "/repo" {
		t.Errorf("Runner dir = %q, want %q", mock.lastDir, "/repo")
	}
	wantArgs := []string{"symbolic-ref", "refs/remotes/origin/HEAD"}
	if !reflect.DeepEqual(mock.lastArgs, wantArgs) {
		t.Errorf("Runner args = %v, want %v", mock.lastArgs, wantArgs)
	}
}

func TestClient_DetectDefaultBranch_ReturnsErrorWhenGitFails(t *testing.T) {
	mock := &mockRunner{
		out: []byte("fatal: ref refs/remotes/origin/HEAD is not a symbolic ref\n"),
		err: errors.New("exit status 128"),
	}
	c := NewClientWithRunner(mock)

	if _, err := c.DetectDefaultBranch("/repo"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestClient_DetectDefaultBranch_ErrorsOnUnexpectedFormat(t *testing.T) {
	mock := &mockRunner{out: []byte("blah")}
	c := NewClientWithRunner(mock)

	if _, err := c.DetectDefaultBranch("/repo"); err == nil {
		t.Fatal("expected error for unexpected output, got nil")
	}
}

func TestClient_Fetch_SendsExpectedArgs(t *testing.T) {
	mock := &mockRunner{}
	c := NewClientWithRunner(mock)

	if err := c.Fetch("/repo", "origin", "main"); err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if mock.lastDir != "/repo" {
		t.Errorf("Runner dir = %q, want %q", mock.lastDir, "/repo")
	}
	wantArgs := []string{"fetch", "origin", "main"}
	if !reflect.DeepEqual(mock.lastArgs, wantArgs) {
		t.Errorf("Runner args = %v, want %v", mock.lastArgs, wantArgs)
	}
}

func TestClient_Fetch_WrapsError(t *testing.T) {
	mock := &mockRunner{
		out: []byte("fatal: could not read from remote repository"),
		err: errors.New("exit status 128"),
	}
	c := NewClientWithRunner(mock)

	err := c.Fetch("/repo", "origin", "main")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "could not read from remote repository") {
		t.Errorf("error %q should contain the git output", err.Error())
	}
}

func TestClient_BranchExists_TrueWhenGitOK(t *testing.T) {
	mock := &mockRunner{}
	c := NewClientWithRunner(mock)

	if !c.BranchExists("/repo", "main") {
		t.Error("expected true when git rev-parse succeeds")
	}
	wantArgs := []string{"rev-parse", "--verify", "--quiet", "refs/heads/main"}
	if !reflect.DeepEqual(mock.lastArgs, wantArgs) {
		t.Errorf("Runner args = %v, want %v", mock.lastArgs, wantArgs)
	}
}

func TestClient_BranchExists_FalseOnError(t *testing.T) {
	mock := &mockRunner{err: errors.New("exit status 1")}
	c := NewClientWithRunner(mock)

	if c.BranchExists("/repo", "no-such-branch") {
		t.Error("expected false when git rev-parse errors")
	}
}

func TestClient_AddWorktree_SendsExpectedArgs(t *testing.T) {
	mock := &mockRunner{}
	c := NewClientWithRunner(mock)

	err := c.AddWorktree("/repo", "wip/jin-abcd1234", "/tmp/wt/jin-abcd1234", "origin/main")
	if err != nil {
		t.Fatalf("AddWorktree failed: %v", err)
	}
	if mock.lastDir != "/repo" {
		t.Errorf("Runner dir = %q, want %q", mock.lastDir, "/repo")
	}
	wantArgs := []string{"worktree", "add", "-b", "wip/jin-abcd1234", "/tmp/wt/jin-abcd1234", "origin/main"}
	if !reflect.DeepEqual(mock.lastArgs, wantArgs) {
		t.Errorf("Runner args = %v, want %v", mock.lastArgs, wantArgs)
	}
}

func TestClient_AddWorktree_WrapsError(t *testing.T) {
	mock := &mockRunner{
		out: []byte("fatal: destination path exists"),
		err: errors.New("exit status 128"),
	}
	c := NewClientWithRunner(mock)

	err := c.AddWorktree("/repo", "b", "/tmp/wt", "origin/main")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "destination path exists") {
		t.Errorf("error %q should contain git output", err.Error())
	}
}

func TestClient_DeleteBranch_SendsExpectedArgs(t *testing.T) {
	mock := &mockRunner{}
	c := NewClientWithRunner(mock)

	if err := c.DeleteBranch("/repo", "wip/jin-abcd1234"); err != nil {
		t.Fatalf("DeleteBranch failed: %v", err)
	}
	if mock.lastDir != "/repo" {
		t.Errorf("Runner dir = %q, want %q", mock.lastDir, "/repo")
	}
	wantArgs := []string{"branch", "-D", "--", "wip/jin-abcd1234"}
	if !reflect.DeepEqual(mock.lastArgs, wantArgs) {
		t.Errorf("Runner args = %v, want %v", mock.lastArgs, wantArgs)
	}
}

func TestClient_PruneWorktrees_SendsExpectedArgs(t *testing.T) {
	mock := &mockRunner{}
	c := NewClientWithRunner(mock)

	if err := c.PruneWorktrees("/repo"); err != nil {
		t.Fatalf("PruneWorktrees failed: %v", err)
	}
	if mock.lastDir != "/repo" {
		t.Errorf("Runner dir = %q, want %q", mock.lastDir, "/repo")
	}
	wantArgs := []string{"worktree", "prune"}
	if !reflect.DeepEqual(mock.lastArgs, wantArgs) {
		t.Errorf("Runner args = %v, want %v", mock.lastArgs, wantArgs)
	}
}

func TestClient_PruneWorktrees_WrapsError(t *testing.T) {
	mock := &mockRunner{
		out: []byte("fatal: not a git repository"),
		err: errors.New("exit status 128"),
	}
	c := NewClientWithRunner(mock)

	err := c.PruneWorktrees("/repo")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error %q should contain git output", err.Error())
	}
}
