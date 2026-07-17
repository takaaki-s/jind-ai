package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/tmux"
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

// TestPopupSizeWithEnvFallback_FlagOverridesEnv verifies an explicit flag
// value wins over the JIN_PLUGIN_POPUP_* env vars.
func TestPopupSizeWithEnvFallback_FlagOverridesEnv(t *testing.T) {
	t.Setenv("JIN_PLUGIN_POPUP_WIDTH", "80%")
	t.Setenv("JIN_PLUGIN_POPUP_HEIGHT", "80%")

	width, height := popupSizeWithEnvFallback("30%", "40%")
	if width != "30%" {
		t.Errorf("width = %q, want %q", width, "30%")
	}
	if height != "40%" {
		t.Errorf("height = %q, want %q", height, "40%")
	}
}

// TestPopupSizeWithEnvFallback_UsesEnvWhenFlagEmpty verifies the env var is
// used when the corresponding flag was left unset.
func TestPopupSizeWithEnvFallback_UsesEnvWhenFlagEmpty(t *testing.T) {
	t.Setenv("JIN_PLUGIN_POPUP_WIDTH", "80%")
	t.Setenv("JIN_PLUGIN_POPUP_HEIGHT", "60%")

	width, height := popupSizeWithEnvFallback("", "")
	if width != "80%" {
		t.Errorf("width = %q, want %q", width, "80%")
	}
	if height != "60%" {
		t.Errorf("height = %q, want %q", height, "60%")
	}
}

// TestPopupSizeWithEnvFallback_EmptyWhenBothMissing verifies both values
// stay empty when neither the flag nor the env var is set, so tmux falls
// back to its own default.
func TestPopupSizeWithEnvFallback_EmptyWhenBothMissing(t *testing.T) {
	t.Setenv("JIN_PLUGIN_POPUP_WIDTH", "")
	t.Setenv("JIN_PLUGIN_POPUP_HEIGHT", "")

	width, height := popupSizeWithEnvFallback("", "")
	if width != "" {
		t.Errorf("width = %q, want empty", width)
	}
	if height != "" {
		t.Errorf("height = %q, want empty", height)
	}
}

// TestPopupSizeWithEnvFallback_Table exercises width/height fallback
// precedence independently via a table of flag/env combinations.
func TestPopupSizeWithEnvFallback_Table(t *testing.T) {
	tests := []struct {
		name       string
		flagWidth  string
		flagHeight string
		envWidth   string
		envHeight  string
		wantWidth  string
		wantHeight string
	}{
		{
			name:       "both flags set, env ignored",
			flagWidth:  "50%",
			flagHeight: "50%",
			envWidth:   "90%",
			envHeight:  "90%",
			wantWidth:  "50%",
			wantHeight: "50%",
		},
		{
			name:       "only width flag set",
			flagWidth:  "50%",
			flagHeight: "",
			envWidth:   "90%",
			envHeight:  "90%",
			wantWidth:  "50%",
			wantHeight: "90%",
		},
		{
			name:       "only height flag set",
			flagWidth:  "",
			flagHeight: "50%",
			envWidth:   "90%",
			envHeight:  "90%",
			wantWidth:  "90%",
			wantHeight: "50%",
		},
		{
			name:       "no flags, no env",
			flagWidth:  "",
			flagHeight: "",
			envWidth:   "",
			envHeight:  "",
			wantWidth:  "",
			wantHeight: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("JIN_PLUGIN_POPUP_WIDTH", tt.envWidth)
			t.Setenv("JIN_PLUGIN_POPUP_HEIGHT", tt.envHeight)

			width, height := popupSizeWithEnvFallback(tt.flagWidth, tt.flagHeight)
			if width != tt.wantWidth {
				t.Errorf("width = %q, want %q", width, tt.wantWidth)
			}
			if height != tt.wantHeight {
				t.Errorf("height = %q, want %q", height, tt.wantHeight)
			}
		})
	}
}

