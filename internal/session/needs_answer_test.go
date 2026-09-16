package session

import (
	"encoding/json"
	"testing"
)

func reliableNeedsAnswerCapabilities() AgentCapabilities {
	return AgentCapabilities{
		SchemaVersion:       AgentCapabilitiesSchemaVersion,
		ReliableNeedsAnswer: CapabilitySupported,
	}
}

func TestNeedsAnswer_TransitionsKeepSeenSeparateFromResolution(t *testing.T) {
	var evidence NeedsAnswer
	if got := evidence.state(); got != NeedsAnswerUnknown {
		t.Fatalf("zero state = %q, want unknown", got)
	}

	evidence = evidence.resolved()
	if got := evidence.state(); got != NeedsAnswerNotNeeded {
		t.Fatalf("baseline state = %q, want not-needed", got)
	}
	evidence = evidence.required()
	if got := evidence.state(); got != NeedsAnswerRequired || !evidence.Unseen() {
		t.Fatalf("required = %+v, state %q; want unseen needs-answer", evidence, got)
	}

	duplicate := evidence.required()
	if duplicate.Generation != 1 {
		t.Fatalf("duplicate Generation = %d, want 1", duplicate.Generation)
	}
	evidence = evidence.acknowledged()
	if !evidence.Unresolved() || evidence.Unseen() {
		t.Fatalf("acknowledged evidence = %+v, want resolved=false unseen=false", evidence)
	}

	evidence = evidence.resolved().required()
	if evidence.Generation != 2 || !evidence.Unseen() {
		t.Fatalf("next wait = %+v, want generation 2 unseen", evidence)
	}
}

func TestNeedsAnswer_MergeCannotRollBackWaitResolutionOrSeen(t *testing.T) {
	required := NeedsAnswer{Known: true, Generation: 2, ResolvedGeneration: 1}
	resolvedAndSeen := NeedsAnswer{Known: true, Generation: 2, ResolvedGeneration: 2, SeenGeneration: 2}
	if got := mergeNeedsAnswer(required, resolvedAndSeen); got != resolvedAndSeen {
		t.Errorf("merge = %+v, want %+v", got, resolvedAndSeen)
	}
	if got := mergeNeedsAnswer(resolvedAndSeen, required); got != resolvedAndSeen {
		t.Errorf("reverse merge = %+v, want %+v", got, resolvedAndSeen)
	}
}

func TestNeedsAnswerInfo_UnknownIsExplicitJSON(t *testing.T) {
	data, err := json.Marshal((&Session{}).ToInfo())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		NeedsAnswer NeedsAnswerInfo `json:"needs_answer"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.NeedsAnswer.State != NeedsAnswerUnknown {
		t.Errorf("state = %q, want unknown; JSON=%s", got.NeedsAnswer.State, data)
	}
}

func TestManager_ReliableNeedsAnswerLifecycleAndRestart(t *testing.T) {
	dir := t.TempDir()
	mgr, _, _ := newTestManagerIn(t, dir, testIdentity())
	fakeClaudeAgent(t, mgr).capabilities = reliableNeedsAnswerCapabilities()

	sess, info, err := mgr.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if info.NeedsAnswer.State != NeedsAnswerNotNeeded {
		t.Fatalf("new state = %q, want not-needed", info.NeedsAnswer.State)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Notification", "permission_prompt", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Notification", "permission_prompt", "", "")
	info, _ = mgr.GetInfo(sess.ID)
	if info.NeedsAnswer.State != NeedsAnswerRequired || info.NeedsAnswer.Generation != 1 || !info.NeedsAnswer.Unseen {
		t.Fatalf("duplicate wait = %+v, want generation 1 unseen", info.NeedsAnswer)
	}

	info, err = mgr.MarkSeen(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.NeedsAnswer.State != NeedsAnswerRequired || info.NeedsAnswer.Unseen {
		t.Fatalf("seen wait = %+v, want still required but seen", info.NeedsAnswer)
	}

	restarted, _, _ := newTestManagerIn(t, dir, testIdentity())
	fakeClaudeAgent(t, restarted).capabilities = reliableNeedsAnswerCapabilities()
	restarted.SetAgentResolver(restarted.agentResolver)
	info, _ = restarted.GetInfo(sess.ID)
	if info.NeedsAnswer.State != NeedsAnswerRequired || info.NeedsAnswer.Unseen {
		t.Fatalf("restarted wait = %+v, want required and seen", info.NeedsAnswer)
	}

	restarted.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	info, _ = restarted.GetInfo(sess.ID)
	if info.NeedsAnswer.State != NeedsAnswerNotNeeded || info.NeedsAnswer.ResolvedGeneration != 1 {
		t.Fatalf("resolved wait = %+v, want not-needed at generation 1", info.NeedsAnswer)
	}
}

func TestManager_UnreliableNeedsAnswerNeverAsserts(t *testing.T) {
	for _, state := range []CapabilityState{CapabilityUnsupported, CapabilityUnknown} {
		t.Run(state.wireValue(), func(t *testing.T) {
			mgr, _, _ := newTestManager(t)
			ag := fakeClaudeAgent(t, mgr)
			ag.capabilities = AgentCapabilities{
				SchemaVersion:       AgentCapabilitiesSchemaVersion,
				ReliableNeedsAnswer: state,
			}
			ag.statusFn = func(StatusSignal) (StatusUpdate, bool) {
				return StatusUpdate{
					Status:      StatusPermission,
					NeedsAnswer: NeedsAnswerSignalRequired,
				}, true
			}

			sess, _, err := mgr.ReserveCreation(CreateOptions{WorkDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "PermissionRequest", "", "", "")
			info, _ := mgr.GetInfo(sess.ID)
			if info.Status != StatusPermission {
				t.Errorf("status = %q, want permission (axes must be independent)", info.Status)
			}
			if info.NeedsAnswer.State != NeedsAnswerUnknown || info.NeedsAnswer.Unseen {
				t.Errorf("needs-answer = %+v, want explicit unknown", info.NeedsAnswer)
			}
		})
	}
}
