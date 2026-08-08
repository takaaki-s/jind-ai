package debug

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/paths"
)

// enabled is true when JIN_DEBUG=1 is set.
var enabled = os.Getenv("JIN_DEBUG") == "1"

// Enabled reports whether debug logging is on for this process.
//
// It exists so that the one place that has to pass the flag on to a child —
// Manager, building the agent's environment — asks this package rather than
// re-reading the variable and re-deciding what counts as on. Two readings of
// the same variable can disagree, and the shape of the disagreement is a child
// that logs when the parent does not, or the reverse.
//
// It answers only "is the flag on", and deliberately does not consult
// isTestBinary: the answer is what gets passed to a child process, and a test
// that checks the flag reaches the child needs the flag to still be on when it
// looks. Where the lines are allowed to land is logDir's question, not this
// one.
func Enabled() bool { return enabled }

// isTestBinary reports whether this process is a test binary built by `go test`.
//
// A variable only so this package's tests can force the production branch,
// which is otherwise unreachable from where they run. Production never
// reassigns it. TestProductionBinaryWritesTheLog covers what an in-process test
// cannot: inside a test binary this and a constant true are indistinguishable.
var isTestBinary = testing.Testing

// logDir returns the directory NewLogger writes into, and whether it may write
// at all.
//
// A test binary gets nothing, and there is nothing for a test to opt into: the
// loggers this serves are package-level variables, so their path is resolved
// before TestMain runs and a test cannot redirect one for itself. The Debug
// Logging section of docs/conventions.md has what that cost when it was not
// refused here.
func logDir() (string, bool) {
	if isTestBinary() {
		return "", false
	}
	return paths.StateOrEmpty()
}

// Untrusted renders a value the local process did not choose so that it is safe
// to put in a log line: quoted, and bounded to max bytes. Use it with %s — it
// has already quoted the value.
//
// One verb rather than a bound and a quote, because the two only work together
// and remembering both at every call site is a rule that has already been
// broken once. A bound alone leaves newlines intact, and a value carrying them
// forges whole entries in a log that is read as jind-ai's own; a quote alone
// lets one payload fill the file.
//
// Cutting on a byte boundary may split a rune, and strconv.Quote renders the
// remnant as an escape. That is the right trade for a value being reported as
// suspect. A negative max means "no bound" rather than a panic: a logger has no
// business crashing the process it was recording for.
func Untrusted(s string, max int) string {
	if max >= 0 && len(s) > max {
		// Clone, so the short result does not pin the whole original: a
		// slice shares its backing array, and these values are exactly the
		// ones that can be arbitrarily large. Only the abnormal branch pays
		// for it.
		s = strings.Clone(s[:max])
	}
	return strconv.Quote(s)
}

// UntrustedBytes is Untrusted for a value that has not been turned into a string
// yet. Converting first would copy the whole thing onto the heap before the
// bound threw it away — measured at ~1MB and ~3000x the time for a 1MiB payload,
// on precisely the input the bound exists to survive.
func UntrustedBytes(b []byte, max int) string {
	if max >= 0 && len(b) > max {
		b = b[:max]
	}
	return strconv.Quote(string(b))
}

// NewLogger returns a debug logging function that writes to
// $XDG_STATE_HOME/jind-ai/<filename> (default ~/.local/state/jind-ai/<filename>)
// when JIN_DEBUG=1 is set.
// When debugging is disabled, the state directory cannot be resolved, or this
// process is a test binary, the returned function is a no-op.
func NewLogger(filename string) func(string, ...any) {
	if !enabled {
		return func(string, ...any) {}
	}

	stateDir, ok := logDir()
	if !ok {
		return func(string, ...any) {}
	}
	logPath := filepath.Join(stateDir, filename)

	return func(format string, args ...any) {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		defer f.Close()
		msg := fmt.Sprintf(format, args...)
		// The date is load-bearing, not decoration. These files are appended
		// to and never rotated, so one of them accumulates days of a long-lived
		// daemon; a clock-only stamp leaves no way to order two lines that read
		// 20:41 and 05:51, and the file is the only record of when anything
		// happened. Reconstructing the boundaries afterwards means scanning
		// backwards for the points where time runs backwards, with a threshold
		// to tell a day rollover from the small out-of-order writes concurrent
		// goroutines produce — which is guesswork the writer can just avoid.
		fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("2006-01-02 15:04:05.000"), msg)
	}
}
