package session

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Most assertions in this file are about what was NOT sent.
//
// That is deliberate. "RespondToBlock returned an error" is satisfied just as
// well by an implementation that types a digit into the pane and then reports
// a failure, and that implementation is the bug this whole path exists to
// avoid: a digit landing in a dialog nobody meant to answer, or in the input
// box of a session that was not blocked at all. So the refusal tests count
// key calls rather than trusting the error.

const (
	// A capture whose content is irrelevant — the fake adapter classifies it,
	// not the string. Named so the tests read as "some pane".
	somePane  = "some pane content\n"
	emptyPane = "idle pane\n"
)

// withShortRespond shrinks the respond loop knobs so tests do not pay
// production waits. Same package-var-rewrite caveat as withShortSendVerify:
// not safe under t.Parallel.
func withShortRespond(t *testing.T, poll, budget time.Duration) {
	t.Helper()
	setForTest(t, &respondClearPollDelay, poll)
	setForTest(t, &respondClearBudget, budget)
}

// respondFixture wires a manager whose single session is blocked, with an
// adapter that reports kind and plans steps. capturesInOrder, when non-empty,
// is what CapturePane returns on successive calls, so a test can make the
// dialog disappear (or refuse to).
type respondFixture struct {
	mgr   *Manager
	mock  *mockTmuxRunner
	agent *fakeAgent
	sess  *Session
	pane  string
}

func newRespondFixture(t *testing.T, kind BlockKind, steps []KeyStep, capturesInOrder []string) *respondFixture {
	t.Helper()
	withShortRespond(t, time.Millisecond, 200*time.Millisecond)

	mgr, mock, _ := newTestManager(t)
	pane := "%7"
	sess := newIdleSessionWithPane(t, mgr, t.TempDir(), "blocked", pane)

	ag := &fakeAgent{
		capabilities: AgentCapabilities{
			SchemaVersion:       AgentCapabilitiesSchemaVersion,
			Respond:             CapabilitySupported,
			ReliableNeedsAnswer: CapabilitySupported,
		},
		detectFn: func(capture string) BlockKind {
			// The fake keys off the capture so a test can drive the
			// transition by changing what CapturePane returns.
			if strings.Contains(capture, "CLEARED") {
				return BlockNone
			}
			return kind
		},
		answerFn: func(BlockKind, string, BlockAnswer) ([]KeyStep, error) {
			return steps, nil
		},
	}
	mgr.SetAgentResolver(&fakeAgentResolver{agents: map[string]Agent{"claude": ag}})

	if len(capturesInOrder) > 0 {
		mock.capturedSequence[pane] = capturesInOrder
	} else {
		mock.captured[pane] = somePane
	}
	return &respondFixture{mgr: mgr, mock: mock, agent: ag, sess: sess, pane: pane}
}

// keyCalls counts every key that reached the pane, by either transport. This
// is the number the refusal tests assert is zero.
func (f *respondFixture) keyCalls() int {
	return countCalls(f.mock, "SendKeys", f.pane) + countCalls(f.mock, "SendKeysLiteral", f.pane)
}

func TestRespondToBlock_Option(t *testing.T) {
	f := newRespondFixture(t, BlockPermission, []KeyStep{{Literal: "2"}},
		[]string{somePane, somePane + "CLEARED"})

	kind, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 2})
	if err != nil {
		t.Fatalf("RespondToBlock returned err=%v, want nil", err)
	}
	if kind != BlockPermission {
		t.Errorf("kind = %q, want %q", kind, BlockPermission)
	}
	if n := countCallsWithArgs(f.mock, "SendKeysLiteral", f.pane, "2"); n != 1 {
		t.Errorf("SendKeysLiteral(%q) called %d times, want 1", "2", n)
	}
	if n := countCalls(f.mock, "SendKeys", f.pane); n != 0 {
		t.Errorf("SendKeys called %d times, want 0 — a digit commits on its own", n)
	}
}

func TestRespondToBlock_VerifiedClearResolvesNeedsAnswer(t *testing.T) {
	f := newRespondFixture(t, BlockPermission, []KeyStep{{Literal: "2"}},
		[]string{somePane, somePane + "CLEARED"})
	f.mgr.mu.Lock()
	f.sess.NeedsAnswer = f.sess.NeedsAnswer.required()
	f.mgr.mu.Unlock()

	if _, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 2}); err != nil {
		t.Fatal(err)
	}
	info, _ := f.mgr.GetInfo(f.sess.ID)
	if info.NeedsAnswer.State != NeedsAnswerNotNeeded || info.NeedsAnswer.ResolvedGeneration != 1 {
		t.Errorf("NeedsAnswer = %+v, want resolved generation 1", info.NeedsAnswer)
	}
}

