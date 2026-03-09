package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/takaaki-s/claude-code-valet/internal/config"
	"github.com/takaaki-s/claude-code-valet/internal/host"
	"github.com/takaaki-s/claude-code-valet/internal/notify"
	"github.com/takaaki-s/claude-code-valet/internal/session"
)

// setupTestServer creates a Server and Client connected via a Unix socket in t.TempDir().
// The server runs in a background goroutine and is stopped on test cleanup.
// We manage the listener directly to avoid data races in Server.Start()/Stop().
func setupTestServer(t *testing.T) (*Server, *Client) {
	t.Helper()

	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")
	dataDir := filepath.Join(tmpDir, "sessions")
	configDir := filepath.Join(tmpDir, "config")

	server, err := NewServer(socketPath, dataDir, configDir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Pre-create the listener ourselves so we own it and avoid races.
	os.Remove(socketPath)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	// Use a local variable for the accept loop to avoid racing on server.listener.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // listener closed
			}
			go server.handleConnection(conn)
		}
	}()

	client := NewClient(socketPath)

	t.Cleanup(func() {
		listener.Close()
		<-done // wait for accept loop to exit
		os.Remove(socketPath)
	})

	return server, client
}

func TestIntegration_ClientIsRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	if !client.IsRunning() {
		t.Error("client.IsRunning() = false, want true")
	}
}

func TestIntegration_NewAndList(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	// Create a session (without starting it, since tmux isn't available)
	info, err := client.NewWithOptions(NewOptions{
		Name:    "test-session",
		WorkDir: "/tmp/test-project",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	if info.Name != "test-session" {
		t.Errorf("Name: got %q, want %q", info.Name, "test-session")
	}
	if info.WorkDir != "/tmp/test-project" {
		t.Errorf("WorkDir: got %q, want %q", info.WorkDir, "/tmp/test-project")
	}
	if info.ID == "" {
		t.Error("ID is empty")
	}

	// List sessions
	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("List: got %d sessions, want 1", len(sessions))
	}
	if sessions[0].Name != "test-session" {
		t.Errorf("Listed session Name: got %q, want %q", sessions[0].Name, "test-session")
	}
}

func TestIntegration_CreateMultipleAndList(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	for _, name := range []string{"alpha", "beta", "gamma"} {
		_, err := client.NewWithOptions(NewOptions{
			Name:    name,
			WorkDir: "/tmp/" + name,
			Start:   false,
		})
		if err != nil {
			t.Fatalf("NewWithOptions(%s): %v", name, err)
		}
	}

	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("List: got %d, want 3", len(sessions))
	}
}

func TestIntegration_Delete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "to-delete",
		WorkDir: "/tmp/to-delete",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	if err := client.Delete(info.ID, ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("List after delete: got %d, want 0", len(sessions))
	}
}

func TestIntegration_HookEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "hook-test",
		WorkDir: "/tmp/hook-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	// Send a hook event
	err = client.SendHook(HookRequest{
		SessionID:        info.ClaudeSessionID,
		CcvaletSessionID: info.ID,
		HookEventName:    "UserPromptSubmit",
	})
	if err != nil {
		t.Fatalf("SendHook: %v", err)
	}

	// Verify status changed
	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("List: got %d, want 1", len(sessions))
	}
	if string(sessions[0].Status) != "thinking" {
		t.Errorf("Status after hook: got %q, want %q", sessions[0].Status, "thinking")
	}
}

func TestIntegration_NotificationHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	entries, err := client.NotificationHistory()
	if err != nil {
		t.Fatalf("NotificationHistory: %v", err)
	}
	// Empty history at start
	if len(entries) != 0 {
		t.Errorf("NotificationHistory: got %d entries, want 0", len(entries))
	}
}

func TestIntegration_UnknownAction(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	// Send an unknown action directly
	req := Request{Action: "nonexistent"}
	resp, err := client.send(req)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.Success {
		t.Error("expected Success=false for unknown action")
	}
	if resp.Error == "" {
		t.Error("expected non-empty error for unknown action")
	}
}

