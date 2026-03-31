package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestToInfo_CopiesAllFields(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s := &Session{
		ID:                   "test-id-123",
		Name:                 "my-session",
		WorkDir:              "/home/user/project",
		CreatedAt:            now,
		Status:               StatusThinking,
		LastActiveAt:         now.Add(-5 * time.Minute),
		ErrorMessage:         "something went wrong",
		ClaudeSessionID:      "claude-sess-456",
		ClaudeSessionStarted: true,
		Fleet:                "backend",
		HostID:               "ec2-instance",
		TmuxWindowName:       "ccvalet_test-id-123",
		TmuxPaneID:           "%42",

		// Runtime fields (should NOT appear in Info but CurrentWorkDir/CurrentBranch are mapped)
		LastOutputTime: now.Add(-1 * time.Minute),
		StartedAt:      now.Add(-10 * time.Minute),
		SSHAuthSock:    "/tmp/ssh-agent.sock",
		CurrentWorkDir: "/home/user/project/subdir",
		CurrentBranch:  "feature/cool",
		IsGitRepo:      true,
		GitRepoRoot:    "project",
	}

	info := s.ToInfo()

	// Verify all mapped fields
	if info.ID != s.ID {
		t.Errorf("ID: got %q, want %q", info.ID, s.ID)
	}
	if info.Name != s.Name {
		t.Errorf("Name: got %q, want %q", info.Name, s.Name)
	}
	if info.WorkDir != s.WorkDir {
		t.Errorf("WorkDir: got %q, want %q", info.WorkDir, s.WorkDir)
	}
	if info.Status != s.Status {
		t.Errorf("Status: got %q, want %q", info.Status, s.Status)
	}
	if !info.CreatedAt.Equal(s.CreatedAt) {
		t.Errorf("CreatedAt: got %v, want %v", info.CreatedAt, s.CreatedAt)
	}
	if !info.LastActiveAt.Equal(s.LastActiveAt) {
		t.Errorf("LastActiveAt: got %v, want %v", info.LastActiveAt, s.LastActiveAt)
	}
	if info.ErrorMessage != s.ErrorMessage {
		t.Errorf("ErrorMessage: got %q, want %q", info.ErrorMessage, s.ErrorMessage)
	}
	if info.ClaudeSessionID != s.ClaudeSessionID {
		t.Errorf("ClaudeSessionID: got %q, want %q", info.ClaudeSessionID, s.ClaudeSessionID)
	}
	if info.TmuxWindowName != s.TmuxWindowName {
		t.Errorf("TmuxWindowName: got %q, want %q", info.TmuxWindowName, s.TmuxWindowName)
	}
	if info.Fleet != s.Fleet {
		t.Errorf("Fleet: got %q, want %q", info.Fleet, s.Fleet)
	}
	if info.HostID != s.HostID {
		t.Errorf("HostID: got %q, want %q", info.HostID, s.HostID)
	}
	if info.CurrentWorkDir != s.CurrentWorkDir {
		t.Errorf("CurrentWorkDir: got %q, want %q", info.CurrentWorkDir, s.CurrentWorkDir)
	}
	if info.CurrentBranch != s.CurrentBranch {
		t.Errorf("CurrentBranch: got %q, want %q", info.CurrentBranch, s.CurrentBranch)
	}
	if info.GitRepoRoot != s.GitRepoRoot {
		t.Errorf("GitRepoRoot: got %q, want %q", info.GitRepoRoot, s.GitRepoRoot)
	}

	// LastUserMessage and LastAssistantMessage are NOT set by ToInfo (populated elsewhere)
	if info.LastUserMessage != "" {
		t.Errorf("LastUserMessage: expected empty, got %q", info.LastUserMessage)
	}
	if info.LastAssistantMessage != "" {
		t.Errorf("LastAssistantMessage: expected empty, got %q", info.LastAssistantMessage)
	}
}

