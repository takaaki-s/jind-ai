package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
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
	if got, _, err := rootCmd.Find([]string{"task", "new"}); err != nil || got != taskNewCmd {
		t.Fatal("task new command is not registered")
	}
}

func newTaskNewFlagCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	addTaskNewFlags(cmd)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestTaskNewRequestMapsEveryFlag(t *testing.T) {
	cmd := newTaskNewFlagCommand(t,
		"--prompt", "do it", "--repo", "/repo", "--title", "Title", "--workdir", "service",
		"--base", "main", "--agent", "codex", "--model", "gpt", "--fleet", "backend",
		"--no-hook", "--idempotency-key", "request-1",
	)
	got, err := taskNewRequest(cmd)
	if err != nil {
		t.Fatal(err)
	}
	want := daemon.TaskNewRequest{
		IdempotencyKey: "request-1", Title: "Title", Prompt: "do it", Repo: "/repo",
		RelativeWorkDir: "service", RequestedBase: "main", AgentKind: "codex", Model: "gpt",
		Fleet: "backend", NoHook: true,
	}
	if got != want {
		t.Fatalf("request = %+v, want %+v", got, want)
	}
}

func TestTaskNewRequestReadsBoundedPromptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(path, []byte("from file"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := taskNewRequest(newTaskNewFlagCommand(t, "--prompt-file", path, "--repo", "/repo"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "from file" || got.IdempotencyKey == "" {
		t.Fatalf("request = %+v", got)
	}

	tooLarge := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(tooLarge, bytes.Repeat([]byte("x"), task.MaxPromptBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := taskNewRequest(newTaskNewFlagCommand(t, "--prompt-file", tooLarge, "--repo", "/repo")); err == nil {
		t.Fatal("oversized prompt file was accepted")
	}
}

func TestTaskNewRequestRequiresExactlyOnePromptSource(t *testing.T) {
	for _, args := range [][]string{
		{"--repo", "/repo"},
		{"--repo", "/repo", "--prompt", "x", "--prompt-file", "prompt.txt"},
	} {
		if _, err := taskNewRequest(newTaskNewFlagCommand(t, args...)); err == nil {
			t.Fatalf("accepted args: %v", args)
		}
	}
}

func TestRenderTaskNewTextShowsRetryAndProgressIdentities(t *testing.T) {
	result := &daemon.TaskNewResponse{
		Task: task.Info{ID: "task-1", Title: "Title"},
		Execution: task.ExecutionInfo{Execution: task.Execution{ID: "exec-1", SessionID: "session-1", Run: &task.Run{
			Phase: task.ExecutionProvisioning, WorktreeName: "jin-abcd", WorktreeBranch: "jin/abcd", IdempotencyKey: "request-1",
		}}},
		Session: session.Info{ID: "session-1", Status: session.StatusCreating},
	}
	var buf bytes.Buffer
	renderTaskNewText(&buf, result)
	for _, want := range []string{"task-1", "exec-1", "session-1", "jin/abcd", "request-1", "jin task info task-1"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("output %q does not contain %q", buf.String(), want)
		}
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
