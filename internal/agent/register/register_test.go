package register_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/transcript"
	// Blank import triggers init() in the register package, which is the
	// side-effect under test. Without this, agent.Registry stays empty in
	// tests because the daemon-side blank import from cmd/jin doesn't
	// reach here.
	_ "github.com/takaaki-s/jind-ai/internal/agent/register"
)

// TestRegisterInit_RegistersKnownKinds is the guardrail for `jin session
// new --agent codex`: if someone deletes the codex import or Register
// call in register.go, the daemon would silently reject Codex sessions
// with an "unknown agent kind" error at spawn time. Failing this test
// forces the mistake to be caught in CI.
func TestRegisterInit_RegistersKnownKinds(t *testing.T) {
	want := map[string]bool{
		"claude":   false,
		"codex":    false,
		"opencode": false,
	}
	for _, k := range agent.Kinds() {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("registry missing %q — check register.go", k)
		}
	}
}

func TestRegisterInit_LookupCodex(t *testing.T) {
	// Beyond presence in Kinds(): the actual Codex adapter object must
	// come back from Lookup and identify itself correctly.
	a, err := agent.Lookup("codex")
	if err != nil {
		t.Fatalf("Lookup(codex): %v", err)
	}
	if a.Kind() != "codex" {
		t.Errorf("Kind() = %q, want %q", a.Kind(), "codex")
	}
}

func TestRegisterInit_LookupOpencode(t *testing.T) {
	// Same guardrail as Codex above, for the opencode adapter.
	a, err := agent.Lookup("opencode")
	if err != nil {
		t.Fatalf("Lookup(opencode): %v", err)
	}
	if a.Kind() != "opencode" {
		t.Errorf("Kind() = %q, want %q", a.Kind(), "opencode")
	}
}

func TestRegisterInit_CapabilityDeclarations(t *testing.T) {
	want := map[string]session.AgentCapabilities{
		"claude": {
			SchemaVersion:       session.AgentCapabilitiesSchemaVersion,
			Liveness:            session.CapabilitySupported,
			Send:                session.CapabilitySupported,
			Respond:             session.CapabilitySupported,
			Resume:              session.CapabilitySupported,
			Hooks:               session.CapabilitySupported,
			Transcript:          session.CapabilitySupported,
			ReliableNeedsAnswer: session.CapabilitySupported,
		},
		"codex": {
			SchemaVersion:       session.AgentCapabilitiesSchemaVersion,
			Liveness:            session.CapabilitySupported,
			Send:                session.CapabilitySupported,
			Respond:             session.CapabilityUnsupported,
			Resume:              session.CapabilitySupported,
			Hooks:               session.CapabilitySupported,
			Transcript:          session.CapabilitySupported,
			ReliableNeedsAnswer: session.CapabilityUnsupported,
		},
		"opencode": {
			SchemaVersion:       session.AgentCapabilitiesSchemaVersion,
			Liveness:            session.CapabilitySupported,
			Send:                session.CapabilitySupported,
			Respond:             session.CapabilityUnsupported,
			Resume:              session.CapabilitySupported,
			Hooks:               session.CapabilitySupported,
			Transcript:          session.CapabilitySupported,
			ReliableNeedsAnswer: session.CapabilitySupported,
		},
	}

	for _, kind := range agent.Kinds() {
		t.Run(kind, func(t *testing.T) {
			ag, err := agent.Lookup(kind)
			if err != nil {
				t.Fatalf("Lookup(%s): %v", kind, err)
			}
			got := session.CapabilitiesOf(ag)
			expected, ok := want[kind]
			if !ok {
				t.Fatalf("registered adapter %q has no capability contract test", kind)
			}
			if got != expected {
				t.Errorf("Capabilities() = %+v, want %+v", got, expected)
			}
		})
	}
}

