package session

import (
	"testing"

	"github.com/takaaki-s/jind-ai/internal/tmux"
)

// paneTestSession creates a stopped session and sets its tmux fields under the
// manager lock, mirroring how running sessions are simulated elsewhere in this
// package (see TestManager_Kill).
func paneTestSession(t *testing.T, mgr *Manager, workDir, paneID, windowName string) *Session {
	t.Helper()
	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: workDir, Description: "pane-test"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.mu.Lock()
	sess.TmuxPaneID = paneID
	sess.TmuxWindowName = windowName
	mgr.mu.Unlock()
	return sess
}

func TestManager_PaneTarget_PaneID(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-a", "%42", "sess-x")

	got, err := mgr.PaneTarget(sess.ID)
	if err != nil {
		t.Fatalf("PaneTarget failed: %v", err)
	}
	if got != "%42" {
		t.Errorf("PaneTarget = %q, want %q", got, "%42")
	}
}

func TestManager_PaneTarget_WindowFallback(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-b", "", "sess-y")

	got, err := mgr.PaneTarget(sess.ID)
	if err != nil {
		t.Fatalf("PaneTarget failed: %v", err)
	}
	want := "jin:sess-y.0"
	if got != want {
		t.Errorf("PaneTarget = %q, want %q", got, want)
	}
}

func TestManager_PaneTarget_NoPane(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-c", "", "")

	if _, err := mgr.PaneTarget(sess.ID); err == nil {
		t.Fatal("expected error when session has no pane, got nil")
	}
}

func TestManager_PaneTarget_UnknownID(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	if _, err := mgr.PaneTarget("does-not-exist"); err == nil {
		t.Fatal("expected error for unknown id, got nil")
	}
}

func TestManager_PanePopup_TargetAndDir(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-popup", "%7", "sess-z")

	if err := mgr.PanePopup(sess.ID, "echo hi", "Title", "80%", "50%"); err != nil {
		t.Fatalf("PanePopup failed: %v", err)
	}

	var found bool
	for _, c := range mock.calls {
		if c.method != "DisplayPopup" {
			continue
		}
		found = true
		if c.args[0] != "%7" {
			t.Errorf("DisplayPopup target = %q, want %q", c.args[0], "%7")
		}
		if c.args[2] != "/tmp/pane-popup" {
			t.Errorf("DisplayPopup dir = %q, want %q", c.args[2], "/tmp/pane-popup")
		}
	}
	if !found {
		t.Error("expected DisplayPopup to be called")
	}
}

func TestManager_PaneCapture_ReturnsContent(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-cap", "%9", "sess-cap")
	mock.captured["%9"] = "line1\nline2"

	got, err := mgr.PaneCapture(sess.ID, false)
	if err != nil {
		t.Fatalf("PaneCapture failed: %v", err)
	}
	if got != "line1\nline2" {
		t.Errorf("PaneCapture = %q, want %q", got, "line1\nline2")
	}
}

func TestManager_PaneSendKeys_LiteralVsNamed(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-keys", "%11", "sess-keys")

	if err := mgr.PaneSendKeys(sess.ID, "hello", true); err != nil {
		t.Fatalf("PaneSendKeys literal failed: %v", err)
	}
	if !mock.hasCalledWith("SendKeysLiteral", "%11") {
		t.Error("expected SendKeysLiteral for literal=true")
	}

	if err := mgr.PaneSendKeys(sess.ID, "Enter", false); err != nil {
		t.Fatalf("PaneSendKeys named failed: %v", err)
	}
	if !mock.hasCalledWith("SendKeys", "%11") {
		t.Error("expected SendKeys for literal=false")
	}
}

func TestManager_PaneSplit_DelegatesToTmux(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-split", "%13", "sess-split")

	paneID, err := mgr.PaneSplit(sess.ID, "", "", tmux.SplitOptions{Cmd: "top", Direction: "right", Size: "30%"})
	if err != nil {
		t.Fatalf("PaneSplit failed: %v", err)
	}
	if paneID != "%99" {
		t.Errorf("PaneSplit pane ID = %q, want %q", paneID, "%99")
	}

	var found bool
	for _, c := range mock.calls {
		if c.method != "SplitPane" {
			continue
		}
		found = true
		if c.args[0] != "%13" {
			t.Errorf("SplitPane target = %q, want %q", c.args[0], "%13")
		}
		if c.args[2] != "right" || c.args[3] != "30%" {
			t.Errorf("SplitPane direction/size = %q/%q, want right/30%%", c.args[2], c.args[3])
		}
		if c.args[4] != "/tmp/pane-split" {
			t.Errorf("SplitPane dir = %q, want session workdir %q", c.args[4], "/tmp/pane-split")
		}
	}
	if !found {
		t.Error("expected SplitPane to be called")
	}
}