// TestPaneSplitCmd_FlagsRegistered verifies the redesigned split flag set,
// including the deprecated aliases.
func TestPaneSplitCmd_FlagsRegistered(t *testing.T) {
	for _, name := range []string{"here", "direction", "size", "full", "no-focus", "name", "if-exists", "horizontal", "percent"} {
		if paneSplitCmd.Flags().Lookup(name) == nil {
			t.Errorf("paneSplitCmd is missing the --%s flag", name)
		}
	}
	for _, name := range []string{"horizontal", "percent"} {
		if f := paneSplitCmd.Flags().Lookup(name); f != nil && f.Deprecated == "" {
			t.Errorf("--%s should be marked deprecated", name)
		}
	}
}

// TestPaneCloseCmd_FlagsRegistered verifies the close subcommand and its flags.
func TestPaneCloseCmd_FlagsRegistered(t *testing.T) {
	for _, name := range []string{"here", "name"} {
		if paneCloseCmd.Flags().Lookup(name) == nil {
			t.Errorf("paneCloseCmd is missing the --%s flag", name)
		}
	}
}

// newSplitFlagsCmd returns a throwaway command carrying the split flag set, so
// splitGeometryFromFlags can be exercised without mutating paneSplitCmd.
func newSplitFlagsCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "split"}
	registerSplitFlags(cmd)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags(%v) failed: %v", args, err)
	}
	return cmd
}

func TestSplitGeometryFromFlags(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantDirection string
		wantSize      string
		wantErr       string
	}{
		{
			name:          "defaults",
			args:          nil,
			wantDirection: "down",
			wantSize:      "",
		},
		{
			name:          "new flags pass through",
			args:          []string{"--direction", "left", "--size", "25%"},
			wantDirection: "left",
			wantSize:      "25%",
		},
		{
			name:          "deprecated horizontal maps to right",
			args:          []string{"--horizontal"},
			wantDirection: "right",
		},
		{
			name:          "deprecated percent maps to size",
			args:          []string{"--percent", "40"},
			wantDirection: "down",
			wantSize:      "40%",
		},
		{
			name:    "horizontal conflicts with direction",
			args:    []string{"--horizontal", "--direction", "up"},
			wantErr: "--horizontal conflicts with --direction",
		},
		{
			name:    "percent conflicts with size",
			args:    []string{"--percent", "40", "--size", "30%"},
			wantErr: "--percent conflicts with --size",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newSplitFlagsCmd(t, tt.args...)
			direction, size, err := splitGeometryFromFlags(cmd)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if direction != tt.wantDirection {
				t.Errorf("direction = %q, want %q", direction, tt.wantDirection)
			}
			if size != tt.wantSize {
				t.Errorf("size = %q, want %q", size, tt.wantSize)
			}
		})
	}
}

func TestValidateSlotFlags(t *testing.T) {
	tests := []struct {
		name     string
		slotName string
		ifExists string
		wantErr  string
	}{
		{"no slot flags", "", "", ""},
		{"name alone", "demo", "", ""},
		{"name with respawn", "demo", "respawn", ""},
		{"if-exists without name", "", "respawn", "--if-exists requires --name"},
		{"invalid if-exists", "demo", "maybe", "invalid if-exists"},
		{"invalid name", "has space", "", "invalid pane name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tmux.ValidateSlotOptions(tt.slotName, tt.ifExists, tmux.SplitOptions{})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestRunSplitHere_NoTmuxClient mirrors TestRunPopupHere_NoTmuxClient for the
// split --here path.
func TestRunSplitHere_NoTmuxClient(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("JIN_CALLER_TMUX_SOCKET", "")
	t.Setenv("JIN_CALLER_TMUX_PANE", "")

	_, err := runSplitHere(tmux.SplitOptions{Cmd: "echo hi"}, "", "")
	if err == nil {
		t.Fatal("expected error when no tmux client is resolvable, got nil")
	}
	if !strings.Contains(err.Error(), "requires a tmux client") {
		t.Errorf("error = %q, want to mention %q", err.Error(), "requires a tmux client")
	}
}

// TestRunCloseHere_NoTmuxClient verifies close --here fails cleanly outside tmux.
func TestRunCloseHere_NoTmuxClient(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("JIN_CALLER_TMUX_SOCKET", "")
	t.Setenv("JIN_CALLER_TMUX_PANE", "")

	if err := runCloseHere("demo"); err == nil {
		t.Fatal("expected error when no tmux client is resolvable, got nil")
	}
}
