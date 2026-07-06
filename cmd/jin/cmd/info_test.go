package cmd

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/takaaki-s/jindaiko/internal/session"
)

func TestRenderSessionInfoJSON(t *testing.T) {
	t.Run("outputs full session info as JSON", func(t *testing.T) {
		info := &session.Info{
			ID:              "abc-123",
			Description:     "my-session",
			WorkDir:         "/home/user/project",
			Status:          session.StatusIdle,
			CreatedAt:       time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			CurrentWorkDir:  "/home/user/project/src",
			CurrentBranch:   "feat/new-feature",
			LastUserMessage: "Fix the bug",
		}
		var buf bytes.Buffer
		if err := renderSessionInfoJSON(&buf, info); err != nil {
			t.Fatalf("renderSessionInfoJSON() error = %v", err)
		}
		var parsed session.Info
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
		}
		if parsed.ID != "abc-123" {
			t.Errorf("expected id %q, got %q", "abc-123", parsed.ID)
		}
		if parsed.Description != "my-session" {
			t.Errorf("expected name %q, got %q", "my-session", parsed.Description)
		}
		if parsed.Status != session.StatusIdle {
			t.Errorf("expected status %q, got %q", session.StatusIdle, parsed.Status)
		}
		if parsed.CurrentBranch != "feat/new-feature" {
			t.Errorf("expected branch %q, got %q", "feat/new-feature", parsed.CurrentBranch)
		}
		if parsed.LastUserMessage != "Fix the bug" {
			t.Errorf("expected last_user_message %q, got %q", "Fix the bug", parsed.LastUserMessage)
		}
	})
}

func TestRenderSessionInfoText(t *testing.T) {
	t.Run("outputs key-value format", func(t *testing.T) {
		info := &session.Info{
			ID:             "abc-123",
			Description:    "my-session",
			WorkDir:        "/home/user/project",
			Status:         session.StatusIdle,
			CreatedAt:      time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			CurrentBranch:  "main",
			CurrentWorkDir: "/home/user/project",
		}
		var buf bytes.Buffer
		renderSessionInfoText(&buf, info)
		output := buf.String()
		if !bytes.Contains([]byte(output), []byte("my-session")) {
			t.Errorf("expected output to contain session name, got:\n%s", output)
		}
		if !bytes.Contains([]byte(output), []byte("idle")) {
			t.Errorf("expected output to contain status, got:\n%s", output)
		}
		if !bytes.Contains([]byte(output), []byte("main")) {
			t.Errorf("expected output to contain branch, got:\n%s", output)
		}
	})
}