// TestRegisterInit_TranscriptWiring guards the three one-line methods that
// decide what `jin session result` answers for each kind.
//
// Every other part of that path is covered from both sides, but the methods
// joining them were not, and each one is a single return statement. Rewriting
// claude's to return nil makes `session result` fail for every Claude Code
// session, and before this test the whole suite stayed green.
func TestRegisterInit_TranscriptWiring(t *testing.T) {
	tests := []struct {
		kind     string
		readable bool
		why      string
	}{
		{"claude", true, "the reference reader; nil here breaks result for every Claude Code session"},
		{"codex", true, "reads the rollout JSONL; nil here silently removes the feature"},
		{"opencode", true, "shells out to `opencode export`; nil here silently removes the feature"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			a, err := agent.Lookup(tt.kind)
			if err != nil {
				t.Fatalf("Lookup(%s): %v", tt.kind, err)
			}
			src := a.Transcript()
			if (src != nil) != tt.readable {
				t.Fatalf("Transcript() != nil is %v, want %v — %s", src != nil, tt.readable, tt.why)
			}
		})
	}
}

// TestRegisterInit_ClaudeReturnsTheSharedReader pins claude to
// transcript.Reader specifically, not merely to something non-nil.
//
// The nil check above cannot see a swap to a different implementation, and a
// swap would change what every existing Claude Code session returns — the one
// outcome this change was required not to have.
func TestRegisterInit_ClaudeReturnsTheSharedReader(t *testing.T) {
	a, err := agent.Lookup("claude")
	if err != nil {
		t.Fatalf("Lookup(claude): %v", err)
	}
	if _, ok := a.Transcript().(*transcript.Reader); !ok {
		t.Fatalf("Transcript() is %T, want *transcript.Reader (the pre-change reader, unchanged)", a.Transcript())
	}
}

// TestRegisterInit_PollableWiring pins which readers the preview path may call
// on a timer.
//
// Manager.AttachLastMessages decorates every row of `session list`, which the
// TUI refreshes on a timer, so it must skip a reader that spawns a process. The
// opencode reader does: on a list of opencode rows that would be one process
// per row per refresh, permanently. Neither `session result` nor the preview is
// wrong on its own, so nothing else would catch this.
func TestRegisterInit_PollableWiring(t *testing.T) {
	tests := []struct {
		kind     string
		pollable bool
		why      string
	}{
		{"claude", true, "opens one file; the preview path has always called it"},
		{"codex", true, "locates and walks a rollout, the same order of cost"},
		{"opencode", false, "spawns `opencode export`; a timer must never call it"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			a, err := agent.Lookup(tt.kind)
			if err != nil {
				t.Fatalf("Lookup(%s): %v", tt.kind, err)
			}
			_, got := a.Transcript().(session.PollableTranscriptSource)
			if got != tt.pollable {
				t.Fatalf("pollable = %v, want %v — %s", got, tt.pollable, tt.why)
			}
		})
	}
}

// hostileSessionIDs are values that were executable once recorded. Manager
// splices SpawnPlan.Command into `SHELL -ic '...'`, so a value an adapter
// concatenates there is interpreted — at the unrelated later moment the session
// resumes, in that session's working directory.
//
// The "ses_" variants matter as much as the bare ones: an adapter that gates its
// resume on a prefix only reaches the dangerous branch for an id that carries it,
// so an id without one would exercise the fresh-spawn path and prove nothing.
var hostileSessionIDs = []string{
	"x$(touch /tmp/jin-should-not-exist)",
	"x;touch /tmp/jin-should-not-exist",
	"x`touch /tmp/jin-should-not-exist`",
	"x'; touch /tmp/jin-should-not-exist; '",
	"ses_x$(touch /tmp/jin-should-not-exist)",
	"ses_x;touch /tmp/jin-should-not-exist",
	"0198f1b2-4c3d-7a1e-8b2f-000000000abc$(touch /tmp/jin-should-not-exist)",
}

// hostileModel is the second value an adapter may name on the command line and
// did not choose. It reaches SpawnCommand by the same route the ids above do —
// persisted at creation, replayed on every later resume — so it is checked in
// the same loop rather than in a test of its own.
//
// Its payload names a different file from theirs so a leak can still be
// attributed to one field or the other.
const hostileModel = "m$(touch /tmp/jin-model-should-not-exist)"

