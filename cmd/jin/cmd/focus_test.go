package cmd

import "testing"

// TestFocusCmd_RegisteredOnSession verifies `jin session focus` is wired up as a
// subcommand of `jin session` and declares an argument validator.
func TestFocusCmd_RegisteredOnSession(t *testing.T) {
	var registered bool
	for _, sub := range sessionCmd.Commands() {
		if sub.Name() == "focus" {
			registered = true
			break
		}
	}
	if !registered {
		t.Fatal("sessionCmd is missing the focus subcommand")
	}
	if focusCmd.Args == nil {
		t.Error("focusCmd.Args is nil, want cobra.ExactArgs(1)")
	}
}
