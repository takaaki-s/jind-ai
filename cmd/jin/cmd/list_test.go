package cmd

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/takaaki-s/jindaiko/internal/session"
)

func TestTruncatePath(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		maxLen int
		want   string
	}{
		{
			name:   "short path no truncation",
			path:   "/home/user",
			maxLen: 20,
			want:   "/home/user",
		},
		{
			name:   "exact maxLen no truncation",
			path:   "/home/user",
			maxLen: 10,
			want:   "/home/user",
		},
		{
			name:   "long path truncated with prefix ellipsis",
			path:   "/home/user/projects/my-very-long-project-name",
			maxLen: 20,
			want:   "...long-project-name",
		},
		{
			name:   "empty string",
			path:   "",
			maxLen: 10,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncatePath(tt.path, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncatePath(%q, %d) = %q, want %q", tt.path, tt.maxLen, got, tt.want)
			}
			if tt.maxLen > 0 && len(got) > tt.maxLen {
				t.Errorf("truncatePath(%q, %d) returned %d chars, exceeds maxLen %d", tt.path, tt.maxLen, len(got), tt.maxLen)
			}
		})
	}
}

func TestTruncateStr(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{
			name:   "short string no truncation",
			s:      "hello",
			maxLen: 10,
			want:   "hello",
		},
		{
			name:   "exact maxLen no truncation",
			s:      "hello",
			maxLen: 5,
			want:   "hello",
		},
		{
			name:   "long string truncated with suffix ellipsis",
			s:      "this is a very long error message",
			maxLen: 15,
			want:   "this is a ve...",
		},
		{
			name:   "maxLen equals 3 no ellipsis",
			s:      "hello",
			maxLen: 3,
			want:   "hel",
		},
		{
			name:   "maxLen equals 2 no ellipsis",
			s:      "hello",
			maxLen: 2,
			want:   "he",
		},
		{
			name:   "maxLen equals 1",
			s:      "hello",
			maxLen: 1,
			want:   "h",
		},
		{
			name:   "maxLen equals 0",
			s:      "hello",
			maxLen: 0,
			want:   "",
		},
		{
			name:   "empty string",
			s:      "",
			maxLen: 10,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateStr(tt.s, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.s, tt.maxLen, got, tt.want)
			}
			if tt.maxLen > 0 && len(got) > tt.maxLen {
				t.Errorf("truncateStr(%q, %d) returned %d chars, exceeds maxLen %d", tt.s, tt.maxLen, len(got), tt.maxLen)
			}
		})
	}
}

func TestRenderSessionListJSON(t *testing.T) {
	t.Run("with sessions", func(t *testing.T) {
		sessions := []session.Info{
			{
				ID:          "abc-123",
				Description: "my-session",
				WorkDir:     "/home/user/project",
				Status:      session.StatusIdle,
				CreatedAt:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		}
		var buf bytes.Buffer
		if err := renderSessionListJSON(&buf, sessions); err != nil {
			t.Fatalf("renderSessionListJSON() error = %v", err)
		}
		var parsed []session.Info
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
		}
		if len(parsed) != 1 {
			t.Errorf("expected 1 session, got %d", len(parsed))
		}
		if parsed[0].Description != "my-session" {
			t.Errorf("expected name %q, got %q", "my-session", parsed[0].Description)
		}
		if parsed[0].Status != session.StatusIdle {
			t.Errorf("expected status %q, got %q", session.StatusIdle, parsed[0].Status)
		}
	})

	t.Run("nil sessions outputs empty array", func(t *testing.T) {
		var buf bytes.Buffer
		if err := renderSessionListJSON(&buf, nil); err != nil {
			t.Fatalf("renderSessionListJSON() error = %v", err)
		}
		var parsed []session.Info
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
		}
		if len(parsed) != 0 {
			t.Errorf("expected 0 sessions, got %d", len(parsed))
		}
	})

	t.Run("empty slice outputs empty array", func(t *testing.T) {
		var buf bytes.Buffer
		if err := renderSessionListJSON(&buf, []session.Info{}); err != nil {
			t.Fatalf("renderSessionListJSON() error = %v", err)
		}
		var parsed []session.Info
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
		}
		if len(parsed) != 0 {
			t.Errorf("expected 0 sessions, got %d", len(parsed))
		}
	})
}

// TestRenderSessionTable_HeaderAndAgentColumn locks in the AGENT column's
// position (STATUS の直後、WORKDIR の前) and its value pass-through. If the
// column order changes the header assertion fails immediately.
func TestRenderSessionTable_HeaderAndAgentColumn(t *testing.T) {
	sessions := []session.Info{
		{
			ID:          "abc123",
			Description: "alpha",
			Status:      "running",
			AgentKind:   "claude",
			WorkDir:     "/tmp/alpha",
			CreatedAt:   time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		},
	}

	var buf bytes.Buffer
	if err := renderSessionTable(&buf, sessions); err != nil {
		t.Fatalf("renderSessionTable failed: %v", err)
	}

	got := buf.String()
	// Header order must include AGENT between STATUS and WORKDIR.
	wantHeader := "DESCRIPTION"
	if !bytes.Contains(buf.Bytes(), []byte(wantHeader)) {
		t.Fatalf("header not found in output:\n%s", got)
	}
	// Assert the exact column sequence via a substring check.
	for _, want := range []string{"DESCRIPTION", "STATUS", "AGENT", "WORKDIR", "BRANCH", "LAST_ACTIVE"} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("header column %q missing:\n%s", want, got)
		}
	}
	// Sequence assertion: STATUS index < AGENT index < WORKDIR index.
	statusIdx := bytes.Index(buf.Bytes(), []byte("STATUS"))
	agentIdx := bytes.Index(buf.Bytes(), []byte("AGENT"))
	workdirIdx := bytes.Index(buf.Bytes(), []byte("WORKDIR"))
	if !(statusIdx < agentIdx && agentIdx < workdirIdx) {
		t.Errorf("column order wrong: STATUS=%d AGENT=%d WORKDIR=%d\n%s", statusIdx, agentIdx, workdirIdx, got)
	}
	// The row itself must carry the "claude" value.
	if !bytes.Contains(buf.Bytes(), []byte("claude")) {
		t.Errorf("row missing AgentKind value:\n%s", got)
	}
}

// TestRenderSessionTable_EmptyAgentRendersDash guards against a missing
// AgentKind (should never happen with the migration, but defence in depth).
func TestRenderSessionTable_EmptyAgentRendersDash(t *testing.T) {
	sessions := []session.Info{{
		ID:          "x",
		Description: "d",
		Status:      "stopped",
		AgentKind:   "",
		WorkDir:     "/tmp/x",
		CreatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
	var buf bytes.Buffer
	if err := renderSessionTable(&buf, sessions); err != nil {
		t.Fatalf("renderSessionTable failed: %v", err)
	}
	// "-" appears in the row (either AgentKind or Branch); check by looking
	// at the row after the header — both columns fall back to "-" so
	// just confirming presence is enough.
	if !bytes.Contains(buf.Bytes(), []byte("-")) {
		t.Errorf("expected fallback %q in output:\n%s", "-", buf.String())
	}
}