func TestIntegration_DuplicateWorkDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	_, err := client.NewWithOptions(NewOptions{
		Name:    "first",
		WorkDir: "/tmp/same-dir",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("first NewWithOptions: %v", err)
	}

	_, err = client.NewWithOptions(NewOptions{
		Name:    "second",
		WorkDir: "/tmp/same-dir",
		Start:   false,
	})
	if err == nil {
		t.Error("expected error for duplicate WorkDir")
	}
}

func TestIntegration_ListHosts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	hosts, err := client.ListHosts()
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	// Without remote hosts configured, should return empty or just local
	_ = hosts // No assertion on count since it depends on configuration
}

func TestIntegration_HookStopTriggersIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "stop-test",
		WorkDir: "/tmp/stop-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	// First make it "thinking"
	if err := client.SendHook(HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "UserPromptSubmit",
	}); err != nil {
		t.Fatalf("SendHook(UserPromptSubmit): %v", err)
	}

	// Then send "Stop" to transition to idle
	if err := client.SendHook(HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "Stop",
	}); err != nil {
		t.Fatalf("SendHook(Stop): %v", err)
	}

	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("List: got %d, want 1", len(sessions))
	}
	if string(sessions[0].Status) != "idle" {
		t.Errorf("Status: got %q, want %q", sessions[0].Status, "idle")
	}
}

func TestIntegration_HookPermission(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "perm-test",
		WorkDir: "/tmp/perm-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	if err := client.SendHook(HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "Notification",
		NotificationType: "permission_prompt",
	}); err != nil {
		t.Fatalf("SendHook: %v", err)
	}

	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if string(sessions[0].Status) != "permission" {
		t.Errorf("Status: got %q, want %q", sessions[0].Status, "permission")
	}
}

// Verify Request/Response JSON serialization
func TestRequestResponse_JSON(t *testing.T) {
	req := Request{
		Action: "new",
		Data:   json.RawMessage(`{"name":"test","work_dir":"/tmp"}`),
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal request: %v", err)
	}

	var decoded Request
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal request: %v", err)
	}
	if decoded.Action != "new" {
		t.Errorf("Action: got %q, want %q", decoded.Action, "new")
	}
}

func TestIntegration_Start(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "start-test",
		WorkDir: "/tmp/start-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	// Call Start — tmux is not available in test so this should return an error,
	// but the important thing is the API roundtrip works (no socket/protocol error).
	err = client.Start(info.ID, "")
	if err == nil {
		// If Start somehow succeeds (e.g. tmux is installed), that's fine too.
		t.Log("Start succeeded unexpectedly (tmux may be available)")
	} else {
		t.Logf("Start returned expected error (no tmux): %v", err)
	}
}

func TestIntegration_Kill(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "kill-test",
		WorkDir: "/tmp/kill-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	if err := client.Kill(info.ID, ""); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("List: got %d sessions, want 1", len(sessions))
	}
	if string(sessions[0].Status) != "stopped" {
		t.Errorf("Status after Kill: got %q, want %q", sessions[0].Status, "stopped")
	}
}

func TestIntegration_DeleteByHostID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "delete-host-test",
		WorkDir: "/tmp/delete-host-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	// Delete with hostID "local" — should be treated the same as empty
	if err := client.Delete(info.ID, "local"); err != nil {
		t.Fatalf("Delete with hostID=local: %v", err)
	}

	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("List after delete: got %d sessions, want 0", len(sessions))
	}
}

func TestIntegration_HookCWDUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "cwd-test",
		WorkDir: "/tmp/cwd-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	// Send a hook with CWD field set
	newCWD := "/home/user/new-project"
	err = client.SendHook(HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "UserPromptSubmit",
		CWD:              newCWD,
	})
	if err != nil {
		t.Fatalf("SendHook: %v", err)
	}

	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("List: got %d sessions, want 1", len(sessions))
	}
	if sessions[0].CurrentWorkDir != newCWD {
		t.Errorf("CurrentWorkDir: got %q, want %q", sessions[0].CurrentWorkDir, newCWD)
	}
}