func TestStatus_StringValues(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{StatusCreating, "creating"},
		{StatusStopped, "stopped"},
		{StatusRunning, "running"},
		{StatusIdle, "idle"},
		{StatusThinking, "thinking"},
		{StatusPermission, "permission"},
	}

	for _, tt := range tests {
		if string(tt.status) != tt.want {
			t.Errorf("Status %v: got %q, want %q", tt.status, string(tt.status), tt.want)
		}
	}
}

func TestSession_JSONRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	original := &Session{
		ID:                   "round-trip-id",
		Name:                 "roundtrip",
		WorkDir:              "/tmp/work",
		CreatedAt:            now,
		Status:               StatusIdle,
		LastActiveAt:         now.Add(-2 * time.Minute),
		ErrorMessage:         "test error",
		ClaudeSessionID:      "claude-rt-789",
		ClaudeSessionStarted: true,
		Fleet:                "frontend",
		HostID:               "docker-dev",
		TmuxWindowName:       "ccvalet_round-trip-id",
		TmuxPaneID:           "%99",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var restored Session
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Verify all persisted fields survive the round trip
	if restored.ID != original.ID {
		t.Errorf("ID: got %q, want %q", restored.ID, original.ID)
	}
	if restored.Name != original.Name {
		t.Errorf("Name: got %q, want %q", restored.Name, original.Name)
	}
	if restored.WorkDir != original.WorkDir {
		t.Errorf("WorkDir: got %q, want %q", restored.WorkDir, original.WorkDir)
	}
	if !restored.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("CreatedAt: got %v, want %v", restored.CreatedAt, original.CreatedAt)
	}
	if restored.Status != original.Status {
		t.Errorf("Status: got %q, want %q", restored.Status, original.Status)
	}
	if !restored.LastActiveAt.Equal(original.LastActiveAt) {
		t.Errorf("LastActiveAt: got %v, want %v", restored.LastActiveAt, original.LastActiveAt)
	}
	if restored.ErrorMessage != original.ErrorMessage {
		t.Errorf("ErrorMessage: got %q, want %q", restored.ErrorMessage, original.ErrorMessage)
	}
	if restored.ClaudeSessionID != original.ClaudeSessionID {
		t.Errorf("ClaudeSessionID: got %q, want %q", restored.ClaudeSessionID, original.ClaudeSessionID)
	}
	if restored.ClaudeSessionStarted != original.ClaudeSessionStarted {
		t.Errorf("ClaudeSessionStarted: got %v, want %v", restored.ClaudeSessionStarted, original.ClaudeSessionStarted)
	}
	if restored.Fleet != original.Fleet {
		t.Errorf("Fleet: got %q, want %q", restored.Fleet, original.Fleet)
	}
	if restored.HostID != original.HostID {
		t.Errorf("HostID: got %q, want %q", restored.HostID, original.HostID)
	}
	if restored.TmuxWindowName != original.TmuxWindowName {
		t.Errorf("TmuxWindowName: got %q, want %q", restored.TmuxWindowName, original.TmuxWindowName)
	}
	if restored.TmuxPaneID != original.TmuxPaneID {
		t.Errorf("TmuxPaneID: got %q, want %q", restored.TmuxPaneID, original.TmuxPaneID)
	}
}

