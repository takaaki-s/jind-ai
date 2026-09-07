package session

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

// attentionOf reads a session's in-memory receipt through Get.
func attentionOf(t *testing.T, mgr *Manager, id string) Attention {
	t.Helper()
	got, ok := mgr.Get(id)
	if !ok {
		t.Fatalf("Get(%s) returned ok=false", id)
	}
	return got.Attention
}

// persistedAttentionOf reads a session's receipt back from its file, which is
// the only thing that survives a daemon restart.
func persistedAttentionOf(t *testing.T, mgr *Manager, id string) Attention {
	t.Helper()
	loaded, err := mgr.store.Load(id)
	if err != nil {
		t.Fatalf("Load(%s): %v", id, err)
	}
	return loaded.Attention
}

func newAttentionSession(t *testing.T, mgr *Manager, desc string) *Session {
	t.Helper()
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: t.TempDir(), Description: desc})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	if sess.Attention != (Attention{}) {
		t.Fatalf("a new session starts with %+v, want the zero receipt", sess.Attention)
	}
	return sess
}

// The core transition: an applied task-completion verdict leaves exactly one
// new generation, in memory and on disk alike.
func TestManager_CompletionRaisesOneGeneration(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "completion")

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	want := Attention{State: AttentionDone, Generation: 1}
	if got := attentionOf(t, mgr, sess.ID); got != want {
		t.Errorf("in-memory Attention = %+v, want %+v", got, want)
	}
	if got := persistedAttentionOf(t, mgr, sess.ID); got != want {
		t.Errorf("persisted Attention = %+v, want %+v", got, want)
	}
}

// The process axis is untouched: a completion does not invent a status, and
// the status a completion accompanies is the adapter's, not attention's.
func TestManager_CompletionDoesNotChangeStatusHandling(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "status")

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	got, _ := mgr.Get(sess.ID)
	if got.Status != StatusIdle {
		t.Errorf("Status = %q, want %q", got.Status, StatusIdle)
	}
}

// Each turn raises its own generation, so a second completion is unseen again
// even without an intervening acknowledgement.
func TestManager_EachAppliedCompletionRaisesItsOwnGeneration(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "two turns")

	for range 2 {
		mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
		mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")
	}

	if got := attentionOf(t, mgr, sess.ID); got.Generation != 2 {
		t.Errorf("Generation = %d, want 2", got.Generation)
	}
}

// Every status a turn can end from reaches the same receipt: the predicate is
// "the status moved", not "it moved from thinking".
func TestManager_CompletionFromEveryOpenStatus(t *testing.T) {
	for _, opening := range []struct {
		name  string
		event string
		ntype string
		want  Status
	}{
		{"thinking", "UserPromptSubmit", "", StatusThinking},
		{"permission", "Notification", "permission_prompt", StatusPermission},
	} {
		t.Run(opening.name, func(t *testing.T) {
			mgr, _, _ := newTestManager(t)
			sess := newAttentionSession(t, mgr, opening.name)

			mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, opening.event, opening.ntype, "", "")
			if got, _ := mgr.Get(sess.ID); got.Status != opening.want {
				t.Fatalf("setup: Status = %q, want %q", got.Status, opening.want)
			}

			mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")
			if got := attentionOf(t, mgr, sess.ID); got.Generation != 1 {
				t.Errorf("Generation = %d, want 1", got.Generation)
			}
		})
	}
}

// The predicate reads the adapter's normalized verdict, not the event name, so
// an adapter that calls the end of a turn something else still gets a receipt.
// A Manager keyed on the string "Stop" would pass every other test here.
func TestManager_CompletionKeyedOnTheVerdictNotTheEventName(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	installStatusSource(t, mgr, func(sig StatusSignal) (StatusUpdate, bool) {
		switch sig.Payload["event"] {
		case "turn.started":
			return StatusUpdate{Status: StatusThinking, Notify: NotifyNone}, true
		case "turn.finished":
			return StatusUpdate{Status: StatusIdle, Notify: NotifyTaskComplete}, true
		}
		return StatusUpdate{}, false
	})
	sess := newAttentionSession(t, mgr, "other vocabulary")

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "turn.started", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "turn.finished", "", "", "")

	if got := attentionOf(t, mgr, sess.ID); got.Generation != 1 {
		t.Errorf("Generation = %d, want 1 — the receipt must follow NotifyTaskComplete, not the event name", got.Generation)
	}
}