// TestSpawnCommand_NoAdapterPutsUntrustedValuesInTheCommand is the conformance
// check for the one rule that, if forgotten, restores arbitrary code execution.
//
// It is written over agent.Kinds() rather than per adapter, and that is the
// whole point. Manager validates an id before recording it, but validation and
// this rule defend different things: a record written by an older jind-ai, or
// edited by hand, reaches SpawnCommand having passed no gate. A per-package
// test cannot fail for a package that does not exist yet, so a fourth adapter
// would reintroduce the defect with nothing to catch it. Registering a kind is
// what enrols it here.
//
// Both untrusted values ride together rather than in two loops: an adapter
// builds one command line from both, and pairing them checks the model on the
// resume branches the ids reach as well as on the fresh one.
//
// It lives in register_test because that package imports both internal/agent
// and internal/session, so it can reach every adapter at once. An adapter's own
// package cannot: internal/agent imports internal/session.
func TestSpawnCommand_NoAdapterPutsUntrustedValuesInTheCommand(t *testing.T) {
	for _, kind := range agent.Kinds() {
		a, err := agent.Lookup(kind)
		if err != nil {
			t.Fatalf("Lookup(%s): %v", kind, err)
		}
		for _, id := range hostileSessionIDs {
			for _, started := range []bool{false, true} {
				plan := a.SpawnCommand(session.SpawnOptions{
					AgentSessionID:      id,
					AgentSessionStarted: started,
					Model:               hostileModel,
					WorkDir:             t.TempDir(),
				})
				if strings.Contains(plan.Command, id) {
					t.Errorf("%s.SpawnCommand(started=%v) spliced the session id into Command: %q",
						kind, started, plan.Command)
				}
				if strings.Contains(plan.Command, hostileModel) {
					t.Errorf("%s.SpawnCommand(started=%v) spliced the model into Command: %q",
						kind, started, plan.Command)
				}
				// Catches a partial splice too — an adapter that escaped or
				// trimmed either value still hands the shell something to run.
				if strings.Contains(plan.Command, "touch") {
					t.Errorf("%s.SpawnCommand(started=%v) put a payload fragment into Command: %q",
						kind, started, plan.Command)
				}
				// Whatever an adapter chooses to carry, it carries verbatim:
				// Manager quotes every ExtraEnv value, so pre-escaping here
				// would double-escape at the shell.
				for k, v := range plan.ExtraEnv {
					if strings.Contains(v, "touch") && v != id && v != hostileModel {
						t.Errorf("%s.SpawnCommand(started=%v) pre-escaped a value in ExtraEnv[%s] = %q, want %q or %q",
							kind, started, k, v, id, hostileModel)
					}
				}
			}
		}
	}
}

// TestRecognizesSessionID_NoAdapterAcceptsEverything is the companion for the
// second gate. An adapter that answers true unconditionally satisfies the
// interface and compiles, and Manager's own safety gate would still stand — so
// the failure is a quiet one: ids belonging to a different agent become
// recordable, and a session's identity can be pointed at a conversation that is
// not its own.
func TestRecognizesSessionID_NoAdapterAcceptsEverything(t *testing.T) {
	for _, kind := range agent.Kinds() {
		a, err := agent.Lookup(kind)
		if err != nil {
			t.Fatalf("Lookup(%s): %v", kind, err)
		}
		// Safe by every character test and shaped like no agent's id.
		for _, id := range []string{"not-a-session-id", "x"} {
			if a.RecognizesSessionID(id) {
				t.Errorf("%s.RecognizesSessionID(%q) = true; the adapter recognises anything", kind, id)
			}
		}
	}
}

