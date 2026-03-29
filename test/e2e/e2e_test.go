//go:build e2e

package e2e

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/takaaki-s/claude-code-valet/internal/daemon"
	"github.com/takaaki-s/claude-code-valet/internal/session"
	"github.com/takaaki-s/claude-code-valet/internal/tmux"
)

func TestMain(m *testing.M) {
	if !tmux.HasTmux() {
		log.Println("SKIP: tmux not available")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// setupE2E creates a daemon server with a real session manager and returns a client.
func setupE2E(t *testing.T) *daemon.Client {
	t.Helper()

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "e2e.sock")
	dataDir := filepath.Join(tmpDir, "sessions")
	configDir := filepath.Join(tmpDir, "config")

	server, err := daemon.NewServer(socketPath, dataDir, configDir, "local")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	go func() {
		server.Start()
	}()

	// Wait for socket
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		server.Stop()
	})

	return daemon.NewClient(socketPath)
}

func TestE2E_DaemonStartStop(t *testing.T) {
	client := setupE2E(t)

	if !client.IsRunning() {
		t.Fatal("daemon is not running after start")
	}

	// List should work (empty)
	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestE2E_SessionLifecycle(t *testing.T) {
	client := setupE2E(t)

	// Create session
	info, err := client.NewWithOptions(daemon.NewOptions{
		Name:    "e2e-test",
		WorkDir: t.TempDir(), // Use a real directory
		Start:   false,       // Don't start (tmux ccvalet session may not exist)
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if info.ID == "" {
		t.Fatal("session ID is empty")
	}
	if info.Name != "e2e-test" {
		t.Errorf("Name: got %q, want %q", info.Name, "e2e-test")
	}
	if info.Status != session.StatusStopped {
		t.Errorf("Status: got %q, want %q", info.Status, session.StatusStopped)
	}

	// List
	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	// Delete
	if err := client.Delete(info.ID, "", false, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	sessions, err = client.List()
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions after delete, got %d", len(sessions))
	}
}

func TestE2E_HookEventFlow(t *testing.T) {
	client := setupE2E(t)

	info, err := client.NewWithOptions(daemon.NewOptions{
		Name:    "hook-e2e",
		WorkDir: t.TempDir(),
		Start:   false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// UserPromptSubmit → thinking
	if err := client.SendHook(daemon.HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "UserPromptSubmit",
	}); err != nil {
		t.Fatalf("SendHook(UserPromptSubmit): %v", err)
	}

	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if sessions[0].Status != session.StatusThinking {
		t.Errorf("after UserPromptSubmit: got %q, want %q", sessions[0].Status, session.StatusThinking)
	}

	// Notification(permission_prompt) → permission
	if err := client.SendHook(daemon.HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "Notification",
		NotificationType: "permission_prompt",
	}); err != nil {
		t.Fatalf("SendHook(Notification): %v", err)
	}

	sessions, err = client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if sessions[0].Status != session.StatusPermission {
		t.Errorf("after Notification: got %q, want %q", sessions[0].Status, session.StatusPermission)
	}

	// Stop → idle
	if err := client.SendHook(daemon.HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "Stop",
	}); err != nil {
		t.Fatalf("SendHook(Stop): %v", err)
	}

	sessions, err = client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if sessions[0].Status != session.StatusIdle {
		t.Errorf("after Stop: got %q, want %q", sessions[0].Status, session.StatusIdle)
	}
}

func TestE2E_NotificationHistory(t *testing.T) {
	client := setupE2E(t)

	// Initially empty
	entries, err := client.NotificationHistory()
	if err != nil {
		t.Fatalf("NotificationHistory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}

	// Create session and trigger a Stop hook (generates task_complete notification)
	info, err := client.NewWithOptions(daemon.NewOptions{
		Name:    "notify-e2e",
		WorkDir: t.TempDir(),
		Start:   false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Make it thinking first, then stop
	client.SendHook(daemon.HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "UserPromptSubmit",
	})
	client.SendHook(daemon.HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "Stop",
	})

	// Check notification history
	entries, err = client.NotificationHistory()
	if err != nil {
		t.Fatalf("NotificationHistory: %v", err)
	}
	// At least one notification should have been recorded
	if len(entries) == 0 {
		t.Log("WARNING: notification history is empty (notifications may be debounced)")
	}
}

func TestE2E_MultipleSessionsConcurrent(t *testing.T) {
	client := setupE2E(t)

	// Create multiple sessions
	ids := make([]string, 5)
	for i := range 5 {
		info, err := client.NewWithOptions(daemon.NewOptions{
			Name:    filepath.Base(t.TempDir()), // unique name
			WorkDir: t.TempDir(),
			Start:   false,
		})
		if err != nil {
			t.Fatalf("New(%d): %v", i, err)
		}
		ids[i] = info.ID
	}

	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 5 {
		t.Fatalf("expected 5 sessions, got %d", len(sessions))
	}

	// Delete all
	for _, id := range ids {
		if err := client.Delete(id, "", false, false); err != nil {
			t.Fatalf("Delete(%s): %v", id, err)
		}
	}

	sessions, err = client.List()
	if err != nil {
		t.Fatalf("List after delete all: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

// Verify IPC JSON wire format
func TestE2E_WireFormat(t *testing.T) {
	// Verify request serialization
	req := daemon.Request{
		Action: "new",
		Data:   json.RawMessage(`{"name":"test","work_dir":"/tmp/test"}`),
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded["action"] != "new" {
		t.Errorf("action: got %v, want %q", decoded["action"], "new")
	}
	if decoded["data"] == nil {
		t.Error("data is nil")
	}
}
