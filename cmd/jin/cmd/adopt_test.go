package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/tmux"
)

func resetAdoptFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		for name, value := range map[string]string{
			"agent": "", "description": "", "fleet": "", "tmux-socket": "",
			"tmux-socket-path": "", "confirmation-key": "", "dry-run": "false", "confirm": "false",
		} {
			_ = adoptCmd.Flags().Set(name, value)
		}
	})
}

func TestPrintAdoptionPreview_AmbiguousRequiresExplicitPreview(t *testing.T) {
	var out bytes.Buffer
	old := adoptCmd.OutOrStdout()
	adoptCmd.SetOut(&out)
	t.Cleanup(func() { adoptCmd.SetOut(old) })

	printAdoptionPreview(adoptCmd, &daemon.AdoptResponse{Preview: session.AdoptionPreview{
		Server: tmux.ServerRef{Kind: tmux.ServerDefault},
		Pane: tmux.PaneInfo{
			SessionID: "$1", SessionName: "work", WindowID: "@1", PaneID: "%1",
			PanePID: 42, PaneStarted: "now", CurrentPath: "/repo", CurrentCommand: "codex",
		},
		Detection: session.AgentDetection{
			Provenance: session.AgentDetectionAmbiguous,
			Candidates: []session.AgentDetectionCandidate{{Kind: "codex", Score: 400}, {Kind: "claude", Score: 300}},
		},
		Capabilities:    session.AgentCapabilities{SchemaVersion: session.AgentCapabilitiesSchemaVersion, Liveness: session.CapabilitySupported},
		ConfirmationKey: "key",
	}})
	got := out.String()
	for _, want := range []string{"Agent:   ambiguous", "codex (score 400)", "claude (score 300)", "Re-run --dry-run with --agent"} {
		if !strings.Contains(got, want) {
			t.Errorf("preview missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Repeat with --confirm") {
		t.Fatalf("ambiguous preview offered confirmation:\n%s", got)
	}
}

func TestAdoptOptionsRequiresExplicitPhaseAndAllowsDetection(t *testing.T) {
	resetAdoptFlags(t)
	if _, err := adoptOptions(adoptCmd, "%1"); err == nil {
		t.Fatal("adoptOptions accepted neither phase")
	}
	_ = adoptCmd.Flags().Set("dry-run", "true")
	opts, err := adoptOptions(adoptCmd, "%1")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Server.Kind != tmux.ServerDefault || !opts.DryRun || opts.AgentKind != "" {
		t.Fatalf("options = %+v", opts)
	}
	_ = adoptCmd.Flags().Set("dry-run", "false")
	_ = adoptCmd.Flags().Set("confirm", "true")
	if _, err := adoptOptions(adoptCmd, "%1"); err == nil {
		t.Fatal("confirm accepted missing confirmation key")
	}
}

func TestAdoptOptionsResolvesCurrentAndExplicitTmuxServers(t *testing.T) {
	t.Run("current tmux", func(t *testing.T) {
		resetAdoptFlags(t)
		t.Setenv("TMUX", "/tmp/tmux-current,123,0")
		_ = adoptCmd.Flags().Set("dry-run", "true")
		_ = adoptCmd.Flags().Set("agent", "claude")
		opts, err := adoptOptions(adoptCmd, "work:1.2")
		if err != nil {
			t.Fatal(err)
		}
		if opts.Server != (tmux.ServerRef{Kind: tmux.ServerPath, Value: "/tmp/tmux-current"}) {
			t.Fatalf("server = %+v", opts.Server)
		}
	})
	t.Run("named override", func(t *testing.T) {
		resetAdoptFlags(t)
		t.Setenv("TMUX", "/tmp/tmux-current,123,0")
		_ = adoptCmd.Flags().Set("dry-run", "true")
		_ = adoptCmd.Flags().Set("agent", "claude")
		_ = adoptCmd.Flags().Set("tmux-socket", "agents")
		opts, err := adoptOptions(adoptCmd, "%9")
		if err != nil {
			t.Fatal(err)
		}
		if opts.Server != (tmux.ServerRef{Kind: tmux.ServerName, Value: "agents"}) {
			t.Fatalf("server = %+v", opts.Server)
		}
	})
}
