//go:build e2e

package session

import (
	"testing"

	"github.com/takaaki-s/jind-ai/internal/testutil"
	"github.com/takaaki-s/jind-ai/internal/tmux"
)

func TestE2E_AdoptKeepsPIDAndDeleteLeavesForeignPaneAlive(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	socket := testutil.TmuxSocket(t)
	foreign, err := tmux.NewClientWithSocket(socket)
	if err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	workDir := t.TempDir()
	if err := foreign.NewSessionWithCmdInDir("foreign", 100, 30, workDir, "sleep 60"); err != nil {
		t.Skipf("cannot start tmux server: %v", err)
	}
	paneID, err := foreign.GetPaneID("foreign")
	if err != nil {
		t.Fatal(err)
	}
	server := tmux.ServerRef{Kind: tmux.ServerName, Value: socket}
	mgr.SetTmuxFactory(func(got tmux.ServerRef) (tmux.Runner, error) {
		if got != server {
			t.Fatalf("server = %+v, want %+v", got, server)
		}
		return foreign, nil
	})
	opts := AdoptOptions{Server: server, Target: paneID, AgentKind: "claude"}
	preview, err := mgr.PreviewAdoption(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Pane.ProcessAncestry) == 0 {
		t.Fatal("preview omitted process ancestry")
	}
	originalPID := preview.Pane.PanePID
	opts.ConfirmationKey = preview.ConfirmationKey
	info, err := mgr.AdoptPane(opts)
	if err != nil {
		t.Fatal(err)
	}
	afterAdopt, err := foreign.InspectPane(paneID)
	if err != nil {
		t.Fatal(err)
	}
	if afterAdopt.PanePID != originalPID {
		t.Fatalf("pane PID changed during adoption: %d -> %d", originalPID, afterAdopt.PanePID)
	}
	if err := mgr.Delete(info.ID, false, false); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := foreign.InspectPane(paneID)
	if err != nil {
		t.Fatalf("foreign pane died on record delete: %v", err)
	}
	if afterDelete.PanePID != originalPID {
		t.Fatalf("foreign pane PID changed on delete: %d -> %d", originalPID, afterDelete.PanePID)
	}
}
