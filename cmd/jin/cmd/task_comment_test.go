package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/task"
)

func newTaskCommentFlagCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	addTaskCommentFlags(cmd)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestTaskCommentRequestRequiresBodyModeAndConfirmKey(t *testing.T) {
	tests := [][]string{
		{"--body", "done"},
		{"--body", "done", "--dry-run", "--confirm"},
		{"--dry-run"},
		{"--body", "done", "--body-file", "x", "--dry-run"},
		{"--body", "done", "--confirm"},
	}
	for _, args := range tests {
		if _, err := taskCommentRequest(newTaskCommentFlagCommand(t, args...)); err == nil {
			t.Fatalf("accepted args %v", args)
		}
	}
}

func TestTaskCommentRequestMapsDryRunAndConfirm(t *testing.T) {
	dry, err := taskCommentRequest(newTaskCommentFlagCommand(t, "--body", "done", "--dry-run"))
	if err != nil || dry.Body != "done" || !dry.DryRun || dry.Confirm || dry.IdempotencyKey != "" {
		t.Fatalf("dry=%+v err=%v", dry, err)
	}
	confirmed, err := taskCommentRequest(newTaskCommentFlagCommand(t,
		"--body", "done", "--confirm", "--idempotency-key", "mut_key"))
	if err != nil || !confirmed.Confirm || confirmed.IdempotencyKey != "mut_key" {
		t.Fatalf("confirmed=%+v err=%v", confirmed, err)
	}
}

func TestTaskCommentRequestReadsBoundedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "comment.md")
	if err := os.WriteFile(path, []byte("from file"), 0600); err != nil {
		t.Fatal(err)
	}
	req, err := taskCommentRequest(newTaskCommentFlagCommand(t, "--body-file", path, "--dry-run"))
	if err != nil || req.Body != "from file" {
		t.Fatalf("req=%+v err=%v", req, err)
	}
	large := filepath.Join(t.TempDir(), "large.md")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", task.MaxMutationBodyBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := taskCommentRequest(newTaskCommentFlagCommand(t, "--body-file", large, "--dry-run")); err == nil {
		t.Fatal("oversized comment file accepted")
	}
}
