package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/takaaki-s/jindaiko/internal/transcript"
)

func TestRenderOutputJSON(t *testing.T) {
	t.Run("outputs single message as JSON", func(t *testing.T) {
		msg := &transcript.Message{
			Type:      "assistant",
			Content:   "Hello, world!",
			Timestamp: "2025-01-01T00:00:00Z",
		}
		var buf bytes.Buffer
		if err := renderOutputJSON(&buf, msg); err != nil {
			t.Fatalf("renderOutputJSON returned error: %v", err)
		}

		var parsed transcript.Message
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("failed to parse JSON: %v", err)
		}
		if parsed.Type != "assistant" {
			t.Errorf("type = %q, want %q", parsed.Type, "assistant")
		}
		if parsed.Content != "Hello, world!" {
			t.Errorf("content = %q, want %q", parsed.Content, "Hello, world!")
		}
	})

	t.Run("outputs message list as JSON", func(t *testing.T) {
		msgs := []transcript.Message{
			{Type: "user", Content: "Hi", Timestamp: "2025-01-01T00:00:00Z"},
			{Type: "assistant", Content: "Hello!", Timestamp: "2025-01-01T00:00:01Z"},
		}
		var buf bytes.Buffer
		if err := renderOutputJSON(&buf, msgs); err != nil {
			t.Fatalf("renderOutputJSON returned error: %v", err)
		}

		var parsed []transcript.Message
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("failed to parse JSON: %v", err)
		}
		if len(parsed) != 2 {
			t.Fatalf("got %d messages, want 2", len(parsed))
		}
		if parsed[0].Type != "user" {
			t.Errorf("first message type = %q, want %q", parsed[0].Type, "user")
		}
		if parsed[1].Type != "assistant" {
			t.Errorf("second message type = %q, want %q", parsed[1].Type, "assistant")
		}
	})

	t.Run("outputs empty list as JSON", func(t *testing.T) {
		msgs := []transcript.Message{}
		var buf bytes.Buffer
		if err := renderOutputJSON(&buf, msgs); err != nil {
			t.Fatalf("renderOutputJSON returned error: %v", err)
		}

		var parsed []transcript.Message
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("failed to parse JSON: %v", err)
		}
		if len(parsed) != 0 {
			t.Fatalf("got %d messages, want 0", len(parsed))
		}
	})
}
