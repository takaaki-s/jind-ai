package session

import (
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/tmux"
)

func adoptedPane(path string) tmux.PaneInfo {
	return tmux.PaneInfo{
		SessionID: "$7", SessionName: "existing", WindowID: "@4", WindowName: "agent",
		WindowIndex: 2, PaneID: "%19", PaneIndex: 1, PanePID: 4242, PaneStarted: "Sun Sep 17 10:00:00 2026",
		CurrentCommand: "claude", StartCommand: "claude --model opus", CurrentPath: path,
		ProcessAncestry: []tmux.PaneProcess{{PID: 4242, PPID: 1, Command: "zsh"}, {PID: 4243, PPID: 4242, Command: "claude"}},
	}
}

func adoptionFixture(t *testing.T) (*Manager, *mockTmuxRunner, AdoptOptions) {
	t.Helper()
	mgr, _, _ := newTestManager(t)
	foreign := newMockTmuxRunner()
	server := tmux.ServerRef{Kind: tmux.ServerPath, Value: "/tmp/tmux-test/default"}
	foreign.paneInfos["%19"] = adoptedPane(t.TempDir())
	mgr.SetTmuxFactory(func(got tmux.ServerRef) (tmux.Runner, error) {
		if got != server {
			t.Fatalf("tmux factory server = %+v, want %+v", got, server)
		}
		return foreign, nil
	})
	return mgr, foreign, AdoptOptions{Server: server, Target: "%19", AgentKind: "claude"}
}

func TestPreviewAndAdoptPane_PreservesForeignPaneAndNegotiatesCapabilities(t *testing.T) {
	mgr, foreign, opts := adoptionFixture(t)
	preview, err := mgr.PreviewAdoption(opts)
	if err != nil {
		t.Fatalf("PreviewAdoption: %v", err)
	}
	if preview.Owner != nil {
		t.Fatalf("preview owner = %+v, want unowned", preview.Owner)
	}
	if preview.Pane.PanePID != 4242 || preview.ConfirmationKey == "" {
		t.Fatalf("preview = %+v", preview)
	}
	if got := preview.Capabilities.State(CapabilityLiveness); got != CapabilitySupported {
		t.Errorf("liveness = %s, want supported", got)
	}
	for _, capability := range []AgentCapability{CapabilitySend, CapabilityRespond, CapabilityResume, CapabilityHooks, CapabilityTranscript, CapabilityReliableNeedsAnswer} {
		if got := preview.Capabilities.State(capability); got != CapabilityUnsupported {
			t.Errorf("%s = %s, want unsupported", capability, got)
		}
	}
	if len(mgr.List()) != 0 {
		t.Fatal("dry-run mutated session state")
	}
	if len(foreign.calls) != 1 || foreign.calls[0].method != "InspectPane" {
		t.Fatalf("dry-run tmux calls = %+v, want one InspectPane", foreign.calls)
	}

	opts.ConfirmationKey = preview.ConfirmationKey
	info, err := mgr.AdoptPane(opts)
	if err != nil {
		t.Fatalf("AdoptPane: %v", err)
	}
	if info.TmuxBinding.Ownership != TmuxOwnershipAdopted || info.TmuxBinding.PanePID != preview.Pane.PanePID {
		t.Fatalf("adopted binding = %+v", info.TmuxBinding)
	}
	if info.TmuxPaneID != "%19" || info.TmuxWindowName != "existing" {
		t.Fatalf("adopted tmux target = %s / %s", info.TmuxWindowName, info.TmuxPaneID)
	}
	for _, call := range foreign.calls {
		if call.method != "InspectPane" {
			t.Fatalf("adoption mutated tmux via %+v", call)
		}
	}

	retry, err := mgr.AdoptPane(opts)
	if err != nil {
		t.Fatalf("idempotent AdoptPane retry: %v", err)
	}
	if retry.ID != info.ID || len(mgr.List()) != 1 {
		t.Fatalf("retry created another record: first=%s retry=%s count=%d", info.ID, retry.ID, len(mgr.List()))
	}
	if err := mgr.Kill(info.ID); err == nil || !strings.Contains(err.Error(), "externally owned") {
		t.Fatalf("Kill(adopted) error = %v", err)
	}
	if err := mgr.Delete(info.ID, false, false); err != nil {
		t.Fatalf("Delete(adopted): %v", err)
	}
	if foreign.hasCalledWith("KillSession", "existing") || foreign.hasCalledWith("TerminatePaneProcess", "%19") {
		t.Fatalf("delete/kill touched foreign pane: %+v", foreign.calls)
	}
}

