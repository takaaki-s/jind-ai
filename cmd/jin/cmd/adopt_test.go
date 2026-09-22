package cmd

import (
	"testing"

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

func TestAdoptOptionsRequiresExplicitPhaseAndAgent(t *testing.T) {
	resetAdoptFlags(t)
	if _, err := adoptOptions(adoptCmd, "%1"); err == nil {
		t.Fatal("adoptOptions accepted neither phase")
	}
	_ = adoptCmd.Flags().Set("dry-run", "true")
	if _, err := adoptOptions(adoptCmd, "%1"); err == nil {
		t.Fatal("adoptOptions accepted missing --agent")
	}
	_ = adoptCmd.Flags().Set("agent", "claude")
	opts, err := adoptOptions(adoptCmd, "%1")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Server.Kind != tmux.ServerDefault || !opts.DryRun {
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