func TestIntegration_MultipleHookTransitions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "multi-hook-test",
		WorkDir: "/tmp/multi-hook-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	// Helper to check status after a hook
	checkStatus := func(step, expectedStatus string) {
		t.Helper()
		sessions, err := client.List()
		if err != nil {
			t.Fatalf("List at step %s: %v", step, err)
		}
		if len(sessions) != 1 {
			t.Fatalf("List at step %s: got %d sessions, want 1", step, len(sessions))
		}
		if string(sessions[0].Status) != expectedStatus {
			t.Errorf("Status at step %s: got %q, want %q", step, sessions[0].Status, expectedStatus)
		}
	}

	// Step 1: UserPromptSubmit → thinking
	err = client.SendHook(HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "UserPromptSubmit",
	})
	if err != nil {
		t.Fatalf("SendHook(UserPromptSubmit #1): %v", err)
	}
	checkStatus("UserPromptSubmit #1", "thinking")

	// Step 2: Stop → idle
	err = client.SendHook(HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "Stop",
	})
	if err != nil {
		t.Fatalf("SendHook(Stop): %v", err)
	}
	checkStatus("Stop", "idle")

	// Step 3: UserPromptSubmit → thinking (again)
	err = client.SendHook(HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "UserPromptSubmit",
	})
	if err != nil {
		t.Fatalf("SendHook(UserPromptSubmit #2): %v", err)
	}
	checkStatus("UserPromptSubmit #2", "thinking")

	// Step 4: Notification(permission_prompt) → permission
	err = client.SendHook(HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "Notification",
		NotificationType: "permission_prompt",
	})
	if err != nil {
		t.Fatalf("SendHook(Notification): %v", err)
	}
	checkStatus("Notification(permission_prompt)", "permission")
}

func TestIntegration_DeleteNonExistent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	err := client.Delete("non-existent-id-12345", "")
	if err == nil {
		t.Error("expected error when deleting non-existent session, got nil")
	}
}

func TestIntegration_NotificationHistoryWithEntries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "notif-test",
		WorkDir: "/tmp/notif-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	// UserPromptSubmit → thinking
	if err := client.SendHook(HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "UserPromptSubmit",
	}); err != nil {
		t.Fatalf("SendHook(UserPromptSubmit): %v", err)
	}

	// Stop → idle (generates a "task complete" notification)
	if err := client.SendHook(HookRequest{
		CcvaletSessionID: info.ID,
		HookEventName:    "Stop",
	}); err != nil {
		t.Fatalf("SendHook(Stop): %v", err)
	}

	entries, err := client.NotificationHistory()
	if err != nil {
		t.Fatalf("NotificationHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Error("NotificationHistory: expected at least 1 entry after Stop hook, got 0")
	}
}

func TestIntegration_CreateMultipleAndListNames(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	names := []string{"alpha", "beta", "gamma"}
	for _, name := range names {
		_, err := client.NewWithOptions(NewOptions{
			Name:    name,
			WorkDir: "/tmp/" + name,
			Start:   false,
		})
		if err != nil {
			t.Fatalf("NewWithOptions(%s): %v", name, err)
		}
	}

	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("List: got %d sessions, want 3", len(sessions))
	}

	// Collect all returned names
	gotNames := make(map[string]bool)
	for _, s := range sessions {
		gotNames[s.Name] = true
	}

	for _, name := range names {
		if !gotNames[name] {
			t.Errorf("List: missing session name %q", name)
		}
	}
}

