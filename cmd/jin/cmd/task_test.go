package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/task"
)

func TestTaskCommandsAreRegistered(t *testing.T) {
	got, _, err := rootCmd.Find([]string{"task", "execution", "add"})
	if err != nil {
		t.Fatal(err)
	}
	if got != taskExecutionAddCmd || taskCreateCmd.Args == nil || taskInfoCmd.Args == nil {
		t.Fatal("task command tree is not fully registered")
	}
	if taskCreateCmd.Flags().Lookup("title") == nil || taskExecutionAddCmd.Flags().Lookup("session") == nil {
		t.Fatal("required task flags are not registered")
	}
}

func TestResolveTaskFromList(t *testing.T) {
	infos := []task.Info{
		{ID: "abcdef00-0000", Title: "Fix flaky build"},
		{ID: "12345678-0000", Title: "Document task model"},
	}
	for _, selector := range []string{"abcdef00-0000", "abcdef", "Fix flaky build", "flaky"} {
		got, err := resolveTaskFromList(infos, selector)
		if err != nil {
			t.Fatalf("selector %q: %v", selector, err)
		}
		if got.ID != infos[0].ID {
			t.Fatalf("selector %q got %s", selector, got.ID)
		}
	}
	if _, err := resolveTaskFromList(infos, "missing"); err == nil {
		t.Fatal("missing selector succeeded")
	}
}

func TestRenderTaskListJSON_UsesEmptyArrayForNil(t *testing.T) {
	var buf bytes.Buffer
	if err := renderTaskListJSON(&buf, nil); err != nil {
		t.Fatal(err)
	}
	var got []task.Info
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("decoded = %#v", got)
	}
}

func TestRenderTaskInfoText_ShowsMissingExecution(t *testing.T) {
	info := &task.Info{
		ID: "task-1", Title: "Keep history", Source: task.Source{Kind: "manual"},
		Executions: []task.ExecutionInfo{{
			Execution:      task.Execution{ID: "exec-1", Sequence: 1, SessionID: "gone"},
			ReferenceState: task.ReferenceMissing,
		}},
	}
	var buf bytes.Buffer
	renderTaskInfoText(&buf, info)
	if !strings.Contains(buf.String(), "session=gone  missing") {
		t.Fatalf("output = %q", buf.String())
	}
}
