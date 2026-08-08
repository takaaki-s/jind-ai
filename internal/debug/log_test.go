package debug

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withStateHome forces internal/paths to resolve State() to a deterministic
// directory by setting XDG_STATE_HOME for the duration of the test.
func withStateHome(t *testing.T, dir string) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", dir)
	stateDir := stateDirUnder(t, dir)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("failed to create state dir: %v", err)
	}
	return stateDir
}

// stateDirUnder names the directory internal/paths resolves under a given state
// home. Spelled once so a test cannot disagree with the layout it is asserting
// against.
func stateDirUnder(t *testing.T, stateHome string) string {
	t.Helper()
	return filepath.Join(stateHome, "jind-ai")
}

// withEnabled drives the flag NewLogger reads, which no test can arrange
// through the real environment: the process decides once at startup.
func withEnabled(t *testing.T, on bool) {
	t.Helper()
	orig := enabled
	enabled = on
	t.Cleanup(func() { enabled = orig })
}

// withProductionBinary makes NewLogger take the branch a real jin process
// takes. Every test that reaches the writing half needs it, and so does every
// test about the flag: with the guard in force NewLogger declines before the
// flag is consulted, so a check that nothing was written would hold no matter
// what the flag said.
func withProductionBinary(t *testing.T) {
	t.Helper()
	orig := isTestBinary
	isTestBinary = func() bool { return false }
	t.Cleanup(func() { isTestBinary = orig })
}

func TestNewLogger_Disabled(t *testing.T) {
	// When JIN_DEBUG is not "1", the logger should be a no-op.
	origDebug := os.Getenv("JIN_DEBUG")
	os.Setenv("JIN_DEBUG", "0")
	defer os.Setenv("JIN_DEBUG", origDebug)

	withEnabled(t, false)
	withProductionBinary(t)

	stateDir := withStateHome(t, t.TempDir())
	filename := "test-disabled.log"

	log := NewLogger(filename)
	log("this message should not appear")

	logPath := filepath.Join(stateDir, filename)
	if _, err := os.Stat(logPath); err == nil {
		t.Error("logger created a file even though debug is disabled")
	}
}

func TestNewLogger_Enabled(t *testing.T) {
	withEnabled(t, true)
	withProductionBinary(t)

	stateDir := withStateHome(t, t.TempDir())

	log := NewLogger("test-enabled.log")
	log("hello %s %d", "world", 42)

	logPath := filepath.Join(stateDir, "test-enabled.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "hello world 42") {
		t.Errorf("log file content %q does not contain expected message", content)
	}
	// The stamp is TestNewLogger_StampCarriesTheDate's subject. Asserting here
	// that a "[" and a "]" appear would pass for any stamp at all, including
	// none.
}