// Events that are not an applied completion leave the receipt at zero. Each
// case is one way a predicate looser than "the adapter's verdict said the turn
// ended, and the status moved" would raise one.
//
// Every case here starts from a session that never completed, so the
// assertion can be exact — a bound like "at most one new receipt" reads as a
// test but passes for four of these.
func TestManager_NonCompletionEventsRaiseNoReceipt(t *testing.T) {
	tests := []struct {
		name   string
		events func(mgr *Manager, sess *Session)
	}{
		{
			name: "a permission prompt",
			events: func(mgr *Manager, sess *Session) {
				mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Notification", "permission_prompt", "", "")
			},
		},
		{
			name: "a prompt that opened a turn",
			events: func(mgr *Manager, sess *Session) {
				mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
			},
		},
		{
			name: "an event the adapter has no verdict for",
			events: func(mgr *Manager, sess *Session) {
				mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SomethingElse", "", "", "")
			},
		},
		{
			name: "a session that ended",
			events: func(mgr *Manager, sess *Session) {
				mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "SessionEnd", "", "", "")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, _, _ := newTestManager(t)
			sess := newAttentionSession(t, mgr, tt.name)

			tt.events(mgr, sess)

			if got := attentionOf(t, mgr, sess.ID); got != (Attention{}) {
				t.Errorf("Attention = %+v, want the zero receipt", got)
			}
		})
	}
}

// The duplicate case deserves an exact number rather than a bound: a second
// Stop for a turn that already ended is the commonest way an agent double-fires
// a hook, and it must not raise a second receipt.
func TestManager_DuplicateCompletionRaisesNoSecondReceipt(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "duplicate")

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	if got := attentionOf(t, mgr, sess.ID); got.Generation != 1 {
		t.Errorf("Generation = %d, want 1", got.Generation)
	}
}

// A turn that failed leaves no receipt at all: "done" is not "stopped".
func TestManager_FailedTurnRaisesNoReceipt(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "failed")

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "StopFailure", "", "", "it broke")

	if got := attentionOf(t, mgr, sess.ID); got != (Attention{}) {
		t.Errorf("Attention = %+v, want the zero receipt", got)
	}
}

// A tool hook straggling in after the Stop is withheld as a status verdict
// (see status_liveness_test.go); it must not raise a receipt through the back
// door either.
func TestManager_SuppressedLivenessVerdictRaisesNoReceipt(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "straggler")

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "PostToolUse", "", "", "")

	if got := attentionOf(t, mgr, sess.ID); got.Generation != 1 {
		t.Errorf("Generation = %d, want 1", got.Generation)
	}
}

func TestManager_MarkSeenAcknowledgesTheReceipt(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "seen")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	info, err := mgr.MarkSeen(sess.ID)
	if err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	want := AttentionInfo{State: AttentionDone, Generation: 1, SeenGeneration: 1}
	if info.Attention != want {
		t.Errorf("returned Attention = %+v, want %+v", info.Attention, want)
	}
	if info.Status != StatusIdle {
		t.Errorf("Status = %q, want %q — seen touches no process status", info.Status, StatusIdle)
	}
	if got := persistedAttentionOf(t, mgr, sess.ID); got.SeenGeneration != 1 {
		t.Errorf("persisted SeenGeneration = %d, want 1 — the acknowledgement must survive a restart", got.SeenGeneration)
	}
}

func TestManager_MarkSeenIsIdempotent(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "idempotent")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	first, err := mgr.MarkSeen(sess.ID)
	if err != nil {
		t.Fatalf("first MarkSeen: %v", err)
	}
	second, err := mgr.MarkSeen(sess.ID)
	if err != nil {
		t.Fatalf("second MarkSeen: %v", err)
	}
	if first.Attention != second.Attention {
		t.Errorf("second MarkSeen = %+v, want %+v", second.Attention, first.Attention)
	}
}

// Acknowledging a session that never completed succeeds and writes no receipt,
// so a script may call it without first asking whether there is one.
func TestManager_MarkSeenOnZeroReceiptSucceeds(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "nothing to see")

	info, err := mgr.MarkSeen(sess.ID)
	if err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	if info.Attention != (AttentionInfo{}) {
		t.Errorf("Attention = %+v, want the zero projection", info.Attention)
	}
}

func TestManager_MarkSeenUnknownSessionIsAnError(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	_, err := mgr.MarkSeen("no-such-session")
	if err == nil {
		t.Fatal("MarkSeen on a missing session returned nil error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to say the session was not found", err)
	}
}

// The whole point of keeping generation and seen separate: the next completion
// is unseen again.
func TestManager_CompletionAfterSeenIsUnseenAgain(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "seen then done")

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")
	if _, err := mgr.MarkSeen(sess.ID); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	want := Attention{State: AttentionDone, Generation: 2, SeenGeneration: 1}
	if got := attentionOf(t, mgr, sess.ID); got != want {
		t.Errorf("Attention = %+v, want %+v", got, want)
	}
	if got := persistedAttentionOf(t, mgr, sess.ID); got != want {
		t.Errorf("persisted Attention = %+v, want %+v", got, want)
	}
}

