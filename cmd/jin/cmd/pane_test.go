package cmd

import (
	"strings"
	"testing"
)

// TestPanePopupCmd_HereFlagRegistered verifies the --here flag exists on the
// popup subcommand.
func TestPanePopupCmd_HereFlagRegistered(t *testing.T) {
	if panePopupCmd.Flags().Lookup("here") == nil {
		t.Error("panePopupCmd is missing the --here flag")
	}
}

// TestRunPopupHere_NoTmuxClient verifies that --here fails with a clear error
// when neither $TMUX nor JIN_CALLER_TMUX_SOCKET can resolve a server socket.
func TestRunPopupHere_NoTmuxClient(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("JIN_CALLER_TMUX_SOCKET", "")
	t.Setenv("JIN_CALLER_TMUX_PANE", "")

	err := runPopupHere("echo hi", "", "", "")
	if err == nil {
		t.Fatal("expected error when no tmux client is resolvable, got nil")
	}
	if !strings.Contains(err.Error(), "requires a tmux client") {
		t.Errorf("error = %q, want to mention %q", err.Error(), "requires a tmux client")
	}
}
