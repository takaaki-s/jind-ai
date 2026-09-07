package codex

import (
	"testing"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/session"
)

func TestHookStatusSource_EventMapping(t *testing.T) {
	// wantLiveness: this adapter sets the flag nowhere, and Manager withholds a
	// Liveness verdict from an idle session — so an added flag would change
	// which hooks reach one.
	tests := []struct {
		event        string
		wantStatus   session.Status
		wantClear    bool
		wantNotify   agent.NotifyKind
		wantLiveness bool
		wantOK       bool
	}{
		{"UserPromptSubmit", session.StatusThinking, true, agent.NotifyNone, false, true},
		{"PreToolUse", session.StatusThinking, true, agent.NotifyNone, false, true},
		{"PostToolUse", session.StatusThinking, true, agent.NotifyNone, false, true},
		{"PermissionRequest", session.StatusThinking, false, agent.NotifyNone, false, true},
		{"Stop", session.StatusIdle, true, agent.NotifyTaskComplete, false, true},
		// SessionStart: no status change, Manager owns the side effects.
		{"SessionStart", "", false, agent.NotifyNone, false, false},
		// Unknown event: parser must fall through cleanly.
		{"CompletelyUnknownEvent", "", false, agent.NotifyNone, false, false},
	}

	src := NewHookStatusSource()
	for _, tc := range tests {
		t.Run(tc.event, func(t *testing.T) {
			sig := agent.StatusSignal{
				Kind:    "hook",
				Payload: map[string]string{"event": tc.event},
			}
			got, ok := src.Interpret(sig)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.ClearError != tc.wantClear {
				t.Errorf("ClearError = %v, want %v", got.ClearError, tc.wantClear)
			}
			if got.Notify != tc.wantNotify {
				t.Errorf("Notify = %q, want %q", got.Notify, tc.wantNotify)
			}
			if got.Liveness != tc.wantLiveness {
				t.Errorf("Liveness = %v, want %v", got.Liveness, tc.wantLiveness)
			}
		})
	}
}

// applyEvents folds an event sequence under Manager's hook-path rule — write a
// verdict only when the adapter returns ok, last one wins — and reports the
// resulting status plus every notification raised along the way. It stands in
// for that rule alone, not for Manager.
func applyEvents(src *HookStatusSource, events []string) (session.Status, []agent.NotifyKind) {
	status := session.StatusRunning // what a spawned session holds before its first hook
	var notifies []agent.NotifyKind
	for _, ev := range events {
		upd, ok := src.Interpret(agent.StatusSignal{
			Kind:    "hook",
			Payload: map[string]string{"event": ev},
		})
		if !ok {
			continue
		}
		status = upd.Status
		if upd.Notify != agent.NotifyNone {
			notifies = append(notifies, upd.Notify)
		}
	}
	return status, notifies
}

// TestHookStatusSource_PermissionRequestSequences records the orderings a turn
// containing a PermissionRequest has to survive. They are synthetic: no capture
// of Codex's PermissionRequest payload exists in this repo, so a testdata file
// stamped with a Codex version would claim a measurement nobody took.
func TestHookStatusSource_PermissionRequestSequences(t *testing.T) {
	tests := []struct {
		name       string
		events     []string
		wantStatus session.Status
	}{
		{
			// The case the mapping exists for: an approval resolved with no
			// tool hook behind it.
			name:       "approval then straight to Stop",
			events:     []string{"UserPromptSubmit", "PermissionRequest", "Stop"},
			wantStatus: session.StatusIdle,
		},
		{
			// Duplicates and a hookless continuation both land here: with no
			// Stop behind them the turn is not known to have ended.
			name:       "duplicate approvals, nothing after them",
			events:     []string{"UserPromptSubmit", "PermissionRequest", "PermissionRequest"},
			wantStatus: session.StatusThinking,
		},
		{
			// Reachable: Codex's hook-trust gate means early hooks may never
			// run at all, so the approval can be the first event jin sees.
			name:       "approval with no preceding UserPromptSubmit",
			events:     []string{"PermissionRequest"},
			wantStatus: session.StatusThinking,
		},
		{
			// Hooks do not arrive in the order their events happened, and
			// this adapter sets no Liveness, so Manager does not withhold an
			// approval verdict from an idle session: a late one reopens the
			// turn. Pinned as the behaviour, not endorsed — the same ordering
			// on Claude Code is what Liveness exists to withhold.
			name:       "approval arriving after the turn's Stop",
			events:     []string{"UserPromptSubmit", "Stop", "PermissionRequest"},
			wantStatus: session.StatusThinking,
		},
	}

	src := NewHookStatusSource()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, notifies := applyEvents(src, tc.events)
			if status != tc.wantStatus {
				t.Errorf("final status = %q, want %q (events %v)", status, tc.wantStatus, tc.events)
			}
			if status == session.StatusPermission {
				t.Errorf("sequence %v reached StatusPermission", tc.events)
			}
			for _, n := range notifies {
				if n == agent.NotifyPermission {
					t.Errorf("sequence %v raised a permission notification", tc.events)
				}
			}
		})
	}
}