func TestAdoptPane_RejectsChangedPaneAndDuplicateOwner(t *testing.T) {
	mgr, foreign, opts := adoptionFixture(t)
	preview, err := mgr.PreviewAdoption(opts)
	if err != nil {
		t.Fatal(err)
	}
	changed := foreign.paneInfos["%19"]
	changed.PanePID++
	foreign.paneInfos["%19"] = changed
	opts.ConfirmationKey = preview.ConfirmationKey
	if _, err := mgr.AdoptPane(opts); err == nil || !strings.Contains(err.Error(), "changed or moved") {
		t.Fatalf("changed-pane error = %v", err)
	}
	if len(mgr.List()) != 0 {
		t.Fatal("changed pane produced a session record")
	}
}

func TestRecoverAdoptedPane_DetachesMovedIdentityWithoutTmuxMutation(t *testing.T) {
	mgr, foreign, opts := adoptionFixture(t)
	preview, _ := mgr.PreviewAdoption(opts)
	opts.ConfirmationKey = preview.ConfirmationKey
	info, err := mgr.AdoptPane(opts)
	if err != nil {
		t.Fatal(err)
	}
	changed := foreign.paneInfos["%19"]
	changed.WindowID = "@99"
	foreign.paneInfos["%19"] = changed
	mgr.RecoverTmuxSessions()
	got, ok := mgr.GetInfo(info.ID)
	if !ok || got.Status != StatusStopped || !strings.Contains(got.ErrorMessage, "identity changed") {
		t.Fatalf("recovered info = %+v, ok=%v", got, ok)
	}
	if got.TmuxPaneID != "%19" || got.TmuxBinding.IdentityKey == "" {
		t.Fatalf("recovery erased forensic binding: %+v", got.TmuxBinding)
	}
	for _, call := range foreign.calls {
		if call.method != "InspectPane" {
			t.Fatalf("recovery mutated foreign tmux: %+v", call)
		}
	}
	if err := mgr.StartBackground(info.ID); err == nil || !strings.Contains(err.Error(), "cannot be restarted") {
		t.Fatalf("StartBackground(adopted stopped) error = %v", err)
	}
}

func TestAdoptedSession_RejectsWorktreeRemovalAndPaneMutationHelpers(t *testing.T) {
	mgr, _, opts := adoptionFixture(t)
	preview, _ := mgr.PreviewAdoption(opts)
	opts.ConfirmationKey = preview.ConfirmationKey
	info, err := mgr.AdoptPane(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.PreCheckDelete(info.ID, true, false); err == nil || !strings.Contains(err.Error(), "adopted") {
		t.Fatalf("PreCheckDelete(remove worktree) error = %v", err)
	}
	if _, err := mgr.PaneSplit(info.ID, "logs", "", tmux.SplitOptions{}); err == nil {
		t.Fatal("PaneSplit accepted adopted pane")
	}
	if err := mgr.PaneClose(info.ID, "logs"); err == nil {
		t.Fatal("PaneClose accepted adopted pane")
	}
	if err := mgr.PanePopup(info.ID, "true", "", "", ""); err == nil {
		t.Fatal("PanePopup accepted adopted pane")
	}
}