// A save from an unrelated writer carries a whole session record. It must not
// take the receipt with it.
func TestManager_UnrelatedSavesPreserveTheReceipt(t *testing.T) {
	tests := []struct {
		name string
		save func(t *testing.T, mgr *Manager, sess *Session)
	}{
		{
			name: "set-description",
			save: func(t *testing.T, mgr *Manager, sess *Session) {
				if err := mgr.SetDescription(sess.ID, "renamed"); err != nil {
					t.Fatalf("SetDescription: %v", err)
				}
			},
		},
		{
			name: "a CWD-tracking hook",
			save: func(t *testing.T, mgr *Manager, sess *Session) {
				mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "CwdChanged", "", t.TempDir(), "")
			},
		},
		{
			name: "a status assignment",
			save: func(t *testing.T, mgr *Manager, sess *Session) {
				mgr.SetStatus(sess.ID, StatusRunning)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, _, _ := newTestManager(t)
			sess := newAttentionSession(t, mgr, tt.name)
			mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
			mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

			tt.save(t, mgr, sess)

			want := Attention{State: AttentionDone, Generation: 1}
			if got := attentionOf(t, mgr, sess.ID); got != want {
				t.Errorf("in-memory Attention = %+v, want %+v", got, want)
			}
			if got := persistedAttentionOf(t, mgr, sess.ID); got != want {
				t.Errorf("persisted Attention = %+v, want %+v", got, want)
			}
		})
	}
}

// Completions and acknowledgements racing each other. -race covers the memory
// side; what is asserted here is that the settled file agrees with the settled
// memory and that both invariants hold — the outcome the Store merge exists for.
func TestManager_CompletionAndSeenRace(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "race")

	const turns = 30
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range turns {
			mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
			mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")
		}
	}()
	go func() {
		defer wg.Done()
		for range turns {
			if _, err := mgr.MarkSeen(sess.ID); err != nil {
				t.Errorf("MarkSeen: %v", err)
			}
		}
	}()
	wg.Wait()

	inMemory := attentionOf(t, mgr, sess.ID)
	onDisk := persistedAttentionOf(t, mgr, sess.ID)

	if inMemory.Generation != turns {
		t.Errorf("in-memory Generation = %d, want %d", inMemory.Generation, turns)
	}
	if inMemory.SeenGeneration > inMemory.Generation {
		t.Errorf("in-memory %+v violates seen <= generation", inMemory)
	}
	if onDisk != inMemory {
		t.Errorf("persisted Attention = %+v, want the settled in-memory %+v", onDisk, inMemory)
	}
}

// The save error is the whole of what makes a retry useful: a caller told the
// acknowledgement succeeded has no reason to try again, and the disk record
// still reads unseen. Without this test, dropping the error to nil passes
// every other test in the package.
func TestManager_MarkSeenReportsASaveFailure(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := newAttentionSession(t, mgr, "save fails")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	setForTest(t, &atomicWrite, func(string, []byte, os.FileMode, string) error {
		return errors.New("disk is full")
	})

	info, err := mgr.MarkSeen(sess.ID)
	if err == nil {
		t.Fatal("MarkSeen returned nil error while its save failed")
	}
	if !strings.Contains(err.Error(), "disk is full") {
		t.Errorf("error = %q, want the store's error", err)
	}
	// Memory still advanced, which is what makes the retry a no-op mutation
	// that only has to land the write.
	if info.Attention.SeenGeneration != 1 {
		t.Errorf("returned SeenGeneration = %d, want 1 — the postcondition is the in-memory state",
			info.Attention.SeenGeneration)
	}
}

// A restart is not an acknowledgement. The daemon has no idea whether anyone
// looked while it was down, and recovery raises no completion verdict of its
// own, so an outstanding receipt has to come back outstanding — and an
// acknowledged one has to stay acknowledged.
func TestManager_RestartChangesNoReceipt(t *testing.T) {
	dir := t.TempDir()

	restart := func(t *testing.T) *Manager {
		t.Helper()
		mgr, _, _ := newTestManagerIn(t, dir, testIdentity())
		return mgr
	}

	first := restart(t)
	sess := newAttentionSession(t, first, "restart")
	first.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")
	first.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")

	revived := restart(t)
	got := attentionOf(t, revived, sess.ID)
	if !got.Unseen() {
		t.Errorf("Attention = %+v after a restart, want it still unseen", got)
	}
	if want := (Attention{State: AttentionDone, Generation: 1}); got != want {
		t.Errorf("Attention = %+v, want %+v", got, want)
	}

	if _, err := revived.MarkSeen(sess.ID); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	again := restart(t)
	if got := attentionOf(t, again, sess.ID); got.Unseen() {
		t.Errorf("Attention = %+v after acknowledging and restarting, want it still seen", got)
	}
}
