// Command logprobe writes one debug line and exits.
//
// It exists so a test can observe the one thing a test cannot observe about
// itself: that NewLogger still writes when the process is not a test binary.
// Inside a test binary the guard is always in force, so the production branch
// is unreachable there and a change that removed it would look exactly like a
// change that did not.
//
// It lives under testdata so the go tool leaves it out of ./... — it is not
// part of the package's build, and it cannot be a module of its own because
// internal/debug is importable only from inside this one.
//
// The log filename comes from the command line rather than a constant shared
// with the test, because it cannot be shared: this is a separate main package
// and the name would have to be duplicated to be agreed on.
//
// One thing to know before using it to check that a change is caught: the test
// compiles this from the working tree in a go invocation of its own, so a
// -overlay passed to `go test` does not reach it and every mutation it is meant
// to catch appears to survive. Pass the overlay in GOFLAGS instead, so the
// child inherits it.
package main

import (
	"fmt"
	"os"

	"github.com/takaaki-s/jind-ai/internal/debug"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <log-filename>\n", os.Args[0])
		os.Exit(2)
	}
	debug.NewLogger(os.Args[1])("probe %s %d", "line", 42)
}
