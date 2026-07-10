package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/exitcode"
	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestRenderSendResultJSON(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		result := sendResult{Success: true, Session: "my-session"}
		var buf bytes.Buffer
		if err := renderSendResultJSON(&buf, result); err != nil {
			t.Fatalf("renderSendResultJSON() error = %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
		}
		if parsed["success"] != true {
			t.Errorf("expected success=true, got %v", parsed["success"])
		}
		if parsed["session"] != "my-session" {
			t.Errorf("expected session=%q, got %v", "my-session", parsed["session"])
		}
	})

	t.Run("verified fields present when set", func(t *testing.T) {
		result := sendResult{Success: true, Session: "s", Verified: true, Status: "thinking"}
		var buf bytes.Buffer
		if err := renderSendResultJSON(&buf, result); err != nil {
			t.Fatalf("renderSendResultJSON() error = %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v", err)
		}
		if parsed["verified"] != true {
			t.Errorf("expected verified=true, got %v", parsed["verified"])
		}
		if parsed["status"] != "thinking" {
			t.Errorf("expected status=%q, got %v", "thinking", parsed["status"])
		}
	})

	t.Run("verified fields omitted when unset", func(t *testing.T) {
		result := sendResult{Success: true, Session: "s"}
		var buf bytes.Buffer
		if err := renderSendResultJSON(&buf, result); err != nil {
			t.Fatalf("renderSendResultJSON() error = %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("output is not valid JSON: %v", err)
		}
		if _, ok := parsed["verified"]; ok {
			t.Errorf("verified should be omitted when false, got %v", parsed["verified"])
		}
		if _, ok := parsed["status"]; ok {
			t.Errorf("status should be omitted when empty, got %v", parsed["status"])
		}
	})
}

func TestIsPromptAcceptedStatus(t *testing.T) {
	cases := []struct {
		s    session.Status
		want bool
	}{
		{session.StatusIdle, false},
		{session.StatusRunning, true},
		{session.StatusThinking, true},
		{session.StatusPermission, true},
		{session.StatusStopped, false},
		{session.StatusCreating, false},
	}
	for _, tc := range cases {
		if got := isPromptAcceptedStatus(tc.s); got != tc.want {
			t.Errorf("isPromptAcceptedStatus(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// fakeGetter returns a scripted sequence of statuses. Once the script runs
// out, the last status is repeated indefinitely.
type fakeGetter struct {
	statuses []session.Status
	calls    int
	err      error
}

func (f *fakeGetter) Get(id string) (*session.Info, error) {
	if f.err != nil {
		return nil, f.err
	}
	idx := f.calls
	if idx >= len(f.statuses) {
		idx = len(f.statuses) - 1
	}
	f.calls++
	return &session.Info{ID: id, Status: f.statuses[idx]}, nil
}

func TestPollSendAccepted(t *testing.T) {
	// Use a very short poll interval so tests finish quickly.
	prevInterval := sendPollInterval
	sendPollInterval = 5 * time.Millisecond
	defer func() { sendPollInterval = prevInterval }()

	t.Run("immediate running is accepted on first poll", func(t *testing.T) {
		g := &fakeGetter{statuses: []session.Status{session.StatusRunning}}
		info, err := pollSendAccepted(context.Background(), g, "id", 200*time.Millisecond)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.Status != session.StatusRunning {
			t.Errorf("Status = %q, want running", info.Status)
		}
		if g.calls != 1 {
			t.Errorf("Get calls = %d, want 1 (fast path)", g.calls)
		}
	})

	t.Run("transition idle -> thinking is accepted", func(t *testing.T) {
		g := &fakeGetter{statuses: []session.Status{
			session.StatusIdle, session.StatusIdle, session.StatusThinking,
		}}
		info, err := pollSendAccepted(context.Background(), g, "id", 500*time.Millisecond)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.Status != session.StatusThinking {
			t.Errorf("Status = %q, want thinking", info.Status)
		}
	})

	t.Run("permission also counts as accepted", func(t *testing.T) {
		g := &fakeGetter{statuses: []session.Status{
			session.StatusIdle, session.StatusPermission,
		}}
		info, err := pollSendAccepted(context.Background(), g, "500ms", 500*time.Millisecond)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.Status != session.StatusPermission {
			t.Errorf("Status = %q, want permission", info.Status)
		}
	})

	t.Run("stopped short-circuits with an error", func(t *testing.T) {
		g := &fakeGetter{statuses: []session.Status{session.StatusStopped}}
		_, err := pollSendAccepted(context.Background(), g, "id", 500*time.Millisecond)
		if err == nil {
			t.Fatalf("expected error for stopped session, got nil")
		}
		// Not a timeout error — should be a normal error, not exitcode.Timeout.
		var exitErr *exitcode.ExitError
		if errors.As(err, &exitErr) && exitErr.Code == exitcode.Timeout {
			t.Errorf("expected non-timeout error, got timeout: %v", err)
		}
	})

	t.Run("timeout when status stays idle", func(t *testing.T) {
		g := &fakeGetter{statuses: []session.Status{session.StatusIdle}}
		_, err := pollSendAccepted(context.Background(), g, "id", 30*time.Millisecond)
		if err == nil {
			t.Fatalf("expected timeout error, got nil")
		}
		var exitErr *exitcode.ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != exitcode.Timeout {
			t.Errorf("expected exitcode.Timeout, got %v", err)
		}
	})

	t.Run("context cancellation aborts polling", func(t *testing.T) {
		g := &fakeGetter{statuses: []session.Status{session.StatusIdle}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := pollSendAccepted(ctx, g, "id", 500*time.Millisecond)
		if err == nil {
			t.Fatalf("expected error from cancelled ctx, got nil")
		}
		// Should not be a timeout — the cancellation reached us first.
		var exitErr *exitcode.ExitError
		if errors.As(err, &exitErr) && exitErr.Code == exitcode.Timeout {
			t.Errorf("expected interrupt, got timeout: %v", err)
		}
	})

	t.Run("Get error is propagated", func(t *testing.T) {
		g := &fakeGetter{statuses: []session.Status{session.StatusIdle}, err: errors.New("boom")}
		_, err := pollSendAccepted(context.Background(), g, "id", 500*time.Millisecond)
		if err == nil || err.Error() != "boom" {
			t.Errorf("expected boom error, got %v", err)
		}
	})
}
