package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/transcript"
)

func TestRenderResultText_Empty(t *testing.T) {
	var buf bytes.Buffer
	err := renderResultText(&buf, "my-sess", &daemon.ResultResponse{Entries: []transcript.Entry{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "no entries") {
		t.Errorf("expected 'no entries', got %q", buf.String())
	}
}

func TestRenderResultText_BlocksAndTruncationNote(t *testing.T) {
	resp := &daemon.ResultResponse{
		SessionID: "s1",
		Truncated: true,
		Entries: []transcript.Entry{
			{
				Timestamp: "2026-04-26T10:00:00Z",
				Type:      "assistant",
				Blocks: []transcript.Block{
					{Kind: "text", Text: "hello\nignored"},
					{Kind: "tool_use", ToolName: "Bash", ToolUseID: "abcd1234extra", Input: json.RawMessage(`{"command":"echo hi"}`)},
				},
			},
			{
				Timestamp: "2026-04-26T10:00:01Z",
				Type:      "user",
				Blocks: []transcript.Block{
					{Kind: "tool_result", ToolUseID: "abcd1234extra", Output: "hi", IsError: false},
					{Kind: "tool_result", ToolUseID: "ef567890extra", Output: "boom", IsError: true},
				},
			},
		},
	}
	var buf bytes.Buffer
	if err := renderResultText(&buf, "my-sess", resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	wants := []string{
		"[2026-04-26T10:00:00Z] assistant",
		"text: hello",
		"tool_use Bash [abcd1234]",
		"echo hi",
		"tool_result [abcd1234] hi",
		"tool_result ERROR [ef567890] boom",
		"truncated to last 2 entries",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\nfull:\n%s", w, got)
		}
	}
}

func TestFirstLineSummary(t *testing.T) {
	cases := []struct {
		in     string
		maxLen int
		want   string
	}{
		{"hello", 50, "hello"},
		{"hello\nworld", 50, "hello …"},
		{"abcdefghij", 5, "abcde…"},
		{"  spaces  ", 50, "spaces"},
		{"line1\r\nline2", 50, "line1 …"},
	}
	for _, tc := range cases {
		got := firstLineSummary(tc.in, tc.maxLen)
		if got != tc.want {
			t.Errorf("firstLineSummary(%q,%d) = %q, want %q", tc.in, tc.maxLen, got, tc.want)
		}
	}
}

func TestCompactInputSummary(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := compactInputSummary(nil); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
	t.Run("normalizes JSON", func(t *testing.T) {
		got := compactInputSummary(json.RawMessage(`{ "command" : "echo hi" }`))
		// Re-marshaled JSON drops extra whitespace.
		if !strings.Contains(got, `"command":"echo hi"`) {
			t.Errorf("expected compact JSON, got %q", got)
		}
	})
}

func TestShortID(t *testing.T) {
	if got := shortID("abc"); got != "abc" {
		t.Errorf("expected passthrough for short id, got %q", got)
	}
	if got := shortID("abcdefghijkl"); got != "abcdefgh" {
		t.Errorf("expected truncated to 8, got %q", got)
	}
}