// TestNewLogger_StampCarriesTheDate pins the part of the line that says when.
// NewLogger's comment has why the date is load-bearing.
func TestNewLogger_StampCarriesTheDate(t *testing.T) {
	withEnabled(t, true)
	withProductionBinary(t)

	stateDir := withStateHome(t, t.TempDir())

	// Millisecond truncation because that is the stamp's resolution: an
	// untruncated `before` can round up past a line written in the same
	// millisecond and fail the window below on nothing.
	before := time.Now().Truncate(time.Millisecond)
	NewLogger("stamp.log")("entry")
	after := time.Now()

	data, err := os.ReadFile(filepath.Join(stateDir, "stamp.log"))
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	line := strings.TrimSpace(string(data))
	end := strings.Index(line, "]")
	if !strings.HasPrefix(line, "[") || end < 0 {
		t.Fatalf("line %q is not of the form [stamp] message", line)
	}
	stamp := line[1:end]

	// Parsing with this layout is the assertion: a clock-only stamp does not
	// satisfy it, and neither does a date-only one.
	got, err := time.ParseInLocation("2006-01-02 15:04:05.000", stamp, time.Local)
	if err != nil {
		t.Fatalf("stamp %q does not carry a full date and time: %v", stamp, err)
	}
	// A parseable stamp could still be a constant. Bounding it to the call
	// makes the test about when the line was written rather than its shape.
	if got.Before(before) || got.After(after) {
		t.Errorf("stamp %s is outside [%s, %s] — it does not report when the line was written",
			got.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
	}
}

func TestNewLogger_NoopWhenDisabled(t *testing.T) {
	withEnabled(t, false)
	withProductionBinary(t)

	log := NewLogger("noop.log")

	// Should not panic
	log("this should be a no-op")
}

// TestNewLogger_WritesNothingFromATestBinary is the guard this package exists
// to hold. It runs with isTestBinary left alone, so the condition it asserts
// under is the real one.
func TestNewLogger_WritesNothingFromATestBinary(t *testing.T) {
	withEnabled(t, true)

	// Point the production resolver somewhere disposable. What is asserted is
	// that nothing is written where that resolver points, and whether it points
	// at the user's real state directory or at this one does not change the
	// claim — but it does decide whether running a build with the guard removed
	// damages the file this test exists to protect.
	stateDir := withStateHome(t, t.TempDir())

	// A weakened answer — the directory refused but the permission granted —
	// resolves to "", and filepath.Join("", name) is a relative path, so the
	// line lands in the working directory instead. Move that somewhere
	// disposable too, and check it, so that form is caught rather than dropped
	// into the source tree.
	//
	// t.Chdir moves the whole process, which is safe here only because this
	// package runs its tests one at a time. Two others resolve paths against
	// the working directory — TestProductionBinaryWritesTheLog reaches testdata
	// and TestNoPackageReadsTheDebugEnvDirectly walks up to go.mod — so adding
	// t.Parallel to anything in this package breaks those rather than this one.
	cwd := t.TempDir()
	t.Chdir(cwd)

	// An assertion that nothing was written passes just as well when nothing
	// was attempted. The flag is the half that can go quiet without the checks
	// below noticing; the guard being in force is TestIsTestBinary_
	// IsWiredToTheRealAnswer's subject, and losing it shows up here as a file.
	if !enabled {
		t.Fatal("precondition: the flag is off, so the guard below is not what stops the write")
	}

	NewLogger("from-a-test-binary.log")("this line must not be written anywhere")

	for _, dir := range []string{stateDir, cwd} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", dir, err)
		}
		for _, e := range entries {
			t.Errorf("a test binary wrote %q into %s", e.Name(), dir)
		}
	}
}

// TestIsTestBinary_IsWiredToTheRealAnswer guards the link the tests that
// replace isTestBinary cannot: a version defined as a constant false would
// satisfy every one of them while leaving the guard above dead.
//
// Its reach stops one mutation short, and saying so is the point. A constant
// true passes this too, and that is the change that turns production logging
// off everywhere. Nothing running inside a test binary can distinguish it —
// TestProductionBinaryWritesTheLog is where that is settled, in a process that
// is not one.
func TestIsTestBinary_IsWiredToTheRealAnswer(t *testing.T) {
	if !isTestBinary() {
		t.Error("isTestBinary() is false inside a test binary; the guard it feeds is disconnected from what it stands for")
	}
}

// TestEnabled_IsNotAffectedByTheTestBinaryGuard pins the separation between the
// two questions: whether the flag is on, and whether this process may write
// where the flag says to.
//
// Folding the guard into Enabled looks like a tightening and is the opposite.
// Enabled is what Manager passes on to the agent it starts, and
// session.TestDebugEnabled_IsWiredToTheRealFlag compares its seam against this
// function — under JIN_DEBUG=1, which is the only condition where that
// comparison can fail. Make Enabled report false here and both sides read
// false, so the check passes for the rest of time without testing anything.
func TestEnabled_IsNotAffectedByTheTestBinaryGuard(t *testing.T) {
	withEnabled(t, true)

	if !isTestBinary() {
		t.Fatal("precondition: this is expected to run in a test binary")
	}
	if !Enabled() {
		t.Error("Enabled() reports false in a test binary with the flag set; " +
			"the flag would stop reaching the agent, and session's seam check would agree with it by accident")
	}

	// The other direction, because a constant true satisfies the check above
	// and is the more damaging half: Manager injects JIN_DEBUG=1 into every
	// agent it starts on this answer, and those are processes that are not test
	// binaries, so the guard does not reach them. An operator who asked for
	// nothing would get the logs of every session in their state directory.
	enabled = false
	if Enabled() {
		t.Error("Enabled() reports true with the flag off; the agents Manager starts would be told to log")
	}
}