func TestSession_FleetAlwaysPresentInJSON(t *testing.T) {
	s := &Session{
		ID:      "fleet-json-test",
		Name:    "fj",
		WorkDir: "/tmp/fj",
		Fleet:   DefaultFleet,
		Status:  StatusIdle,
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if !strings.Contains(string(data), `"fleet":"default"`) {
		t.Errorf("JSON missing fleet field: %s", data)
	}
}

func TestSession_JSONOmitsRuntimeFields(t *testing.T) {
	now := time.Now()
	s := &Session{
		ID:        "omit-test",
		Name:      "omit",
		WorkDir:   "/tmp/omit",
		CreatedAt: now,
		Status:    StatusRunning,

		// Runtime fields (json:"-") -- these should NOT appear in JSON
		LastOutputTime: now.Add(-1 * time.Minute),
		StartedAt:      now.Add(-5 * time.Minute),
		SSHAuthSock:    "/tmp/ssh.sock",
		CurrentWorkDir: "/tmp/omit/sub",
		CurrentBranch:  "main",
		IsGitRepo:      true,
		GitRepoRoot:    "omit",
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	jsonStr := string(data)

	// These field names must NOT appear in the JSON output
	runtimeFields := []string{
		"LastOutputTime",
		"last_output_time",
		"StartedAt",
		"started_at",
		"SSHAuthSock",
		"ssh_auth_sock",
		"CurrentWorkDir",
		"current_work_dir",
		"CurrentBranch",
		"current_branch",
		"IsGitRepo",
		"is_git_repo",
		"GitRepoRoot",
		"git_repo_root",
	}

	for _, field := range runtimeFields {
		if strings.Contains(jsonStr, field) {
			t.Errorf("JSON output should not contain runtime field %q, but got: %s", field, jsonStr)
		}
	}

	// Sanity check: persisted fields SHOULD be present
	persistedFields := []string{"id", "name", "work_dir", "status"}
	for _, field := range persistedFields {
		if !strings.Contains(jsonStr, field) {
			t.Errorf("JSON output should contain persisted field %q, but got: %s", field, jsonStr)
		}
	}
}

func TestSortInfos(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)
	t2 := t0.Add(2 * time.Second)

	tests := []struct {
		name  string
		input []Info
		want  []string // expected ID order
	}{
		{
			name:  "empty slice",
			input: []Info{},
			want:  []string{},
		},
		{
			name:  "single element",
			input: []Info{{ID: "a", Fleet: "work", CreatedAt: t0}},
			want:  []string{"a"},
		},
		{
			name: "default fleet sorts last",
			input: []Info{
				{ID: "d", Fleet: DefaultFleet, CreatedAt: t0},
				{ID: "w", Fleet: "work", CreatedAt: t0},
			},
			want: []string{"w", "d"},
		},
		{
			name: "non-default fleets sorted alphabetically",
			input: []Info{
				{ID: "z", Fleet: "zz", CreatedAt: t0},
				{ID: "a", Fleet: "aa", CreatedAt: t0},
				{ID: "d", Fleet: DefaultFleet, CreatedAt: t0},
			},
			want: []string{"a", "z", "d"},
		},
		{
			name: "three non-default fleets sorted lexicographically",
			input: []Info{
				{ID: "c", Fleet: "cc", CreatedAt: t0},
				{ID: "a", Fleet: "aa", CreatedAt: t0},
				{ID: "b", Fleet: "bb", CreatedAt: t0},
				{ID: "d", Fleet: DefaultFleet, CreatedAt: t0},
			},
			want: []string{"a", "b", "c", "d"},
		},
		{
			name: "same fleet sorted by CreatedAt ascending",
			input: []Info{
				{ID: "new", Fleet: "work", CreatedAt: t2},
				{ID: "old", Fleet: "work", CreatedAt: t0},
				{ID: "mid", Fleet: "work", CreatedAt: t1},
			},
			want: []string{"old", "mid", "new"},
		},
		{
			name: "SliceStable preserves original order for equal CreatedAt",
			input: []Info{
				{ID: "first", Fleet: "work", CreatedAt: t0},
				{ID: "second", Fleet: "work", CreatedAt: t0},
			},
			want: []string{"first", "second"},
		},
		{
			name: "only default fleet",
			input: []Info{
				{ID: "b", Fleet: DefaultFleet, CreatedAt: t1},
				{ID: "a", Fleet: DefaultFleet, CreatedAt: t0},
			},
			want: []string{"a", "b"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			SortInfos(tc.input)
			if len(tc.input) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(tc.input), len(tc.want))
			}
			for i, info := range tc.input {
				if info.ID != tc.want[i] {
					t.Errorf("[%d] ID = %q, want %q", i, info.ID, tc.want[i])
				}
			}
		})
	}
}