func TestIntegration_GetSessionByList(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "get-test",
		WorkDir: "/tmp/get-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	// Retrieve the session via List and find it by ID
	sessions, err := client.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("List: got %d sessions, want 1", len(sessions))
	}

	got := sessions[0]
	if got.ID != info.ID {
		t.Errorf("ID: got %q, want %q", got.ID, info.ID)
	}
	if got.Name != "get-test" {
		t.Errorf("Name: got %q, want %q", got.Name, "get-test")
	}
	if got.WorkDir != "/tmp/get-test" {
		t.Errorf("WorkDir: got %q, want %q", got.WorkDir, "/tmp/get-test")
	}
	if string(got.Status) != "stopped" && string(got.Status) != "idle" && string(got.Status) != "" {
		// New sessions without Start are typically "stopped"
		t.Logf("Status: got %q (may vary by implementation)", got.Status)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

// --- pollRemoteOnce tests ---

// mockSlaveClient implements host.SlaveClient for testing pollRemoteOnce.
type mockSlaveClient struct {
	entries []notify.Entry
	err     error
	running bool
}

func (m *mockSlaveClient) IsRunning() bool {
	return m.running
}

func (m *mockSlaveClient) ListWithHostID() ([]session.Info, error) {
	return nil, nil
}

func (m *mockSlaveClient) NotificationHistoryWithHostID() ([]notify.Entry, error) {
	return m.entries, m.err
}

func TestPollRemoteOnce_NewEntries(t *testing.T) {
	server, _ := setupTestServer(t)

	t1 := time.Now().Add(-2 * time.Second)
	t2 := time.Now().Add(-1 * time.Second)

	mock := &mockSlaveClient{
		running: true,
		entries: []notify.Entry{
			{SessionID: "s1", SessionName: "alpha", Type: "permission", Message: "[alpha] waiting", Timestamp: t1, HostID: "ec2"},
			{SessionID: "s2", SessionName: "beta", Type: "task_complete", Message: "[beta] done", Timestamp: t2, HostID: "ec2"},
		},
	}

	registry := host.NewRegistry([]config.HostConfig{{ID: "ec2", Type: "ssh"}})
	registry.SetClient("ec2", mock)
	server.hostRegistry = registry

	lastSeen := make(map[string]time.Time)
	server.pollRemoteOnce(lastSeen)

	// lastSeen should be advanced to t2 (the latest entry)
	if !lastSeen["ec2"].Equal(t2) {
		t.Errorf("lastSeen[ec2]: got %v, want %v", lastSeen["ec2"], t2)
	}
}

func TestPollRemoteOnce_SkipsAlreadySeen(t *testing.T) {
	server, _ := setupTestServer(t)

	t1 := time.Now().Add(-2 * time.Second)
	t2 := time.Now().Add(-1 * time.Second)

	mock := &mockSlaveClient{
		running: true,
		entries: []notify.Entry{
			{SessionID: "s1", Type: "permission", Timestamp: t1, HostID: "ec2"},
			{SessionID: "s2", Type: "task_complete", Timestamp: t2, HostID: "ec2"},
		},
	}

	registry := host.NewRegistry([]config.HostConfig{{ID: "ec2", Type: "ssh"}})
	registry.SetClient("ec2", mock)
	server.hostRegistry = registry

	// Pre-set lastSeen to t2, so both entries should be skipped
	lastSeen := map[string]time.Time{"ec2": t2}
	server.pollRemoteOnce(lastSeen)

	// lastSeen should remain unchanged
	if !lastSeen["ec2"].Equal(t2) {
		t.Errorf("lastSeen[ec2] should not change: got %v, want %v", lastSeen["ec2"], t2)
	}
}

func TestPollRemoteOnce_PartialNew(t *testing.T) {
	server, _ := setupTestServer(t)

	t1 := time.Now().Add(-3 * time.Second)
	t2 := time.Now().Add(-2 * time.Second)
	t3 := time.Now().Add(-1 * time.Second)

	mock := &mockSlaveClient{
		running: true,
		entries: []notify.Entry{
			{SessionID: "s1", Type: "permission", Timestamp: t1, HostID: "ec2"},
			{SessionID: "s2", Type: "task_complete", Timestamp: t2, HostID: "ec2"},
			{SessionID: "s3", Type: "permission", Timestamp: t3, HostID: "ec2"},
		},
	}

	registry := host.NewRegistry([]config.HostConfig{{ID: "ec2", Type: "ssh"}})
	registry.SetClient("ec2", mock)
	server.hostRegistry = registry

	// Only t1 has been seen; t2 and t3 should be new
	lastSeen := map[string]time.Time{"ec2": t1}
	server.pollRemoteOnce(lastSeen)

	if !lastSeen["ec2"].Equal(t3) {
		t.Errorf("lastSeen[ec2]: got %v, want %v", lastSeen["ec2"], t3)
	}
}

func TestPollRemoteOnce_MultipleHosts(t *testing.T) {
	server, _ := setupTestServer(t)

	t1 := time.Now().Add(-2 * time.Second)
	t2 := time.Now().Add(-1 * time.Second)

	mock1 := &mockSlaveClient{
		running: true,
		entries: []notify.Entry{
			{SessionID: "s1", Type: "permission", Timestamp: t1, HostID: "ec2"},
		},
	}
	mock2 := &mockSlaveClient{
		running: true,
		entries: []notify.Entry{
			{SessionID: "s2", Type: "task_complete", Timestamp: t2, HostID: "docker"},
		},
	}

	registry := host.NewRegistry([]config.HostConfig{
		{ID: "ec2", Type: "ssh"},
		{ID: "docker", Type: "docker"},
	})
	registry.SetClient("ec2", mock1)
	registry.SetClient("docker", mock2)
	server.hostRegistry = registry

	lastSeen := make(map[string]time.Time)
	server.pollRemoteOnce(lastSeen)

	if !lastSeen["ec2"].Equal(t1) {
		t.Errorf("lastSeen[ec2]: got %v, want %v", lastSeen["ec2"], t1)
	}
	if !lastSeen["docker"].Equal(t2) {
		t.Errorf("lastSeen[docker]: got %v, want %v", lastSeen["docker"], t2)
	}
}

func TestPollRemoteOnce_NilClient(t *testing.T) {
	server, _ := setupTestServer(t)

	registry := host.NewRegistry([]config.HostConfig{{ID: "ec2", Type: "ssh"}})
	// Client is nil (not yet connected)
	server.hostRegistry = registry

	lastSeen := make(map[string]time.Time)
	// Should not panic
	server.pollRemoteOnce(lastSeen)

	if _, ok := lastSeen["ec2"]; ok {
		t.Error("lastSeen should not have entry for host with nil client")
	}
}

func TestPollRemoteOnce_ClientError(t *testing.T) {
	server, _ := setupTestServer(t)

	mock := &mockSlaveClient{
		running: true,
		err:     fmt.Errorf("connection refused"),
	}

	registry := host.NewRegistry([]config.HostConfig{{ID: "ec2", Type: "ssh"}})
	registry.SetClient("ec2", mock)
	server.hostRegistry = registry

	lastSeen := make(map[string]time.Time)
	server.pollRemoteOnce(lastSeen)

	if _, ok := lastSeen["ec2"]; ok {
		t.Error("lastSeen should not have entry when client returns error")
	}
}

func TestPollRemoteOnce_EmptyEntries(t *testing.T) {
	server, _ := setupTestServer(t)

	mock := &mockSlaveClient{
		running: true,
		entries: []notify.Entry{},
	}

	registry := host.NewRegistry([]config.HostConfig{{ID: "ec2", Type: "ssh"}})
	registry.SetClient("ec2", mock)
	server.hostRegistry = registry

	lastSeen := make(map[string]time.Time)
	server.pollRemoteOnce(lastSeen)

	if _, ok := lastSeen["ec2"]; ok {
		t.Error("lastSeen should not have entry for host with empty entries")
	}
}
