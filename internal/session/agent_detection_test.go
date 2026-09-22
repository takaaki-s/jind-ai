package session

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/tmux"
)

type detectingAgent struct {
	*fakeAgent
	kind       string
	executable string
}

func TestAgentDetection_PersistedRoundTrip(t *testing.T) {
	want := AgentDetection{
		Provenance: AgentDetectionUserSelected, SelectedKind: "claude",
		Candidates: []AgentDetectionCandidate{{
			Kind: "claude", Score: detectionScoreProcess,
			Evidence: []AgentDetectionEvidence{{Source: "process", PID: 102, Command: "claude"}},
		}},
	}
	data, err := json.Marshal(Session{ID: "s", AgentKind: "claude", AgentDetection: want})
	if err != nil {
		t.Fatal(err)
	}
	var got Session
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AgentDetection, want) {
		t.Fatalf("detection round trip = %+v, want %+v", got.AgentDetection, want)
	}
	if info := got.ToInfo(); !reflect.DeepEqual(info.AgentDetection, want) {
		t.Fatalf("Info detection = %+v, want %+v", info.AgentDetection, want)
	}
}

func (a *detectingAgent) Kind() string { return a.kind }
func (a *detectingAgent) RecognizesExecutable(name string) bool {
	return name == a.executable
}

func detectionResolver(kinds ...string) *fakeAgentResolver {
	agents := make(map[string]Agent, len(kinds))
	for _, kind := range kinds {
		agents[kind] = &detectingAgent{fakeAgent: &fakeAgent{}, kind: kind, executable: kind}
	}
	return &fakeAgentResolver{agents: agents}
}

func detectionPane(current, start string, processes ...tmux.PaneProcess) tmux.PaneInfo {
	return tmux.PaneInfo{
		PanePID: 100, CurrentCommand: current, StartCommand: start, ProcessAncestry: processes,
	}
}

func TestDetectAgent_ExactThroughWrapper(t *testing.T) {
	pane := detectionPane("zsh", "env TERM=x zsh",
		tmux.PaneProcess{PID: 100, PPID: 1, Command: "zsh"},
		tmux.PaneProcess{PID: 101, PPID: 100, Command: "node"},
		tmux.PaneProcess{PID: 102, PPID: 101, Command: "/usr/local/bin/claude"},
	)
	got, err := detectAgent(pane, detectionResolver("claude", "codex", "opencode"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance != AgentDetectionDetected || got.SelectedKind != "claude" || len(got.Candidates) != 1 {
		t.Fatalf("detection = %+v", got)
	}
	if got.Candidates[0].Score != detectionScoreProcess {
		t.Fatalf("score = %d, want process score %d", got.Candidates[0].Score, detectionScoreProcess)
	}
}

func TestDetectAgent_NestedKindsStayAmbiguousDespiteRanking(t *testing.T) {
	pane := detectionPane("codex", "claude",
		tmux.PaneProcess{PID: 100, PPID: 1, Command: "zsh"},
		tmux.PaneProcess{PID: 101, PPID: 100, Command: "claude"},
		tmux.PaneProcess{PID: 102, PPID: 101, Command: "codex"},
	)
	got, err := detectAgent(pane, detectionResolver("claude", "codex", "opencode"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance != AgentDetectionAmbiguous || got.SelectedKind != "" || len(got.Candidates) != 2 {
		t.Fatalf("detection = %+v", got)
	}
	if got.Candidates[0].Kind != "codex" || got.Candidates[0].Score <= got.Candidates[1].Score {
		t.Fatalf("ranked candidates = %+v", got.Candidates)
	}
}

func TestDetectAgent_NoMatchFallsBackToGeneric(t *testing.T) {
	pane := detectionPane("vim", "sh -c 'echo claude'",
		tmux.PaneProcess{PID: 100, PPID: 1, Command: "zsh"},
		tmux.PaneProcess{PID: 101, PPID: 100, Command: "vim"},
	)
	got, err := detectAgent(pane, detectionResolver("claude", "codex", "opencode"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance != AgentDetectionGeneric || got.SelectedKind != GenericAgentKind || len(got.Candidates) != 0 {
		t.Fatalf("detection = %+v", got)
	}
}

func TestDetectAgent_UserSelectionIsAuditedWithoutPromotingCapabilities(t *testing.T) {
	pane := detectionPane("codex", "claude",
		tmux.PaneProcess{PID: 100, PPID: 1, Command: "claude"},
		tmux.PaneProcess{PID: 101, PPID: 100, Command: "codex"},
	)
	got, err := detectAgent(pane, detectionResolver("claude", "codex"), "claude")
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance != AgentDetectionUserSelected || got.SelectedKind != "claude" || len(got.Candidates) != 2 {
		t.Fatalf("detection = %+v", got)
	}
	caps := adoptedCapabilities(AgentCapabilities{
		SchemaVersion: AgentCapabilitiesSchemaVersion, Hooks: CapabilitySupported, Resume: CapabilitySupported,
	})
	if caps.Hooks != CapabilityUnsupported || caps.Resume != CapabilityUnsupported || caps.Liveness != CapabilitySupported {
		t.Fatalf("effective adopted capabilities = %+v", caps)
	}
}

func TestStartCommandExecutable_OnlyTransparentModifiers(t *testing.T) {
	tests := map[string]string{
		"env FOO=bar exec /usr/local/bin/opencode --model x": "opencode",
		"env -u CLAUDECODE claude":                           "claude",
		"command codex":                                      "codex",
		"sh -c 'echo claude'":                                "sh",
		"FOO=bar claude":                                     "claude",
		"":                                                   "",
	}
	for input, want := range tests {
		if got := startCommandExecutable(input); got != want {
			t.Errorf("startCommandExecutable(%q) = %q, want %q", input, got, want)
		}
	}
}
