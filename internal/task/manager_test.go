package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/session"
)

type fakeSessions struct {
	mu    sync.RWMutex
	infos map[string]session.Info
}

func (f *fakeSessions) GetInfo(id string) (session.Info, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	info, ok := f.infos[id]
	return info, ok
}

func (f *fakeSessions) delete(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.infos, id)
}

func TestManager_CreateAppendAndRestartRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	sessions := &fakeSessions{infos: map[string]session.Info{
		"session-a": {ID: "session-a", Status: session.StatusIdle},
		"session-b": {ID: "session-b", Status: session.StatusThinking},
	}}
	m, err := NewManager(dir, sessions)
	if err != nil {
		t.Fatal(err)
	}
	created, err := m.Create(CreateOptions{
		Title: "  Fix flaky build  ", Source: Source{Kind: "issue", Ref: "github:42"},
		RequestedBase: "origin/main", PromptSummary: "Investigate the bounded symptom",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Title != "Fix flaky build" || len(created.Executions) != 0 {
		t.Fatalf("created = %+v", created)
	}
	first, err := m.AppendExecution(created.ID, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.AppendExecution(created.ID, "session-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Executions) != 2 || second.Executions[0].Sequence != 1 || second.Executions[1].Sequence != 2 {
		t.Fatalf("executions = %+v", second.Executions)
	}
	if second.Executions[0].ID == second.Executions[1].ID || second.Executions[0].ID == created.ID {
		t.Fatal("task and execution IDs must have independent identities")
	}
	if first.UpdatedAt.Before(created.UpdatedAt) || second.UpdatedAt.Before(first.UpdatedAt) {
		t.Fatal("updated_at moved backwards")
	}
	persisted, err := os.ReadFile(filepath.Join(dir, created.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(persisted, &record); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"prompt", "transcript", "session_status", "attention"} {
		if _, ok := record[forbidden]; ok {
			t.Errorf("persisted task unexpectedly contains %q", forbidden)
		}
	}

	restarted, err := NewManager(dir, sessions)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := restarted.Get(created.ID)
	if !ok {
		t.Fatal("task disappeared after restart")
	}
	if len(got.Executions) != 2 || got.Executions[1].SessionID != "session-b" {
		t.Fatalf("round-trip executions = %+v", got.Executions)
	}
	listed := restarted.List()
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("list = %+v", listed)
	}
}

func TestManager_ProjectsLatestAttentionAndMissingSession(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	sessions := &fakeSessions{infos: map[string]session.Info{
		"session-a": {
			ID: "session-a", Status: session.StatusIdle,
			Attention: session.AttentionInfo{State: session.AttentionDone, Generation: 2, SeenGeneration: 1, Unseen: true},
		},
	}}
	m, _ := NewManager(dir, sessions)
	created, _ := m.Create(CreateOptions{Title: "Review result"})
	withExecution, err := m.AppendExecution(created.ID, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if withExecution.LatestAttention == nil || !withExecution.LatestAttention.Attention.Unseen {
		t.Fatalf("latest attention = %+v", withExecution.LatestAttention)
	}

	sessions.delete("session-a")
	got, ok := m.Get(created.ID)
	if !ok {
		t.Fatal("task disappeared with its session")
	}
	if got.Executions[0].ReferenceState != ReferenceMissing || got.LatestAttention.ReferenceState != ReferenceMissing {
		t.Fatalf("deleted session projection = %+v", got)
	}
	if got.Executions[0].ID == "" || got.Executions[0].SessionID != "session-a" {
		t.Fatal("execution history was corrupted when session disappeared")
	}
}

func TestManager_ConcurrentAppendIsAtomicAndOrdered(t *testing.T) {
	const count = 32
	dir := filepath.Join(t.TempDir(), "tasks")
	infos := make(map[string]session.Info, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("session-%02d", i)
		infos[id] = session.Info{ID: id, Status: session.StatusStopped}
	}
	m, _ := NewManager(dir, &fakeSessions{infos: infos})
	created, _ := m.Create(CreateOptions{Title: "Parallel attempts"})
	doneReading := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		path := filepath.Join(dir, created.ID+".json")
		for {
			select {
			case <-doneReading:
				readErr <- nil
				return
			default:
				data, err := os.ReadFile(path)
				if err != nil {
					readErr <- err
					return
				}
				var value Task
				if err := json.Unmarshal(data, &value); err != nil {
					readErr <- err
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, count)
	for id := range infos {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := m.AppendExecution(created.ID, id)
			errs <- err
		}(id)
	}
	wg.Wait()
	close(errs)
	close(doneReading)
	if err := <-readErr; err != nil {
		t.Fatalf("reader observed a partial task file: %v", err)
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	restarted, err := NewManager(dir, &fakeSessions{infos: infos})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := restarted.Get(created.ID)
	if len(got.Executions) != count {
		t.Fatalf("got %d executions, want %d", len(got.Executions), count)
	}
	seen := make(map[string]bool, count)
	for i, execution := range got.Executions {
		if execution.Sequence != uint64(i+1) {
			t.Fatalf("sequence[%d] = %d", i, execution.Sequence)
		}
		if seen[execution.ID] {
			t.Fatalf("duplicate execution ID %s", execution.ID)
		}
		seen[execution.ID] = true
	}
}

func TestNewManager_DoesNotRewriteLegacyStateTree(t *testing.T) {
	stateDir := t.TempDir()
	sessionsDir := filepath.Join(stateDir, "sessions")
	if err := os.Mkdir(sessionsDir, 0755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(sessionsDir, "legacy.json")
	want := []byte(`{"id":"legacy","unknown_future_field":true}`)
	if err := os.WriteFile(legacyPath, want, 0600); err != nil {
		t.Fatal(err)
	}
	tasksDir := filepath.Join(stateDir, "tasks")

	if _, err := NewManager(tasksDir, &fakeSessions{infos: map[string]session.Info{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tasksDir); !os.IsNotExist(err) {
		t.Fatalf("task manager initialization created %s", tasksDir)
	}
	got, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("legacy state changed: %q", got)
	}
}

func TestManager_PreservesUnknownSourceKind(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	m, _ := NewManager(dir, &fakeSessions{infos: map[string]session.Info{}})
	created, err := m.Create(CreateOptions{Title: "Future source", Source: Source{Kind: "linear-v2", Ref: "ENG-8"}})
	if err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewManager(dir, &fakeSessions{infos: map[string]session.Info{}})
	got, _ := restarted.Get(created.ID)
	if got.Source.Kind != "linear-v2" {
		t.Fatalf("source kind = %q", got.Source.Kind)
	}
}

func TestStore_ZeroValueMigrationIsInMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	const id = "legacy-task"
	path := filepath.Join(dir, id+".json")
	want := []byte(`{"id":"legacy-task","title":"Legacy"}`)
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	values, err := NewStore(dir).LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].SchemaVersion != SchemaVersion || values[0].Source.Kind != "unknown" || values[0].Executions == nil {
		t.Fatalf("normalized value = %+v", values)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("load rewrote legacy task: %q", got)
	}
}

func TestStore_FutureSchemaFailsWithoutRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future-task.json")
	want := []byte(`{"schema_version":99,"id":"future-task","title":"Future","future_field":true}`)
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir).LoadAll(); err == nil {
		t.Fatal("future schema was accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("failed load rewrote future task: %q", got)
	}
}

func TestValidateCreateOptions_BoundsMetadata(t *testing.T) {
	tooLong := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = 'x'
		}
		return string(b)
	}
	tests := []CreateOptions{
		{Title: tooLong(MaxTitleLength + 1)},
		{Title: "x", Source: Source{Ref: tooLong(MaxSourceRefLength + 1)}},
		{Title: "x", RequestedBase: tooLong(MaxRequestedBaseLength + 1)},
		{Title: "x", PromptSummary: tooLong(MaxPromptSummaryLength + 1)},
	}
	for _, opts := range tests {
		if err := ValidateCreateOptions(opts); err == nil {
			t.Fatalf("accepted oversized metadata: %+v", opts)
		}
	}
}
