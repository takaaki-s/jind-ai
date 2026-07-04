package cmd

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/takaaki-s/honjin/internal/session"
)

func TestRenderNewSessionJSON(t *testing.T) {
	t.Run("outputs session info as JSON", func(t *testing.T) {
		info := &session.Info{
			ID:        "abc-123",
			Name:      "my-session",
			WorkDir:   "/home/user/project",
			Status:    session.StatusCreating,
			CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		var buf bytes.Buffer
		if err := renderNewSessionJSON(&buf, info); err != nil {
			t.Fatalf("renderNewSessionJSON() error = %v", err)
		}
		var parsed session.Info
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
		}
		if parsed.ID != "abc-123" {
			t.Errorf("expected id %q, got %q", "abc-123", parsed.ID)
		}
		if parsed.Name != "my-session" {
			t.Errorf("expected name %q, got %q", "my-session", parsed.Name)
		}
		if parsed.Status != session.StatusCreating {
			t.Errorf("expected status %q, got %q", session.StatusCreating, parsed.Status)
		}
	})
}

// TestNewCmd_WorktreeFlagsParse verifies the --worktree* flags are registered
// on newCmd and parse to the expected values, without going through a daemon.
func TestNewCmd_WorktreeFlagsParse(t *testing.T) {
	flags := newCmd.Flags()

	if err := flags.Set("worktree", "true"); err != nil {
		t.Fatalf("Set(worktree): %v", err)
	}
	if err := flags.Set("worktree-name", "my-wt"); err != nil {
		t.Fatalf("Set(worktree-name): %v", err)
	}
	if err := flags.Set("worktree-branch", "feat/xyz"); err != nil {
		t.Fatalf("Set(worktree-branch): %v", err)
	}
	if err := flags.Set("worktree-base", "develop"); err != nil {
		t.Fatalf("Set(worktree-base): %v", err)
	}
	t.Cleanup(func() {
		_ = flags.Set("worktree", "false")
		_ = flags.Set("worktree-name", "")
		_ = flags.Set("worktree-branch", "")
		_ = flags.Set("worktree-base", "")
	})

	worktree, err := flags.GetBool("worktree")
	if err != nil || !worktree {
		t.Errorf("worktree: got (%v, %v), want (true, nil)", worktree, err)
	}
	worktreeName, err := flags.GetString("worktree-name")
	if err != nil || worktreeName != "my-wt" {
		t.Errorf("worktree-name: got (%q, %v), want (%q, nil)", worktreeName, err, "my-wt")
	}
	worktreeBranch, err := flags.GetString("worktree-branch")
	if err != nil || worktreeBranch != "feat/xyz" {
		t.Errorf("worktree-branch: got (%q, %v), want (%q, nil)", worktreeBranch, err, "feat/xyz")
	}
	worktreeBase, err := flags.GetString("worktree-base")
	if err != nil || worktreeBase != "develop" {
		t.Errorf("worktree-base: got (%q, %v), want (%q, nil)", worktreeBase, err, "develop")
	}
}