// TestSpawnCommand_EveryAdapterFollowsItsOptionsNotItsLastSetup is the
// conformance check for the rule that an adapter builds a spawn out of the
// SpawnOptions it is handed, and carries nothing between calls (see the
// contract on session.SpawnOptions.StateDir).
//
// Measured by putting the field back in all three adapters, with SpawnOptions
// left as it is: every kind's subtest fails, and each adapter's own field fails
// only its own kind. (Reverting the struct as well is not the same experiment
// and cannot be run — a SpawnOptions without these fields does not compile
// against this test.)
//
// Written over agent.Kinds() for the same reason as the checks above: a
// per-package test cannot fail for a package that does not exist yet, so a
// fourth adapter that quietly cached its first Setup would reintroduce the
// defect. Measured rather than assumed — such an adapter survives the whole
// unit suite with this test removed, and is killed by it.
//
// Both directions are asserted, and the second is what makes the first mean
// anything: an adapter that carries nothing at all out of its options would
// satisfy "the other directory is absent" vacuously. It therefore fails here,
// which is the intended cost.
//
// ExtraEnv is searched beside Command because an adapter may carry either path
// through either: the opencode adapter exposes its directory only as
// OPENCODE_CONFIG_DIR, and a check reading Command alone would pass it blind.
// Setup still runs for both directories, in the order a process would see them,
// because the adapters answer out of what Setup left on disk — preparing only
// one would leave the assertion unable to tell "follows its options" from
// "found nothing anywhere".
func TestSpawnCommand_EveryAdapterFollowsItsOptionsNotItsLastSetup(t *testing.T) {
	// The Claude Code adapter's Setup records trust for WorkDir in
	// ~/.claude.json, so hand the whole test a home of its own.
	t.Setenv("HOME", t.TempDir())

	for _, kind := range agent.Kinds() {
		t.Run(kind, func(t *testing.T) {
			a, err := agent.Lookup(kind)
			if err != nil {
				t.Fatalf("Lookup(%s): %v", kind, err)
			}

			// Two Managers, in the order the process would see them: each
			// names its own state directory and the hook binary copy it
			// established inside it (session.EstablishHookBinary).
			first, second := t.TempDir(), t.TempDir()
			for _, stateDir := range []string{first, second} {
				err := a.Setup(session.SetupContext{
					StateDir: stateDir,
					ExecPath: filepath.Join(stateDir, "bin", "jin"),
					WorkDir:  t.TempDir(),
				})
				if err != nil {
					t.Fatalf("%s.Setup(%s): %v", kind, stateDir, err)
				}
			}

			// Asked for BOTH directories, from the same adapter, after both
			// Setups have run. One direction is not enough and the gap is not
			// hypothetical: a version of this test that only asked for `first`
			// was measured to let a fourth adapter caching its FIRST context
			// through, because the cached answer and the wanted answer were
			// the same directory. A cache can satisfy at most one of these two
			// requests, whichever end it holds.
			for _, ask := range []struct{ want, other string }{
				{want: first, other: second},
				{want: second, other: first},
			} {
				plan := a.SpawnCommand(session.SpawnOptions{
					StateDir: ask.want,
					ExecPath: filepath.Join(ask.want, "bin", "jin"),
					WorkDir:  t.TempDir(),
				})
				spawn := plan.Command
				for k, v := range plan.ExtraEnv {
					spawn += " " + k + "=" + v
				}
				if !strings.Contains(spawn, ask.want) {
					t.Errorf("%s.SpawnCommand does not name the state dir it was given, %s, so nothing in the spawn came from its options: %s",
						kind, ask.want, spawn)
				}
				if strings.Contains(spawn, ask.other) {
					t.Errorf("%s.SpawnCommand was built for %s but names %s, so it is answering from something it kept rather than from its options: %s",
						kind, ask.want, ask.other, spawn)
				}
			}
		})
	}
}

// TestSpawnCommand_NoAdapterResumesWithoutAnID pins the half of
// SpawnPlan.Resumed that holds for every kind: with no id there is nothing to
// continue, so nothing may report that it did. Manager reads the flag to tell a
// failed resume from a fresh start that died, and a kind that answered true
// here would hand its own fresh spawns the retry meant for stale ids.
//
// It lives in register_test for the reason the test above does: only this
// package can reach every registered adapter at once.
func TestSpawnCommand_NoAdapterResumesWithoutAnID(t *testing.T) {
	for _, kind := range agent.Kinds() {
		a, err := agent.Lookup(kind)
		if err != nil {
			t.Fatalf("Lookup(%s): %v", kind, err)
		}
		for _, started := range []bool{false, true} {
			for _, confirmed := range []bool{false, true} {
				plan := a.SpawnCommand(session.SpawnOptions{
					AgentSessionStarted:     started,
					AgentSessionIDConfirmed: confirmed,
					WorkDir:                 t.TempDir(),
				})
				if plan.Resumed {
					t.Errorf("%s.SpawnCommand(started=%v, confirmed=%v) reports Resumed with an empty AgentSessionID: %q",
						kind, started, confirmed, plan.Command)
				}
			}
		}
	}
}
