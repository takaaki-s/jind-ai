package session

import (
	"sync"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/plugin"
)

// mockDispatcher records published events synchronously so tests can assert
// on them without polling.
type mockDispatcher struct {
	mu     sync.Mutex
	events []plugin.Event
}

func (m *mockDispatcher) Publish(ev plugin.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
}

func (m *mockDispatcher) all() []plugin.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]plugin.Event(nil), m.events...)
}

func TestManager_HandleHookEvent_PublishesStatusChanged(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	disp := &mockDispatcher{}
	mgr.SetPluginDispatcher(disp)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/plug-ev", Description: "pev"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")

	events := disp.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Name != plugin.EventStatusChanged {
		t.Errorf("Name = %q, want %q", ev.Name, plugin.EventStatusChanged)
	}
	if ev.SessionID != sess.ID {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, sess.ID)
	}
	if ev.Status != string(StatusThinking) {
		t.Errorf("Status = %q, want %q", ev.Status, StatusThinking)
	}
	if ev.PrevStatus != string(StatusStopped) {
		t.Errorf("PrevStatus = %q, want %q", ev.PrevStatus, StatusStopped)
	}
	if ev.AgentKind != "claude" {
		t.Errorf("AgentKind = %q, want claude", ev.AgentKind)
	}
	if ev.WorkDir != sess.WorkDir {
		t.Errorf("WorkDir = %q, want %q", ev.WorkDir, sess.WorkDir)
	}
	if ev.NotifyKind != "" {
		t.Errorf("NotifyKind = %q, want empty (UserPromptSubmit carries no notification)", ev.NotifyKind)
	}

	// Stop transitions Thinking -> Idle with Notify: NotifyTaskComplete
	// (fakeStatusSource in manager_test.go), so the published event should
	// carry NotifyKind through from upd.Notify.
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "Stop", "", "", "")
	events = disp.all()
	if len(events) != 2 {
		t.Fatalf("published %d events after Stop, want 2", len(events))
	}
	if got := events[1].NotifyKind; got != "task-complete" {
		t.Errorf("NotifyKind = %q, want %q", got, "task-complete")
	}
}

func TestManager_HandleHookEvent_NoPublishWithoutTransition(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	disp := &mockDispatcher{}
	mgr.SetPluginDispatcher(disp)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/plug-same", Description: "psame"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mgr.SetStatus(sess.ID, StatusThinking)

	// thinking -> thinking: a verdict arrives but the status does not move.
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")

	if events := disp.all(); len(events) != 0 {
		t.Fatalf("published %d events for a no-op transition, want 0", len(events))
	}
}

func TestManager_HandleHookEvent_NilDispatcher(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sess, _, err := mgr.CreateWithOptions(CreateOptions{WorkDir: "/tmp/plug-nil", Description: "pnil"})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Must not panic without a dispatcher; status handling is unchanged.
	mgr.HandleHookEvent(sess.AgentSessionID, sess.ID, "UserPromptSubmit", "", "", "")

	got, ok := mgr.Get(sess.ID)
	if !ok || got.Status != StatusThinking {
		t.Fatalf("Status = %v (ok=%v), want %v", got.Status, ok, StatusThinking)
	}
}
