package agent_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/takaaki-s/jindaiko/internal/agent"
	"github.com/takaaki-s/jindaiko/internal/agent/agenttest"
)

func TestRegisterAndLookup(t *testing.T) {
	t.Cleanup(agenttest.Reset)
	agenttest.Reset()

	stub := &agenttest.StubAgent{KindStr: "claude"}
	agent.Register(stub)

	got, err := agent.Lookup("claude")
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if got.Kind() != "claude" {
		t.Errorf("Lookup returned kind %q, want claude", got.Kind())
	}
}

func TestLookupUnknownReturnsAvailableList(t *testing.T) {
	t.Cleanup(agenttest.Reset)
	agenttest.Reset()

	agent.Register(&agenttest.StubAgent{KindStr: "claude"})
	agent.Register(&agenttest.StubAgent{KindStr: "codex"})

	_, err := agent.Lookup("aider")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown agent kind: aider") {
		t.Errorf("error message %q missing unknown-kind prefix", msg)
	}
	if !strings.Contains(msg, "claude") || !strings.Contains(msg, "codex") {
		t.Errorf("error message %q should list available kinds", msg)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	t.Cleanup(agenttest.Reset)
	agenttest.Reset()

	agent.Register(&agenttest.StubAgent{KindStr: "claude"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate registration, got nil")
		}
		// Stringify defensively: a future refactor might panic(err) instead
		// of panic(string), and the substring assertion should still work.
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "duplicate kind") {
			t.Errorf("panic message = %q, want mention of duplicate kind", msg)
		}
	}()
	agent.Register(&agenttest.StubAgent{KindStr: "claude"})
}

func TestRegisterNilPanics(t *testing.T) {
	t.Cleanup(agenttest.Reset)
	agenttest.Reset()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil Agent, got nil")
		}
	}()
	agent.Register(nil)
}

func TestKindsIsSorted(t *testing.T) {
	t.Cleanup(agenttest.Reset)
	agenttest.Reset()

	agent.Register(&agenttest.StubAgent{KindStr: "codex"})
	agent.Register(&agenttest.StubAgent{KindStr: "aider"})
	agent.Register(&agenttest.StubAgent{KindStr: "claude"})

	got := agent.Kinds()
	want := []string{"aider", "claude", "codex"}
	if len(got) != len(want) {
		t.Fatalf("Kinds len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Kinds[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
