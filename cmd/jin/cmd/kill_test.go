package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/exitcode"
)

func TestRenderActionResultJSON(t *testing.T) {
	t.Run("kill success", func(t *testing.T) {
		result := actionResult{
			Success:     true,
			ID:          "abc-123",
			Description: "my-session",
		}
		var buf bytes.Buffer
		if err := renderActionResultJSON(&buf, result); err != nil {
			t.Fatalf("renderActionResultJSON() error = %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
		}
		if parsed["success"] != true {
			t.Errorf("expected success=true, got %v", parsed["success"])
		}
		if parsed["id"] != "abc-123" {
			t.Errorf("expected id %q, got %v", "abc-123", parsed["id"])
		}
		if parsed["description"] != "my-session" {
			t.Errorf("expected name %q, got %v", "my-session", parsed["description"])
		}
	})

	t.Run("delete success", func(t *testing.T) {
		result := actionResult{
			Success:     true,
			ID:          "def-456",
			Description: "other-session",
		}
		var buf bytes.Buffer
		if err := renderActionResultJSON(&buf, result); err != nil {
			t.Fatalf("renderActionResultJSON() error = %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
		}
		if parsed["success"] != true {
			t.Errorf("expected success=true, got %v", parsed["success"])
		}
		if parsed["description"] != "other-session" {
			t.Errorf("expected name %q, got %v", "other-session", parsed["description"])
		}
	})
}

// TestDeleteCmd_FlagsRegistered verifies the worktree removal flags are registered
// on deleteCmd.
func TestDeleteCmd_FlagsRegistered(t *testing.T) {
	if deleteCmd.Flags().Lookup("worktree") == nil {
		t.Error("deleteCmd is missing the --worktree flag")
	}
	if deleteCmd.Flags().Lookup("force-worktree") == nil {
		t.Error("deleteCmd is missing the --force-worktree flag")
	}
}

// TestDeleteCmd_ForceWorktreeRequiresWorktree verifies that --force-worktree without
// --worktree is rejected locally, before any daemon call is attempted.
func TestDeleteCmd_ForceWorktreeRequiresWorktree(t *testing.T) {
	flags := deleteCmd.Flags()
	if err := flags.Set("force-worktree", "true"); err != nil {
		t.Fatalf("Set(force-worktree): %v", err)
	}
	t.Cleanup(func() {
		_ = flags.Set("force-worktree", "false")
	})

	err := deleteCmd.RunE(deleteCmd, []string{"some-session"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires --worktree") {
		t.Errorf("error message = %q, want to contain %q", err.Error(), "requires --worktree")
	}

	var exitErr *exitcode.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error is not *exitcode.ExitError: %v", err)
	}
	if exitErr.Code != exitcode.GeneralError {
		t.Errorf("exit code = %d, want %d", exitErr.Code, exitcode.GeneralError)
	}
}
