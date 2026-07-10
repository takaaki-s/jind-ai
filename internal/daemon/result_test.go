package daemon

import (
	"testing"

	"github.com/takaaki-s/jind-ai/internal/transcript"
)

func TestFilterResultEntries_Passthrough(t *testing.T) {
	entries := []transcript.Entry{
		{Type: "assistant", Blocks: []transcript.Block{{Kind: "text", Text: "hi"}}},
		{Type: "user", Blocks: []transcript.Block{{Kind: "text", Text: "ok"}}},
	}
	got := filterResultEntries(entries, "", false)
	if len(got) != len(entries) {
		t.Errorf("expected passthrough, got %d entries (want %d)", len(got), len(entries))
	}
}

func TestFilterResultEntries_ByTool(t *testing.T) {
	entries := []transcript.Entry{
		{Type: "assistant", Blocks: []transcript.Block{
			{Kind: "tool_use", ToolName: "Bash", ToolUseID: "tu_1"},
		}},
		{Type: "user", Blocks: []transcript.Block{
			{Kind: "tool_result", ToolUseID: "tu_1", Output: "out"},
		}},
		{Type: "assistant", Blocks: []transcript.Block{
			{Kind: "tool_use", ToolName: "Read", ToolUseID: "tu_2"},
		}},
		{Type: "user", Blocks: []transcript.Block{
			{Kind: "tool_result", ToolUseID: "tu_2", Output: "x"},
		}},
		{Type: "assistant", Blocks: []transcript.Block{
			{Kind: "text", Text: "all done"},
		}},
	}
	got := filterResultEntries(entries, "Bash", false)
	// Should keep tool_use Bash + its tool_result (matched by use_id)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].Blocks[0].ToolName != "Bash" {
		t.Errorf("entry 0 wrong: %+v", got[0])
	}
	if got[1].Blocks[0].ToolUseID != "tu_1" {
		t.Errorf("entry 1 wrong: %+v", got[1])
	}
}

func TestFilterResultEntries_ErrorsOnly(t *testing.T) {
	entries := []transcript.Entry{
		{Type: "user", Blocks: []transcript.Block{
			{Kind: "tool_result", ToolUseID: "tu_1", Output: "ok", IsError: false},
		}},
		{Type: "user", Blocks: []transcript.Block{
			{Kind: "tool_result", ToolUseID: "tu_2", Output: "boom", IsError: true},
		}},
		{Type: "assistant", Blocks: []transcript.Block{
			{Kind: "tool_use", ToolName: "X", ToolUseID: "tu_3"},
		}},
	}
	got := filterResultEntries(entries, "", true)
	// Only the IsError entry should remain. Assistant tool_use is not an error.
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if !got[0].Blocks[0].IsError {
		t.Errorf("expected error entry, got %+v", got[0])
	}
}

func TestFilterResultEntries_ToolAndErrorsOnly(t *testing.T) {
	entries := []transcript.Entry{
		{Type: "assistant", Blocks: []transcript.Block{
			{Kind: "tool_use", ToolName: "Bash", ToolUseID: "tu_1"},
		}},
		{Type: "user", Blocks: []transcript.Block{
			{Kind: "tool_result", ToolUseID: "tu_1", Output: "ok"},
		}},
		{Type: "assistant", Blocks: []transcript.Block{
			{Kind: "tool_use", ToolName: "Bash", ToolUseID: "tu_2"},
		}},
		{Type: "user", Blocks: []transcript.Block{
			{Kind: "tool_result", ToolUseID: "tu_2", Output: "boom", IsError: true},
		}},
	}
	got := filterResultEntries(entries, "Bash", true)
	// Only the failed Bash result should remain. tool_use is excluded under errorsOnly.
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if !got[0].Blocks[0].IsError || got[0].Blocks[0].ToolUseID != "tu_2" {
		t.Errorf("unexpected entry: %+v", got[0])
	}
}