// TestRespondToBlock_IdleStatusIsAllowed pins that status is not the gate.
// A session can sit at idle with a dialog up — recovery derives idle without
// looking at the screen — and refusing those is the defect this avoids.
func TestRespondToBlock_IdleStatusIsAllowed(t *testing.T) {
	f := newRespondFixture(t, BlockQuestion, []KeyStep{{Literal: "1"}},
		[]string{somePane, somePane + "CLEARED"})

	f.mgr.mu.Lock()
	f.sess.Status = StatusIdle
	f.mgr.mu.Unlock()

	if _, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 1}); err != nil {
		t.Fatalf("RespondToBlock on an idle session returned err=%v, want nil", err)
	}
}

func TestRespondToBlock_RequiresConfirmedCapabilityBeforeCapture(t *testing.T) {
	for _, state := range []CapabilityState{CapabilityUnknown, CapabilityUnsupported} {
		t.Run(state.wireValue(), func(t *testing.T) {
			f := newRespondFixture(t, BlockPermission, []KeyStep{{Literal: "1"}}, nil)
			f.agent.capabilities.Respond = state

			if _, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 1}); err == nil {
				t.Fatal("RespondToBlock returned nil, want a capability refusal")
			}
			if n := countCalls(f.mock, "CapturePane", f.pane); n != 0 {
				t.Errorf("CapturePane called %d times, want 0", n)
			}
			if n := f.keyCalls(); n != 0 {
				t.Errorf("key calls = %d, want 0", n)
			}
		})
	}
}

// TestRespondToBlock_DeadStatusesNeverCapture covers the three statuses
// refused outright. The assertion that matters is the second one: they are
// refused before the pane is even read, so no tmux work happens for a session
// that cannot have a prompt.
func TestRespondToBlock_DeadStatusesNeverCapture(t *testing.T) {
	for _, status := range []Status{StatusStopped, StatusCreating, StatusDeleting} {
		t.Run(string(status), func(t *testing.T) {
			f := newRespondFixture(t, BlockPermission, []KeyStep{{Literal: "1"}}, nil)
			f.mgr.mu.Lock()
			f.sess.Status = status
			f.mgr.mu.Unlock()

			if _, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 1}); err == nil {
				t.Fatal("RespondToBlock returned nil, want an error")
			}
			if n := countCalls(f.mock, "CapturePane", f.pane); n != 0 {
				t.Errorf("CapturePane called %d times, want 0", n)
			}
			if n := f.keyCalls(); n != 0 {
				t.Errorf("%d keys sent, want 0", n)
			}
		})
	}
}

// TestRespondToBlock_NoBlockSendsNothing is the guard for the most likely
// misuse: answering a session that is not blocked. The digit would otherwise
// land in its input box and sit there until something submitted it.
func TestRespondToBlock_NoBlockSendsNothing(t *testing.T) {
	f := newRespondFixture(t, BlockNone, []KeyStep{{Literal: "1"}}, nil)
	f.mock.captured[f.pane] = emptyPane
	f.agent.detectFn = func(string) BlockKind { return BlockNone }
	f.agent.answerFn = func(BlockKind, string, BlockAnswer) ([]KeyStep, error) {
		return nil, fmt.Errorf("nothing to answer")
	}

	kind, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 1})
	if err == nil {
		t.Fatal("RespondToBlock returned nil, want an error")
	}
	if kind != BlockNone {
		t.Errorf("kind = %q, want %q", kind, BlockNone)
	}
	if n := f.keyCalls(); n != 0 {
		t.Errorf("%d keys sent, want 0", n)
	}
}

// TestRespondToBlock_UnanswerableKindsSendNothing covers the screens jin
// recognises but will not drive. The error must be the adapter's, because
// only the adapter knows which screen it is looking at and therefore what the
// caller should do instead.
func TestRespondToBlock_UnanswerableKindsSendNothing(t *testing.T) {
	for _, kind := range []BlockKind{BlockQuestionMulti, BlockQuestionSubmit} {
		t.Run(string(kind), func(t *testing.T) {
			adapterMsg := "adapter says: attach and answer " + string(kind)
			f := newRespondFixture(t, kind, nil, nil)
			f.agent.answerFn = func(BlockKind, string, BlockAnswer) ([]KeyStep, error) {
				return nil, fmt.Errorf("%s", adapterMsg)
			}

			gotKind, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 1})
			if err == nil {
				t.Fatal("RespondToBlock returned nil, want an error")
			}
			if !strings.Contains(err.Error(), adapterMsg) {
				t.Errorf("error = %q, want it to carry the adapter's message %q", err, adapterMsg)
			}
			if gotKind != kind {
				t.Errorf("kind = %q, want %q — the caller is told which screen it hit", gotKind, kind)
			}
			if n := f.keyCalls(); n != 0 {
				t.Errorf("%d keys sent, want 0", n)
			}
		})
	}
}

