package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/agent/agenttest"
)

// TestHandleNew_UnknownAgentKind exercises the validation branch of
// handleNew without spinning a full Server. Only the registry is touched;
// unknown kinds must produce Response.Success=false with a message that
// names the requested kind and lists the available ones.
//
// The test uses Server{} directly: handleNew's unknown-kind branch returns
// before it touches configMgr / manager, so the zero-value Server is safe.
func TestHandleNew_UnknownAgentKind(t *testing.T) {
	t.Cleanup(agenttest.Reset)
	agenttest.Reset()

	agent.Register(&agenttest.StubAgent{KindStr: "claude"})

	s := &Server{}
	data, _ := json.Marshal(NewRequest{AgentKind: "codex", WorkDir: "/tmp"})
	resp := s.handleNew(data)

	if resp.Success {
		t.Fatalf("expected Success=false, got success with data=%s", resp.Data)
	}
	if !strings.Contains(resp.Error, "unknown agent kind: codex") {
		t.Errorf("Error = %q, want to contain 'unknown agent kind: codex'", resp.Error)
	}
	if !strings.Contains(resp.Error, "claude") {
		t.Errorf("Error = %q, want to list 'claude' as available", resp.Error)
	}
}
