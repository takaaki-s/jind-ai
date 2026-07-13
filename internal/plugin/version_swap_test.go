package plugin

import (
	"testing"

	"github.com/takaaki-s/jind-ai/internal/version"
)

// setJinVersionForTest overrides the ldflags-driven internal/version.Version
// for the duration of the test. Returns a restore function callers defer.
// Needed by tests that exercise the jin compat rejection path — the default
// "dev" is treated as "satisfies everything" so a real version has to be
// pinned to trip the check.
func setJinVersionForTest(t *testing.T, v string) func() {
	t.Helper()
	prev := version.Version
	version.Version = v
	return func() { version.Version = prev }
}
