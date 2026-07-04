package git

import (
	"os/exec"
	"strings"
	"testing"
)

func TestExecRunner_Run_reportsMissingCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	r := execRunner{}
	out, err := r.Run(dir, "this-subcommand-does-not-exist")
	if err == nil {
		t.Fatalf("expected error for bogus subcommand, got nil (output=%q)", out)
	}
	// git surfaces the failure reason on stderr, which CombinedOutput must include.
	if len(out) == 0 {
		t.Errorf("expected non-empty combined output, got empty")
	}
	if !strings.Contains(string(out), "this-subcommand-does-not-exist") {
		t.Logf("combined output: %s", out)
	}
}