// TestRespondToBlock_UnanswerableKindWithKeysIsRefused covers an adapter bug
// rather than a caller one: planning keys for a kind Manager does not drive.
// Running them would answer one question of a form jin cannot finish, so the
// keys are dropped even though the adapter offered them.
func TestRespondToBlock_UnanswerableKindWithKeysIsRefused(t *testing.T) {
	f := newRespondFixture(t, BlockQuestionMulti, []KeyStep{{Literal: "1"}}, nil)

	if _, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 1}); err == nil {
		t.Fatal("RespondToBlock returned nil, want an error")
	}
	if n := f.keyCalls(); n != 0 {
		t.Errorf("%d keys sent, want 0", n)
	}
}

func TestRespondToBlock_AdapterRefusalSendsNothing(t *testing.T) {
	f := newRespondFixture(t, BlockQuestion, nil, nil)
	f.agent.answerFn = func(BlockKind, string, BlockAnswer) ([]KeyStep, error) {
		return nil, fmt.Errorf("this question offers no free-text entry")
	}

	if _, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Text: "hi"}); err == nil {
		t.Fatal("RespondToBlock returned nil, want an error")
	}
	if n := f.keyCalls(); n != 0 {
		t.Errorf("%d keys sent, want 0", n)
	}
}

// TestRespondToBlock_NoAdapterSendsNothing covers the one place this
// deliberately differs from SendPrompt, which falls back to defaults so the
// transport never refuses a send. There is no default answer to a dialog.
func TestRespondToBlock_NoAdapterSendsNothing(t *testing.T) {
	f := newRespondFixture(t, BlockPermission, []KeyStep{{Literal: "1"}}, nil)
	f.mgr.SetAgentResolver(nil)

	if _, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 1}); err == nil {
		t.Fatal("RespondToBlock returned nil, want an error")
	}
	if n := countCalls(f.mock, "CapturePane", f.pane); n != 0 {
		t.Errorf("CapturePane called %d times, want 0", n)
	}
	if n := f.keyCalls(); n != 0 {
		t.Errorf("%d keys sent, want 0", n)
	}
}

// TestRespondToBlock_VerifyFailureWithholdsEnter is the free-text safety net.
// The typed answer is the only step whose effect can be seen; if it is not
// on screen, the Enter after it would commit whatever the dialog was pointing
// at instead of the answer.
func TestRespondToBlock_VerifyFailureWithholdsEnter(t *testing.T) {
	steps := []KeyStep{
		{Literal: "4"},
		{Literal: "紫がいい", Verify: true},
		{Key: "Enter"},
	}
	// Every capture looks the same, so the typed text never appears.
	f := newRespondFixture(t, BlockQuestion, steps, nil)
	setForTest(t, &respondVerifyLooks, 2)

	_, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Text: "紫がいい"})
	if err == nil {
		t.Fatal("RespondToBlock returned nil, want an error")
	}
	// The message must warn that the field may already hold the text. It used
	// to say "nothing was submitted; the prompt should still be waiting",
	// which reads as an invitation to answer again — and answering again types
	// into the same field, where the second attempt's tail could verify
	// against both answers run together.
	if !strings.Contains(err.Error(), "may already hold") {
		t.Errorf("error = %q, want it to warn that the field may already hold the answer", err)
	}
	if strings.Contains(err.Error(), "answering again") == false {
		t.Errorf("error = %q, want it to steer away from answering again", err)
	}
	if n := countCallsWithArgs(f.mock, "SendKeys", f.pane, "Enter"); n != 0 {
		t.Errorf("Enter sent %d times, want 0 — the answer was never on screen", n)
	}
}

// TestRespondToBlock_VerifySuccessSendsEnter is the mirror: once the text is
// visible, the remaining steps run. Without this, an implementation that
// never sent Enter at all would pass the test above.
func TestRespondToBlock_VerifySuccessSendsEnter(t *testing.T) {
	const answer = "紫がいい"
	steps := []KeyStep{
		{Literal: "4"},
		{Literal: answer, Verify: true},
		{Key: "Enter"},
	}
	f := newRespondFixture(t, BlockQuestion, steps, []string{
		somePane,                      // DetectBlock's capture
		somePane,                      // baseline before the verified step
		somePane + answer,             // the answer is drawn
		somePane + answer + "CLEARED", // and the dialog goes
	})
	setForTest(t, &respondVerifyLooks, 3)

	if _, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Text: answer}); err != nil {
		t.Fatalf("RespondToBlock returned err=%v, want nil", err)
	}
	if n := countCallsWithArgs(f.mock, "SendKeys", f.pane, "Enter"); n != 1 {
		t.Errorf("Enter sent %d times, want 1", n)
	}
}

