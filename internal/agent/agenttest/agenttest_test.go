package agenttest_test

import (
	"reflect"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/agent/agenttest"
)

func TestSnapshotRestore_RoundTrip(t *testing.T) {
	// Whatever the ambient registry looks like (empty here — this package has
	// no blank import of internal/agent/register), Snapshot → Reset → Restore
	// must land back on the original kind set.
	original := agenttest.Snapshot()
	t.Cleanup(func() { agenttest.Restore(original) })

	agenttest.Reset()
	agent.Register(&agenttest.StubAgent{KindStr: "claude"})
	agent.Register(&agenttest.StubAgent{KindStr: "codex"})
	baseline := agenttest.Snapshot()
	if got := agent.Kinds(); !reflect.DeepEqual(got, []string{"claude", "codex"}) {
		t.Fatalf("precondition: Kinds = %v, want [claude codex]", got)
	}

	agenttest.Reset()
	if got := agent.Kinds(); len(got) != 0 {
		t.Fatalf("after Reset: Kinds = %v, want empty", got)
	}

	agenttest.Restore(baseline)
	if got := agent.Kinds(); !reflect.DeepEqual(got, []string{"claude", "codex"}) {
		t.Errorf("after Restore: Kinds = %v, want [claude codex]", got)
	}
	for _, kind := range []string{"claude", "codex"} {
		if _, err := agent.Lookup(kind); err != nil {
			t.Errorf("Lookup(%q) after Restore: %v", kind, err)
		}
	}
}

func TestRestore_EmptyLeavesRegistryEmpty(t *testing.T) {
	original := agenttest.Snapshot()
	t.Cleanup(func() { agenttest.Restore(original) })

	agent.Register(&agenttest.StubAgent{KindStr: "claude"})
	agenttest.Restore(nil)
	if got := agent.Kinds(); len(got) != 0 {
		t.Errorf("Restore(nil): Kinds = %v, want empty", got)
	}
}