func TestManager_PaneSplit_NamedFirstTime(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-named", "%13", "sess-named")

	paneID, err := mgr.PaneSplit(sess.ID, "demo", "", tmux.SplitOptions{Cmd: "top"})
	if err != nil {
		t.Fatalf("PaneSplit failed: %v", err)
	}
	if paneID != "%99" {
		t.Errorf("pane ID = %q, want %q", paneID, "%99")
	}
	if !mock.hasCalledWith("SplitPane", "%13") {
		t.Error("expected SplitPane for a not-yet-existing named pane")
	}
	var named bool
	for _, c := range mock.calls {
		if c.method == "SetPaneOption" && c.args[0] == "%99" && c.args[2] == "demo" {
			named = true
		}
	}
	if !named {
		t.Error("expected SetPaneOption to tag the new pane with the slot name")
	}
}

func TestManager_PaneSplit_NamedExisting_Noop(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-noop", "%13", "sess-noop")
	mock.namedPanes["demo"] = "%50"

	paneID, err := mgr.PaneSplit(sess.ID, "demo", "", tmux.SplitOptions{Cmd: "top"})
	if err != nil {
		t.Fatalf("PaneSplit failed: %v", err)
	}
	if paneID != "%50" {
		t.Errorf("pane ID = %q, want existing pane %q", paneID, "%50")
	}
	if mock.hasCalledWith("SplitPane", "%13") {
		t.Error("noop must not split when the named pane already exists")
	}
}

func TestManager_PaneSplit_NamedExisting_Respawn(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-respawn", "%13", "sess-respawn")
	mock.namedPanes["demo"] = "%50"

	paneID, err := mgr.PaneSplit(sess.ID, "demo", "respawn", tmux.SplitOptions{Cmd: "htop"})
	if err != nil {
		t.Fatalf("PaneSplit failed: %v", err)
	}
	if paneID != "%50" {
		t.Errorf("pane ID = %q, want existing pane %q", paneID, "%50")
	}
	if !mock.hasCalledWith("RespawnPane", "%50") {
		t.Error("expected RespawnPane on the existing named pane")
	}
	if mock.hasCalledWith("SplitPane", "%13") {
		t.Error("respawn must not split when the named pane already exists")
	}
}

func TestManager_PaneSplit_NamedExisting_Error(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-err", "%13", "sess-err")
	mock.namedPanes["demo"] = "%50"

	if _, err := mgr.PaneSplit(sess.ID, "demo", "error", tmux.SplitOptions{}); err == nil {
		t.Fatal("expected error for if-exists=error on an existing named pane")
	}
}

func TestManager_PaneClose_KillsNamedPane(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-close", "%13", "sess-close")
	mock.namedPanes["demo"] = "%50"

	if err := mgr.PaneClose(sess.ID, "demo"); err != nil {
		t.Fatalf("PaneClose failed: %v", err)
	}
	if !mock.hasCalledWith("KillPane", "%50") {
		t.Error("expected KillPane on the named pane")
	}
}

func TestManager_PaneClose_NotFound(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-close-nf", "%13", "sess-close-nf")

	err := mgr.PaneClose(sess.ID, "nonexistent")
	if err == nil {
		t.Fatal("expected error for an unknown pane name")
	}
}

func TestManager_PaneClose_RefusesAgentPane(t *testing.T) {
	mgr, mock, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-close-agent", "%13", "sess-close-agent")
	mock.namedPanes["demo"] = "%13" // same pane as the session's agent pane

	if err := mgr.PaneClose(sess.ID, "demo"); err == nil {
		t.Fatal("expected refusal to kill the session's agent pane")
	}
	if mock.hasCalledWith("KillPane", "%13") {
		t.Error("KillPane must not be called on the agent pane")
	}
}

func TestManager_Pane_TmuxUnavailable(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	sess := paneTestSession(t, mgr, "/tmp/pane-nil", "%15", "sess-nil")
	mgr.SetTmuxClient(nil)

	if err := mgr.PanePopup(sess.ID, "cmd", "", "", ""); err == nil {
		t.Error("PanePopup: expected error when tmux is nil")
	}
	if _, err := mgr.PaneSplit(sess.ID, "", "", tmux.SplitOptions{Cmd: "cmd"}); err == nil {
		t.Error("PaneSplit: expected error when tmux is nil")
	}
	if err := mgr.PaneClose(sess.ID, "demo"); err == nil {
		t.Error("PaneClose: expected error when tmux is nil")
	}
	if _, err := mgr.PaneCapture(sess.ID, false); err == nil {
		t.Error("PaneCapture: expected error when tmux is nil")
	}
	if err := mgr.PaneSendKeys(sess.ID, "x", true); err == nil {
		t.Error("PaneSendKeys: expected error when tmux is nil")
	}
}