// TestRespondToBlock_BlockNeverClears covers the timeout. The keys are
// already out by then, so the error has to say the outcome is unknown rather
// than that the answer failed — the difference decides whether a caller is
// safe to retry.
func TestRespondToBlock_BlockNeverClears(t *testing.T) {
	f := newRespondFixture(t, BlockPermission, []KeyStep{{Literal: "1"}}, nil)

	_, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 1})
	if err == nil {
		t.Fatal("RespondToBlock returned nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error = %q, want it to report the outcome as unknown", err)
	}
	if n := countCallsWithArgs(f.mock, "SendKeysLiteral", f.pane, "1"); n != 1 {
		t.Errorf("the answer was sent %d times, want exactly 1 — a retry would answer twice", n)
	}
}

func TestRespondToBlock_SessionNotFound(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	if _, err := mgr.RespondToBlock("nope", BlockAnswer{Option: 1}); err == nil {
		t.Fatal("RespondToBlock returned nil, want an error")
	}
}

// TestRespondToBlock_VerifyIgnoresTextAlreadyOnScreen is the test the review
// found missing, and the case is realistic: `--text "紫"` answering a question
// whose options already list 紫.
//
// The baseline capture is what makes the check mean anything — sendVerifyAppeared
// requires the occurrence count to RISE. Discard the baseline and verification
// passes on text the step never drew, and the Enter after it commits whatever
// the dialog was pointing at instead of the answer.
func TestRespondToBlock_VerifyIgnoresTextAlreadyOnScreen(t *testing.T) {
	const answer = "紫"
	// Present from the very first frame and never redrawn.
	pane := "❯ 1. 紫\n  2. 青\n" + somePane
	steps := []KeyStep{
		{Literal: "3"},
		{Literal: answer, Verify: true},
		{Key: "Enter"},
	}
	f := newRespondFixture(t, BlockQuestion, steps, nil)
	f.mock.captured[f.pane] = pane
	setForTest(t, &respondVerifyLooks, 2)

	if _, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Text: answer}); err == nil {
		t.Fatal("RespondToBlock returned nil; the answer was never drawn, only already present")
	}
	if n := countCallsWithArgs(f.mock, "SendKeys", f.pane, "Enter"); n != 0 {
		t.Errorf("Enter sent %d times, want 0", n)
	}
}

// TestRespondToBlock_RejectsMalformedPlans covers adapter bugs that must be
// caught from the plan alone. Finding one halfway through would mean finding
// it with keys already in the pane, so all of these must send nothing.
func TestRespondToBlock_RejectsMalformedPlans(t *testing.T) {
	tests := []struct {
		name  string
		steps []KeyStep
	}{
		{"empty step", []KeyStep{{}}},
		{"both key and literal", []KeyStep{{Key: "Enter", Literal: "2"}}},
		{"verify with nothing to look for", []KeyStep{{Literal: "1"}, {Key: "Enter", Verify: true}}},
		{"verify on whitespace", []KeyStep{{Literal: "   ", Verify: true}}},
		{"no steps at all", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRespondFixture(t, BlockPermission, tc.steps, nil)
			_, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 1})
			if err == nil {
				t.Fatal("RespondToBlock returned nil, want an error")
			}
			if n := f.keyCalls(); n != 0 {
				t.Errorf("%d keys sent, want 0", n)
			}
			// Asserting only that an error came back is not enough, and the
			// empty-plan case shows why: without its guard the call falls
			// through to the clear poll and still errors — but with
			// ErrBlockNotCleared, whose message tells the operator the keys
			// went out and the outcome is unknown. Nothing went out. A
			// refusal that lies about what it did is worse than the bug.
			if errors.Is(err, ErrBlockNotCleared) {
				t.Errorf("error = %q; a malformed plan must not be reported as an answer "+
					"whose outcome is unknown — no key was sent", err)
			}
		})
	}
}

// TestRespondToBlock_NotClearedIsTyped pins the sentinel rather than the
// sentence. The CLI turns exactly this failure into the timeout exit code that
// the README and the embedded exit-codes doc promise, and it must not depend
// on wording chosen for a person to read.
func TestRespondToBlock_NotClearedIsTyped(t *testing.T) {
	f := newRespondFixture(t, BlockPermission, []KeyStep{{Literal: "1"}}, nil)

	_, err := f.mgr.RespondToBlock(f.sess.ID, BlockAnswer{Option: 1})
	if err == nil {
		t.Fatal("RespondToBlock returned nil, want a not-cleared error")
	}
	if !errors.Is(err, ErrBlockNotCleared) {
		t.Errorf("error %v does not wrap ErrBlockNotCleared", err)
	}
}
