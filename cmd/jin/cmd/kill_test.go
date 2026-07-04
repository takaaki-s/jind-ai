package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/takaaki-s/honjin/internal/exitcode"
	"github.com/takaaki-s/honjin/internal/session"
)

func TestRenderActionResultJSON(t *testing.T) {
	t.Run("kill success", func(t *testing.T) {
		result := actionResult{
			Success: true,
			ID:      "abc-123",
			Name:    "my-session",
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
		if parsed["name"] != "my-session" {
			t.Errorf("expected name %q, got %v", "my-session", parsed["name"])
		}
	})

	t.Run("delete success", func(t *testing.T) {
		result := actionResult{
			Success: true,
			ID:      "def-456",
			Name:    "other-session",
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
		if parsed["name"] != "other-session" {
			t.Errorf("expected name %q, got %v", "other-session", parsed["name"])
		}
	})
}

func TestResolveSession_ReturnsHostID(t *testing.T) {
	// resolveSession should return hostID from the session info.
	// We test the logic by calling resolveSessionFromList directly.

	tests := []struct {
		name       string
		sessions   []session.Info
		nameOrID   string
		wantID     string
		wantName   string
		wantHostID string
		wantErr    bool
	}{
		{
			name: "local session by name",
			sessions: []session.Info{
				{ID: "aaa", Name: "my-session", HostID: "local"},
			},
			nameOrID:   "my-session",
			wantID:     "aaa",
			wantName:   "my-session",
			wantHostID: "local",
		},
		{
			name: "remote session by name",
			sessions: []session.Info{
				{ID: "bbb", Name: "remote-sess", HostID: "ec2-prod"},
			},
			nameOrID:   "remote-sess",
			wantID:     "bbb",
			wantName:   "remote-sess",
			wantHostID: "ec2-prod",
		},
		{
			name: "session by ID",
			sessions: []session.Info{
				{ID: "ccc-123", Name: "some-session", HostID: "docker-dev"},
			},
			nameOrID:   "ccc-123",
			wantID:     "ccc-123",
			wantName:   "some-session",
			wantHostID: "docker-dev",
		},
		{
			name: "session with empty hostID",
			sessions: []session.Info{
				{ID: "ddd", Name: "no-host", HostID: ""},
			},
			nameOrID:   "no-host",
			wantID:     "ddd",
			wantName:   "no-host",
			wantHostID: "",
		},
		{
			name:     "not found",
			sessions: []session.Info{},
			nameOrID: "nonexistent",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, name, hostID, err := resolveSessionFromList(tt.sessions, tt.nameOrID)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id != tt.wantID {
				t.Errorf("id: got %q, want %q", id, tt.wantID)
			}
			if name != tt.wantName {
				t.Errorf("name: got %q, want %q", name, tt.wantName)
			}
			if hostID != tt.wantHostID {
				t.Errorf("hostID: got %q, want %q", hostID, tt.wantHostID)
			}
		})
	}
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
