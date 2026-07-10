package claude

import (
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/agent"
)

func TestSpawnCommand_FreshSessionUsesSessionIDFlag(t *testing.T) {
	a := New()
	plan := a.SpawnCommand(agent.SpawnOptions{
		AgentSessionID:      "fresh-uuid",
		AgentSessionStarted: false,
	})
	if !strings.Contains(plan.Command, "--session-id fresh-uuid") {
		t.Errorf("Command = %q, want to contain --session-id fresh-uuid", plan.Command)
	}
	if strings.Contains(plan.Command, "--resume") {
		t.Errorf("Command = %q, must not contain --resume on a fresh spawn", plan.Command)
	}
}

func TestSpawnCommand_StartedSessionUsesResumeFlag(t *testing.T) {
	a := New()
	plan := a.SpawnCommand(agent.SpawnOptions{
		AgentSessionID:      "old-uuid",
		AgentSessionStarted: true,
	})
	if !strings.Contains(plan.Command, "--resume old-uuid") {
		t.Errorf("Command = %q, want to contain --resume old-uuid", plan.Command)
	}
	if strings.Contains(plan.Command, "--session-id") {
		t.Errorf("Command = %q, must not carry --session-id when resuming", plan.Command)
	}
}

func TestSpawnCommand_EmptyAgentSessionIDOmitsBothFlags(t *testing.T) {
	a := New()
	plan := a.SpawnCommand(agent.SpawnOptions{AgentSessionID: ""})
	if strings.Contains(plan.Command, "--session-id") || strings.Contains(plan.Command, "--resume") {
		t.Errorf("Command = %q, should be plain `claude` when no AgentSessionID is given", plan.Command)
	}
}

func TestSpawnCommand_HooksPathAddsSettingsFlag(t *testing.T) {
	a := New()
	a.hooksPath = "/tmp/hooks-settings.json"
	plan := a.SpawnCommand(agent.SpawnOptions{})
	if !strings.Contains(plan.Command, "--settings /tmp/hooks-settings.json") {
		t.Errorf("Command = %q, want --settings /tmp/hooks-settings.json", plan.Command)
	}
}

func TestSpawnCommand_UnsetsCCInheritanceEnv(t *testing.T) {
	a := New()
	plan := a.SpawnCommand(agent.SpawnOptions{})

	// Every Claude Code var that could leak in from a CC-parent env must
	// be unset — see spawn.go for the failure mode when
	// CLAUDE_CODE_CHILD_SESSION survives (CC 2.x refuses to persist a
	// transcript, silently breaking Layer C).
	required := map[string]bool{
		"CLAUDECODE":                false,
		"CLAUDE_CODE_CHILD_SESSION": false,
		"CLAUDE_CODE_SESSION_ID":    false,
		"CLAUDE_CODE_ENTRYPOINT":    false,
	}
	for _, k := range plan.UnsetEnv {
		if _, ok := required[k]; ok {
			required[k] = true
		}
	}
	for k, seen := range required {
		if !seen {
			t.Errorf("UnsetEnv = %v, want to contain %s", plan.UnsetEnv, k)
		}
	}
}