// TestHookStatusSource_RecoverNormalizesPersistedPermission covers the one
// value the recover branch corrects.
func TestHookStatusSource_RecoverNormalizesPersistedPermission(t *testing.T) {
	src := NewHookStatusSource()
	got, ok := src.Interpret(agent.StatusSignal{
		Kind: "recover",
		Payload: map[string]string{
			"persisted_status": string(session.StatusPermission),
			"agent_session_id": "0199cf6e-1d3b-7f2a-9c40-6a1b2c3d4e5f",
			"workdir":          "/tmp/jin-codex",
		},
	})
	if !ok {
		t.Fatalf("ok = false, want a verdict for a persisted permission")
	}
	// Compared whole: Manager applies Status alone on this path — see the
	// "recover" contract on StatusSignal — so any other field set here
	// promises what it cannot deliver.
	if want := (agent.StatusUpdate{Status: session.StatusThinking}); got != want {
		t.Errorf("StatusUpdate = %+v, want %+v", got, want)
	}
}

// TestHookStatusSource_RecoverNeedsNoSessionID: a codex session whose id was
// never confirmed — the hook-trust gate leaves plenty of those — can hold the
// stale permission just the same, so requiring the id would strand them.
func TestHookStatusSource_RecoverNeedsNoSessionID(t *testing.T) {
	src := NewHookStatusSource()
	got, ok := src.Interpret(agent.StatusSignal{
		Kind:    "recover",
		Payload: map[string]string{"persisted_status": string(session.StatusPermission)},
	})
	if !ok || got.Status != session.StatusThinking {
		t.Errorf("Interpret = (%+v, %v), want thinking with ok=true", got, ok)
	}
}

// TestHookStatusSource_RecoverLeavesEveryOtherStatus pins the narrowness: a
// false verdict is what keeps applyRecovery's own decision.
func TestHookStatusSource_RecoverLeavesEveryOtherStatus(t *testing.T) {
	src := NewHookStatusSource()
	others := []session.Status{
		session.StatusIdle,
		session.StatusThinking,
		session.StatusRunning,
		session.StatusStopped,
		session.StatusCreating,
		session.StatusDeleting,
		"", // no persisted status on file
		"Permission",
		"permission ",
	}
	for _, st := range others {
		t.Run(string(st), func(t *testing.T) {
			got, ok := src.Interpret(agent.StatusSignal{
				Kind:    "recover",
				Payload: map[string]string{"persisted_status": string(st)},
			})
			if ok {
				t.Errorf("persisted_status=%q returned ok=true with %+v; want the manager fallback", st, got)
			}
			if got != (agent.StatusUpdate{}) {
				t.Errorf("persisted_status=%q returned %+v, want the zero update", st, got)
			}
		})
	}
}

func TestHookStatusSource_NonHookKind(t *testing.T) {
	// Any Kind other than "hook" / "recover" — pane-tail, poll, whatever —
	// must be ignored. Manager may route non-hook signals through here in the
	// future, and returning ok=true for them would trip a status write on a
	// signal the adapter did not actually understand. The payload carries the
	// discriminator of each understood kind, so a switch that fell through to
	// either branch fails here.
	src := NewHookStatusSource()
	for _, k := range []string{"", "pane-tail", "poll", "running", "Recover"} {
		sig := agent.StatusSignal{
			Kind: k,
			Payload: map[string]string{
				"event":            "UserPromptSubmit",
				"persisted_status": string(session.StatusPermission),
			},
		}
		if got, ok := src.Interpret(sig); ok {
			t.Errorf("Kind=%q returned ok=true with %+v; want ignored", k, got)
		}
	}
}

func TestHookStatusSource_MissingEventField(t *testing.T) {
	// A hook signal without an "event" key is malformed; the adapter must
	// treat it as unknown rather than pick up whatever the zero-value switch
	// case would fall into. persisted_status rides along to pin that the hook
	// path never reads the recover branch's discriminator.
	src := NewHookStatusSource()
	sig := agent.StatusSignal{
		Kind:    "hook",
		Payload: map[string]string{"persisted_status": string(session.StatusPermission)},
	}
	if got, ok := src.Interpret(sig); ok {
		t.Errorf("missing event returned ok=true with %+v", got)
	}
}

func TestHookStatusSource_ImplementsInterface(t *testing.T) {
	// Compile-time interface check: HookStatusSource must satisfy the
	// session.StatusSource contract Manager holds it through.
	var _ session.StatusSource = (*HookStatusSource)(nil)
	var _ session.StatusSource = NewHookStatusSource()
}
