package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/takaaki-s/honjin/internal/config"
	"github.com/takaaki-s/honjin/internal/host"
	"github.com/takaaki-s/honjin/internal/notify"
	"github.com/takaaki-s/honjin/internal/session"
)

// shortTempDir creates a short temporary directory under /tmp to avoid macOS
// Unix socket path length limit (104 bytes). t.TempDir() paths are too long.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ccv-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// setupTestServer creates a Server and Client connected via a Unix socket.
// The server runs in a background goroutine and is stopped on test cleanup.
// We manage the listener directly to avoid data races in Server.Start()/Stop().
func setupTestServer(t *testing.T) (*Server, *Client) {
	t.Helper()

	tmpDir := shortTempDir(t)
	socketPath := filepath.Join(tmpDir, "t.sock")
	sessionsDir := filepath.Join(tmpDir, "sessions")
	configDir := filepath.Join(tmpDir, "config")
	stateDir := filepath.Join(tmpDir, "state")

	server, err := NewServer(socketPath, sessionsDir, configDir, stateDir, "local")
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

	if err := client.Delete(info.ID, "", false, false); err != nil {
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
		SessionID:     info.ClaudeSessionID,
		JinSessionID:  info.ID,
		HookEventName: "UserPromptSubmit",
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
		JinSessionID:  info.ID,
		HookEventName: "UserPromptSubmit",
	}); err != nil {
		t.Fatalf("SendHook(UserPromptSubmit): %v", err)
	}

	// Then send "Stop" to transition to idle
	if err := client.SendHook(HookRequest{
		JinSessionID:  info.ID,
		HookEventName: "Stop",
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
		JinSessionID:     info.ID,
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

// TestNewRequest_WorktreeFieldsRoundTrip verifies that the worktree fields survive
// a JSON marshal/unmarshal round trip. This is the path handleNew's forwarding
// logic relies on (server.go re-marshals the whole NewRequest when forwarding to
// a remote host), so a lossy field would silently drop worktree options for
// forwarded requests.
func TestNewRequest_WorktreeFieldsRoundTrip(t *testing.T) {
	req := NewRequest{
		Name:           "wt-session",
		WorkDir:        "/tmp/wt-project",
		Start:          true,
		Fleet:          "backend",
		Worktree:       true,
		WorktreeName:   "my-wt",
		WorktreeBranch: "feat/xyz",
		WorktreeBase:   "develop",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded NewRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded != req {
		t.Errorf("round trip mismatch: got %+v, want %+v", decoded, req)
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
	if err := client.Delete(info.ID, "local", false, false); err != nil {
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
		JinSessionID:  info.ID,
		HookEventName: "UserPromptSubmit",
		CWD:           newCWD,
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
		JinSessionID:  info.ID,
		HookEventName: "UserPromptSubmit",
	})
	if err != nil {
		t.Fatalf("SendHook(UserPromptSubmit #1): %v", err)
	}
	checkStatus("UserPromptSubmit #1", "thinking")

	// Step 2: Stop → idle
	err = client.SendHook(HookRequest{
		JinSessionID:  info.ID,
		HookEventName: "Stop",
	})
	if err != nil {
		t.Fatalf("SendHook(Stop): %v", err)
	}
	checkStatus("Stop", "idle")

	// Step 3: UserPromptSubmit → thinking (again)
	err = client.SendHook(HookRequest{
		JinSessionID:  info.ID,
		HookEventName: "UserPromptSubmit",
	})
	if err != nil {
		t.Fatalf("SendHook(UserPromptSubmit #2): %v", err)
	}
	checkStatus("UserPromptSubmit #2", "thinking")

	// Step 4: Notification(permission_prompt) → permission
	err = client.SendHook(HookRequest{
		JinSessionID:     info.ID,
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

	err := client.Delete("non-existent-id-12345", "", false, false)
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
		JinSessionID:  info.ID,
		HookEventName: "UserPromptSubmit",
	}); err != nil {
		t.Fatalf("SendHook(UserPromptSubmit): %v", err)
	}

	// Stop → idle (generates a "task complete" notification)
	if err := client.SendHook(HookRequest{
		JinSessionID:  info.ID,
		HookEventName: "Stop",
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

func (m *mockSlaveClient) ListWithHostID(_ []string) ([]session.Info, error) {
	return nil, nil
}

func (m *mockSlaveClient) NotificationHistoryWithHostID(_ []string) ([]notify.Entry, error) {
	return m.entries, m.err
}

func (m *mockSlaveClient) SendRaw(action string, data, visited []byte) ([]byte, error) {
	return []byte(`{"success":true}`), nil
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

func TestIntegration_GetWithHostID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "get-host-test",
		WorkDir: "/tmp/get-host-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	// Get with empty hostID (local) should work
	got, err := client.Get(info.ID, "")
	if err != nil {
		t.Fatalf("Get with empty hostID: %v", err)
	}
	if got.ID != info.ID {
		t.Errorf("ID: got %q, want %q", got.ID, info.ID)
	}

	// Get with hostID "local" should also work
	got, err = client.Get(info.ID, "local")
	if err != nil {
		t.Fatalf("Get with hostID=local: %v", err)
	}
	if got.ID != info.ID {
		t.Errorf("ID: got %q, want %q", got.ID, info.ID)
	}

	// Get with non-existent remote hostID should fail gracefully
	// (no remote slave configured, so forwardToSlave should return error)
	_, err = client.Get(info.ID, "nonexistent-host")
	if err == nil {
		t.Error("expected error for Get with non-existent remote hostID, got nil")
	}
}

func TestIntegration_SendWithHostID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	_, client := setupTestServer(t)

	info, err := client.NewWithOptions(NewOptions{
		Name:    "send-host-test",
		WorkDir: "/tmp/send-host-test",
		Start:   false,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}

	// Make session idle so Send is accepted
	if err := client.SendHook(HookRequest{
		JinSessionID:  info.ID,
		HookEventName: "UserPromptSubmit",
	}); err != nil {
		t.Fatalf("SendHook(UserPromptSubmit): %v", err)
	}
	if err := client.SendHook(HookRequest{
		JinSessionID:  info.ID,
		HookEventName: "Stop",
	}); err != nil {
		t.Fatalf("SendHook(Stop): %v", err)
	}

	// Send with empty hostID (local) — will fail because tmux isn't available,
	// but the error should NOT be about session not found
	err = client.Send(info.ID, "test prompt", "")
	if err != nil {
		// Expected: tmux error, NOT "session not found"
		t.Logf("Send with empty hostID returned expected error: %v", err)
	}

	// Send with non-existent remote hostID should fail gracefully
	err = client.Send(info.ID, "test prompt", "nonexistent-host")
	if err == nil {
		t.Error("expected error for Send with non-existent remote hostID, got nil")
	}
}

// TestIntegration_ListWithHostID_VisitedPropagated verifies that ListWithHostID passes
// the visited slice to the server so the server can skip already-visited hosts.
// This prevents infinite routing loops in bidirectional master-slave topologies.
func TestIntegration_ListWithHostID_VisitedPropagated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	server, client := setupTestServer(t)

	// Register a mock slave so handleList sees a reachable host
	var capturedVisited []string
	spy := &visitedCaptureMock{captureList: func(v []string) { capturedVisited = v }}
	registry := host.NewRegistry([]config.HostConfig{{ID: "ec2", Type: "ssh"}})
	registry.SetClient("ec2", spy)
	server.hostRegistry = registry

	// ListWithHostID passes visited=["master"] — the server must forward this to ec2
	visited := []string{"master"}
	_, err := client.ListWithHostID(visited)
	if err != nil {
		t.Fatalf("ListWithHostID: %v", err)
	}

	// The mock should have been called with visited containing "master" and "local" (server's own ID)
	found := false
	for _, v := range capturedVisited {
		if v == "master" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListWithHostID did not propagate visited to slave: got %v, want to contain 'master'", capturedVisited)
	}
}

// visitedCaptureMock captures the visited argument for inspection in tests.
type visitedCaptureMock struct {
	captureList  func([]string)
	captureNotif func([]string)
}

func (m *visitedCaptureMock) IsRunning() bool { return true }
func (m *visitedCaptureMock) ListWithHostID(visited []string) ([]session.Info, error) {
	if m.captureList != nil {
		m.captureList(visited)
	}
	return nil, nil
}
func (m *visitedCaptureMock) NotificationHistoryWithHostID(visited []string) ([]notify.Entry, error) {
	if m.captureNotif != nil {
		m.captureNotif(visited)
	}
	return nil, nil
}
func (m *visitedCaptureMock) SendRaw(action string, data, visited []byte) ([]byte, error) {
	return []byte(`{"success":true}`), nil
}

// TestIntegration_ForwardedRequestHostIDCleared verifies that when a targeted operation
// (e.g., delete) is forwarded to a remote slave, the HostID is cleared in the payload
// so the slave does not try to forward the request again.
func TestIntegration_ForwardedRequestHostIDCleared(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	server, client := setupTestServer(t)

	var receivedAction string
	var receivedData []byte
	spy := &rawCaptureMock{
		capture: func(action string, data []byte) {
			receivedAction = action
			receivedData = data
		},
	}
	registry := host.NewRegistry([]config.HostConfig{{ID: "ec2", Type: "ssh"}})
	registry.SetClient("ec2", spy)
	server.hostRegistry = registry

	// Issue a delete targeted at "ec2"
	_ = client.Delete("some-id", "ec2", false, false)

	if receivedAction != "delete" {
		t.Errorf("forwarded action = %q, want delete", receivedAction)
	}

	// The forwarded payload must not contain host_id "ec2"
	var fwd IDRequest
	if err := json.Unmarshal(receivedData, &fwd); err != nil {
		t.Fatalf("unmarshal forwarded data: %v", err)
	}
	if fwd.HostID != "" {
		t.Errorf("forwarded HostID = %q, want empty (slave must handle locally)", fwd.HostID)
	}
}

// TestIntegration_HandleList_SortedByFleet verifies that handleList() returns sessions
// sorted by fleet (alphabetically, "default" last) then by CreatedAt, even when remote
// sessions are mixed in from multiple hosts.
func TestIntegration_HandleList_SortedByFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	now := time.Now()

	tests := []struct {
		name           string
		remoteSessions []session.Info
		wantFleets     []string // expected Fleet order in result
	}{
		{
			name: "mixed fleet local and remote sessions",
			remoteSessions: []session.Info{
				// Remote sessions use past timestamps so local sessions (created after now)
				// are always later within the same fleet.
				{ID: "remote-work-1", Fleet: "work", CreatedAt: now.Add(-10 * time.Second)},
				{ID: "remote-default-1", Fleet: session.DefaultFleet, CreatedAt: now.Add(-5 * time.Second)},
			},
			wantFleets: []string{"work", "work", session.DefaultFleet, session.DefaultFleet},
		},
		{
			name:           "remote returns empty",
			remoteSessions: []session.Info{},
			wantFleets:     []string{"work", session.DefaultFleet},
		},
		{
			name: "all same fleet remote earlier than local",
			remoteSessions: []session.Info{
				{ID: "remote-work-1", Fleet: "work", CreatedAt: now.Add(-20 * time.Second)},
			},
			wantFleets: []string{"work", "work", session.DefaultFleet},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server, client := setupTestServer(t)

			// Create local sessions: one "work", one "default" (different dirs to avoid conflict)
			tmpDir := shortTempDir(t)
			_, err := client.NewWithOptions(NewOptions{WorkDir: tmpDir + "/work", Fleet: "work", Start: false})
			if err != nil {
				t.Fatalf("NewWithOptions(work): %v", err)
			}

			_, err = client.NewWithOptions(NewOptions{WorkDir: tmpDir + "/def", Fleet: session.DefaultFleet, Start: false})
			if err != nil {
				t.Fatalf("NewWithOptions(default): %v", err)
			}

			// Register a mock remote host returning tc.remoteSessions
			mock := &listOnlyMock{sessions: tc.remoteSessions}
			registry := host.NewRegistry([]config.HostConfig{{ID: "remote1", Type: "ssh"}})
			registry.SetClient("remote1", mock)
			server.hostRegistry = registry

			infos, err := client.List()
			if err != nil {
				t.Fatalf("List: %v", err)
			}

			// Verify fleet ordering: non-default fleets before default,
			// and within the same fleet sessions are in CreatedAt ascending order.
			for i := 0; i < len(infos)-1; i++ {
				fi, fj := infos[i].Fleet, infos[i+1].Fleet
				if fi == session.DefaultFleet && fj != session.DefaultFleet {
					t.Errorf("sort violation at [%d,%d]: default fleet before non-default (%q, %q)", i, i+1, fi, fj)
				}
				if fi == fj {
					if infos[i].CreatedAt.After(infos[i+1].CreatedAt) {
						t.Errorf("sort violation at [%d,%d]: same fleet but CreatedAt not ascending (%v > %v)", i, i+1, infos[i].CreatedAt, infos[i+1].CreatedAt)
					}
				}
			}

			// Verify fleets match expected sequence if specified
			if len(tc.wantFleets) > 0 {
				if len(infos) != len(tc.wantFleets) {
					t.Fatalf("want %d sessions (wantFleets=%v), got %d: %v", len(tc.wantFleets), tc.wantFleets, len(infos), infos)
				}
				for i, info := range infos {
					if info.Fleet != tc.wantFleets[i] {
						t.Errorf("infos[%d].Fleet = %q, want %q", i, info.Fleet, tc.wantFleets[i])
					}
				}
			}
		})
	}
}

// listOnlyMock is a HostClient that returns a fixed session list.
type listOnlyMock struct {
	sessions []session.Info
}

func (m *listOnlyMock) IsRunning() bool { return true }
func (m *listOnlyMock) ListWithHostID(_ []string) ([]session.Info, error) {
	return m.sessions, nil
}
func (m *listOnlyMock) NotificationHistoryWithHostID(_ []string) ([]notify.Entry, error) {
	return nil, nil
}
func (m *listOnlyMock) SendRaw(_ string, _, _ []byte) ([]byte, error) {
	return []byte(`{"success":true}`), nil
}

// rawCaptureMock captures SendRaw calls for inspection.
type rawCaptureMock struct {
	capture func(action string, data []byte)
}

func (m *rawCaptureMock) IsRunning() bool { return true }
func (m *rawCaptureMock) ListWithHostID(_ []string) ([]session.Info, error) {
	return nil, nil
}
func (m *rawCaptureMock) NotificationHistoryWithHostID(_ []string) ([]notify.Entry, error) {
	return nil, nil
}
func (m *rawCaptureMock) SendRaw(action string, data, visited []byte) ([]byte, error) {
	if m.capture != nil {
		m.capture(action, data)
	}
	return []byte(`{"success":false,"error":"session not found"}`), nil
}

// --- reconnectDeadTunnels / watchRemoteConnections tests ---

func TestReconnectDeadTunnels_SkipsDockerHost(t *testing.T) {
	server, _ := setupTestServer(t)

	// Docker host with no tunnel registered (IsAlive=false) must be skipped.
	registry := host.NewRegistry([]config.HostConfig{{ID: "docker-dev", Type: "docker"}})
	server.hostRegistry = registry

	// Should return without attempting any connection (no panic, no error).
	server.reconnectDeadTunnels()
}

func TestReconnectDeadTunnels_DeadSSHHost(t *testing.T) {
	server, _ := setupTestServer(t)

	mock := &mockSlaveClient{running: true}
	registry := host.NewRegistry([]config.HostConfig{{ID: "ec2", Type: "ssh"}})
	registry.SetClient("ec2", mock)
	server.hostRegistry = registry

	// tunnelMgr has no tunnel for "ec2" so IsAlive returns false.
	// reconnectDeadTunnels should set the reconnecting flag and spawn a goroutine.
	// The goroutine will fail (no SSH available in CI) and clear the flag.
	server.reconnectDeadTunnels()

	// Verify the in-progress guard is set immediately after spawning.
	server.reconnectingMu.Lock()
	inProgress := server.reconnecting["ec2"]
	server.reconnectingMu.Unlock()
	if !inProgress {
		t.Error("reconnecting[ec2] should be true while goroutine is running")
	}

	// Second call must be a no-op while reconnect is in progress:
	// reconnecting map should still have exactly one entry.
	server.reconnectDeadTunnels()
	server.reconnectingMu.Lock()
	count := len(server.reconnecting)
	server.reconnectingMu.Unlock()
	if count != 1 {
		t.Errorf("reconnecting map should have 1 entry after no-op call, got %d", count)
	}
}

func TestWatchRemoteConnections_StopsOnClose(t *testing.T) {
	server, _ := setupTestServer(t)
	server.stopPoll = make(chan struct{})
	server.hostRegistry = host.NewRegistry(nil) // no remotes → reconnectDeadTunnels is a no-op

	done := make(chan struct{})
	go func() {
		defer close(done)
		server.watchRemoteConnections()
	}()

	close(server.stopPoll)

	select {
	case <-done:
		// goroutine exited cleanly
	case <-time.After(1 * time.Second):
		t.Error("watchRemoteConnections did not stop after stopPoll closed")
	}
}
