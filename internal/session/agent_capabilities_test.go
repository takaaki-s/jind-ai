package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentCapabilities_ZeroAndUnknownValuesFailClosed(t *testing.T) {
	var zero AgentCapabilities
	capabilities := []AgentCapability{
		CapabilityLiveness,
		CapabilitySend,
		CapabilityRespond,
		CapabilityResume,
		CapabilityHooks,
		CapabilityTranscript,
		CapabilityReliableNeedsAnswer,
		AgentCapability("future-capability"),
	}
	for _, capability := range capabilities {
		if got := zero.State(capability); got != CapabilityUnknown {
			t.Errorf("zero.State(%q) = %q, want unknown", capability, got)
		}
		if zero.Supports(capability) {
			t.Errorf("zero.Supports(%q) = true, want false", capability)
		}
	}

	future := AgentCapabilities{
		SchemaVersion: AgentCapabilitiesSchemaVersion + 1,
		Respond:       CapabilitySupported,
	}
	if got := future.State(CapabilityRespond); got != CapabilityUnknown {
		t.Errorf("future schema respond = %q, want unknown", got)
	}

	invalid := AgentCapabilities{
		SchemaVersion: AgentCapabilitiesSchemaVersion,
		Respond:       CapabilityState("sometimes"),
	}
	if got := invalid.State(CapabilityRespond); got != CapabilityUnknown {
		t.Errorf("invalid state = %q, want unknown", got)
	}
}

func TestAgentCapabilities_JSONSpellsEveryStateExplicitly(t *testing.T) {
	data, err := json.Marshal(AgentCapabilities{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	wantKeys := []string{
		"schema_version", "liveness", "send", "respond", "resume", "hooks",
		"transcript", "reliable_needs_answer",
	}
	if len(got) != len(wantKeys) {
		t.Fatalf("capability keys = %v, want exactly %v", got, wantKeys)
	}
	for _, key := range wantKeys[1:] {
		if got[key] != "unknown" {
			t.Errorf("%s = %v, want unknown", key, got[key])
		}
	}

	var decoded AgentCapabilities
	if err := json.Unmarshal([]byte(`{
		"schema_version":1,
		"respond":"unsupported",
		"send":"supported",
		"hooks":"newer-state"
	}`), &decoded); err != nil {
		t.Fatalf("decode states: %v", err)
	}
	if got := decoded.State(CapabilityRespond); got != CapabilityUnsupported {
		t.Errorf("respond = %q, want unsupported", got)
	}
	if got := decoded.State(CapabilitySend); got != CapabilitySupported {
		t.Errorf("send = %q, want supported", got)
	}
	if got := decoded.State(CapabilityHooks); got != CapabilityUnknown {
		t.Errorf("future hook state = %q, want unknown", got)
	}
}

func TestCapabilitiesOf_OptionalProviderAndNormalization(t *testing.T) {
	declared := &fakeAgent{capabilities: AgentCapabilities{
		SchemaVersion: AgentCapabilitiesSchemaVersion,
		Send:          CapabilitySupported,
		Respond:       CapabilityUnsupported,
		Hooks:         CapabilityState("invalid"),
	}}

	got := CapabilitiesOf(declared)
	if got.SchemaVersion != AgentCapabilitiesSchemaVersion {
		t.Fatalf("schema = %d, want %d", got.SchemaVersion, AgentCapabilitiesSchemaVersion)
	}
	if got.State(CapabilitySend) != CapabilitySupported {
		t.Errorf("send = %q, want supported", got.State(CapabilitySend))
	}
	if got.State(CapabilityRespond) != CapabilityUnsupported {
		t.Errorf("respond = %q, want unsupported", got.State(CapabilityRespond))
	}
	if got.State(CapabilityHooks) != CapabilityUnknown {
		t.Errorf("hooks = %q, want unknown", got.State(CapabilityHooks))
	}

	// Embedding only the old Agent interface hides fakeAgent's optional
	// Capabilities method and models an adapter compiled before this extension.
	legacy := struct{ Agent }{Agent: declared}
	if got := CapabilitiesOf(legacy); got != (AgentCapabilities{}) {
		t.Errorf("legacy adapter capabilities = %+v, want zero/unknown", got)
	}
	if got := CapabilitiesOf(nil); got != (AgentCapabilities{}) {
		t.Errorf("nil adapter capabilities = %+v, want zero/unknown", got)
	}
}

func TestInfoCapabilities_OldJSONIsUnknownAndNewJSONIsExplicit(t *testing.T) {
	var old Info
	if err := json.Unmarshal([]byte(`{"id":"old"}`), &old); err != nil {
		t.Fatalf("unmarshal old Info: %v", err)
	}
	if got := old.Capabilities.State(CapabilityRespond); got != CapabilityUnknown {
		t.Errorf("old Info respond = %q, want unknown", got)
	}

	data, err := json.Marshal(Info{ID: "new"})
	if err != nil {
		t.Fatalf("marshal new Info: %v", err)
	}
	if !strings.Contains(string(data), `"capabilities":{"schema_version":0`) {
		t.Errorf("Info JSON does not carry an explicit capability set: %s", data)
	}
}

func TestManager_CapabilitiesAreLiveAndNotPersisted(t *testing.T) {
	sessionsDir := t.TempDir()
	first, _, _ := newTestManagerIn(t, sessionsDir, testIdentity())
	created, _, err := first.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ReserveCreation: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(sessionsDir, created.ID+".json"))
	if err != nil {
		t.Fatalf("read session record: %v", err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode session record: %v", err)
	}
	if _, persisted := record["capabilities"]; persisted {
		t.Errorf("persisted session contains live capabilities: %s", data)
	}

	restarted, _, _ := newTestManagerIn(t, sessionsDir, testIdentity())
	capable := &fakeAgent{capabilities: AgentCapabilities{
		SchemaVersion: AgentCapabilitiesSchemaVersion,
		Resume:        CapabilitySupported,
	}}
	restarted.SetAgentResolver(&fakeAgentResolver{agents: map[string]Agent{"claude": capable}})
	info, ok := restarted.GetInfo(created.ID)
	if !ok {
		t.Fatal("restarted manager did not load session")
	}
	if got := info.Capabilities.State(CapabilityResume); got != CapabilitySupported {
		t.Errorf("loaded session resume = %q, want supported", got)
	}

	_, newInfo, err := restarted.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ReserveCreation after resolver: %v", err)
	}
	if got := newInfo.Capabilities.State(CapabilityResume); got != CapabilitySupported {
		t.Errorf("new session resume = %q, want supported", got)
	}
}