// TestNewLogger_WritesNothingWhenTheStateDirCannotBeResolved covers the answer
// logDir passes on from paths, which is a separate decision from the guard
// above and fails in a way the guard cannot catch.
//
// paths.StateOrEmpty reports rather than panics precisely so a logger can
// decline, and dropping that report leaves an empty directory that
// filepath.Join turns into a relative path — so the daemon writes into whatever
// it happened to be started from.
func TestNewLogger_WritesNothingWhenTheStateDirCannotBeResolved(t *testing.T) {
	withEnabled(t, true)
	withProductionBinary(t)

	// Both, because either one alone still resolves: the home directory is the
	// fallback when XDG_STATE_HOME is unset.
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")

	cwd := t.TempDir()
	t.Chdir(cwd)

	// Both preconditions, because this test writes with the test-binary guard
	// deliberately switched off. If the state directory still resolved, the
	// line would land in it — the real one, on a machine where the emptied
	// variables did not take — and the check below only looks at the working
	// directory, so it would report success while doing the damage the whole
	// change exists to stop.
	if dir, ok := logDir(); ok {
		t.Fatalf("precondition: the state directory still resolves, to %s", dir)
	}
	if isTestBinary() {
		t.Fatal("precondition: the production branch is not in force, so this proves nothing")
	}

	NewLogger("unresolvable.log")("this line has nowhere to go")

	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("an unresolved state directory sent %q to the working directory", e.Name())
	}
}

// TestUntrusted covers both halves of the one verb: the bound and the quoting.
// They are one function because they only work together — a bound alone leaves
// newlines intact, and a value carrying them forges whole entries in a log read
// as jind-ai's own.
func TestUntrusted(t *testing.T) {
	short := "ses_084426f78ffeXBrPh5ABEu2dNX"
	if got, want := Untrusted(short, 128), `"`+short+`"`; got != want {
		t.Errorf("Untrusted(%q, 128) = %s, want %s", short, got, want)
	}
	if got, want := Untrusted(short, len(short)), `"`+short+`"`; got != want {
		t.Errorf("Untrusted at exactly len = %s, want %s", got, want)
	}
	// The quote is what a bound cannot do: without it a single value ends the
	// line and starts one that reads as jind-ai's own.
	if got := Untrusted("a\n[HOOK] forged", 128); strings.Contains(got, "\n") {
		t.Errorf("Untrusted left a raw newline in %s", got)
	}
	// Quoting adds two characters, so the bound applies to the value, not the
	// rendering: 512 in, 128 of value plus the quotes out.
	long := strings.Repeat("a", 512)
	if got := Untrusted(long, 128); len(got) != 130 {
		t.Errorf("Untrusted(512 chars, 128) rendered %d chars, want 130 (128 + two quotes)", len(got))
	}
	if got, want := Untrusted("", 128), `""`; got != want {
		t.Errorf("Untrusted(\"\", 128) = %s, want %s", got, want)
	}
	// A negative bound means "no bound" rather than a panic: a logger has no
	// business crashing the process it was recording for.
	if got, want := Untrusted(long, -1), `"`+long+`"`; got != want {
		t.Errorf("Untrusted with a negative bound truncated to %d chars", len(got))
	}
}

// TestUntrustedBytes exists because the string conversion is the expensive
// half: converting first copies the whole payload onto the heap before the
// bound throws it away, on precisely the input the bound exists to survive.
func TestUntrustedBytes(t *testing.T) {
	if got, want := UntrustedBytes([]byte("abc"), 128), `"abc"`; got != want {
		t.Errorf("UntrustedBytes = %s, want %s", got, want)
	}
	big := []byte(strings.Repeat("b", 1<<20))
	if got := UntrustedBytes(big, 64); len(got) != 66 {
		t.Errorf("UntrustedBytes(1MiB, 64) rendered %d chars, want 66", len(got))
	}
	if got := UntrustedBytes(nil, 64); got != `""` {
		t.Errorf("UntrustedBytes(nil, 64) = %s, want %s", got, `""`)
	}
}
