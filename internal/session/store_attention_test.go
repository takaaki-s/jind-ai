package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// preAttentionJSON is a session file at the current schema minus attention —
// every field a migration would otherwise rewrite is already present, so a
// test that asserts "the file was not touched" is measuring attention rather
// than an unrelated migration that happened not to fire.
const preAttentionJSON = `{
  "id": "pre-attention",
  "description": "an old session",
  "work_dir": "/tmp/pre-attention",
  "created_at": "2026-01-02T03:04:05Z",
  "status": "idle",
  "agent_kind": "claude",
  "agent_session_id": "agent-1",
  "agent_session_started": true,
  "agent_session_id_confirmed": true,
  "fleet": "default"
}`

func writeSessionFile(t *testing.T, dir, id, content string) string {
	t.Helper()
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// A record written before attention existed decodes to the zero value and is
// left on disk exactly as it was: attention needs no migration precisely
// because its zero value is what a missing key already means.
func TestStore_PreAttentionRecordLoadsZeroAndIsNotRewritten(t *testing.T) {
	store, dir := newTestStore(t)
	path := writeSessionFile(t, dir, "pre-attention", preAttentionJSON)

	loaded, err := store.Load("pre-attention")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Attention != (Attention{}) {
		t.Errorf("Attention = %+v, want the zero value", loaded.Attention)
	}
	if loaded.Attention.Unseen() {
		t.Error("a record that predates attention must not report a completion nobody saw")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after) != preAttentionJSON {
		t.Errorf("Load rewrote the file:\n got: %s\nwant: %s", after, preAttentionJSON)
	}
}

// The first completion on such a record starts at generation 1 — the counter
// resumes from the zero value rather than from anything synthesised.
func TestStore_PreAttentionRecordFirstCompletionIsGenerationOne(t *testing.T) {
	store, dir := newTestStore(t)
	writeSessionFile(t, dir, "pre-attention", preAttentionJSON)

	loaded, err := store.Load("pre-attention")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loaded.Attention = loaded.Attention.completed()
	if err := store.Save(*loaded); err != nil {
		t.Fatalf("Save: %v", err)
	}

	again, err := store.Load("pre-attention")
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	want := Attention{State: AttentionDone, Generation: 1}
	if again.Attention != want {
		t.Errorf("Attention = %+v, want %+v", again.Attention, want)
	}
}

// The logical race the merge exists for: two lock-correct mutations snapshot
// in one order and reach the file in the other. -race cannot see this — both
// writes are properly serialised — so it is pinned deterministically.
func TestStore_StaleSnapshotCannotRollAttentionBack(t *testing.T) {
	tests := []struct {
		name     string
		newer    Attention
		stale    Attention
		want     Attention
		wantSeen bool // Unseen() on the settled record
	}{
		{
			name:     "a stale pre-completion snapshot cannot erase the receipt",
			newer:    Attention{State: AttentionDone, Generation: 1},
			stale:    Attention{},
			want:     Attention{State: AttentionDone, Generation: 1},
			wantSeen: true,
		},
		{
			name:     "a stale pre-seen snapshot cannot resurrect an acknowledged receipt",
			newer:    Attention{State: AttentionDone, Generation: 1, SeenGeneration: 1},
			stale:    Attention{State: AttentionDone, Generation: 1},
			want:     Attention{State: AttentionDone, Generation: 1, SeenGeneration: 1},
			wantSeen: false,
		},
		{
			// The stale writer here is not an attention writer at all — a
			// description or CWD save carrying an old copy of the record. It
			// must not roll the receipt back either.
			name:     "an unrelated stale full-session save cannot roll it back",
			newer:    Attention{State: AttentionDone, Generation: 3, SeenGeneration: 2},
			stale:    Attention{State: AttentionDone, Generation: 1},
			want:     Attention{State: AttentionDone, Generation: 3, SeenGeneration: 2},
			wantSeen: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := newTestStore(t)
			base := Session{ID: "s1", Description: "race", AgentKind: "claude"}

			newer := base
			newer.Attention = tt.newer
			if err := store.Save(newer); err != nil {
				t.Fatalf("Save newer: %v", err)
			}

			stale := base
			stale.Attention = tt.stale
			if err := store.Save(stale); err != nil {
				t.Fatalf("Save stale: %v", err)
			}

			loaded, err := store.Load("s1")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if loaded.Attention != tt.want {
				t.Errorf("persisted Attention = %+v, want %+v", loaded.Attention, tt.want)
			}
			if got := loaded.Attention.Unseen(); got != tt.wantSeen {
				t.Errorf("Unseen() = %v, want %v", got, tt.wantSeen)
			}
		})
	}
}

// Under concurrency the file stays parseable and settles on the component-wise
// maximum whichever order the saves land in.
//
// Two things make the failure reproducible rather than lucky. The writers are
// released together from a barrier — Save's read-merge-write is a few hundred
// microseconds, so goroutines started in sequence mostly finish before the
// next one begins, and a run where nothing overlapped has tested nothing. And
// the whole phase repeats against a fresh store, because losing an update
// needs one writer's read-to-write window to contain another's entirely: one
// round can miss that interleaving, several in a row rarely do.
func TestStore_ConcurrentSavesConvergeToComponentWiseMaxima(t *testing.T) {
	const (
		writers = 16
		rounds  = 8
	)

	for round := range rounds {
		store, _ := newTestStore(t)
		base := Session{ID: "s1", Description: "concurrent", AgentKind: "claude"}

		start := make(chan struct{})
		var ready, done sync.WaitGroup
		ready.Add(writers)
		done.Add(writers)

		for i := range writers {
			go func(i int) {
				defer done.Done()
				sess := base
				// Every writer carries a different pair, so the settled record
				// is only right if the highest of each survived — a lost
				// update shows up as a lower number, not as a corrupt file.
				sess.Attention = Attention{
					State:          AttentionDone,
					Generation:     uint64(i + 1),
					SeenGeneration: uint64(i),
				}
				ready.Done()
				<-start
				if err := store.Save(sess); err != nil {
					t.Errorf("round %d: Save: %v", round, err)
				}
			}(i)
		}

		ready.Wait()
		close(start)
		done.Wait()

		loaded, err := store.Load("s1")
		if err != nil {
			t.Fatalf("round %d: Load: %v", round, err)
		}
		want := Attention{State: AttentionDone, Generation: writers, SeenGeneration: writers - 1}
		if loaded.Attention != want {
			t.Fatalf("round %d: settled Attention = %+v, want %+v", round, loaded.Attention, want)
		}
	}
}

// A record whose JSON no longer parses must not stop a Save: the merge treats
// an unreadable predecessor as "no information" and the new record replaces it.
func TestStore_SaveOverCorruptRecordStillWrites(t *testing.T) {
	store, dir := newTestStore(t)
	writeSessionFile(t, dir, "s1", "{ not json")

	sess := Session{ID: "s1", Description: "recovered", AgentKind: "claude"}
	sess.Attention = Attention{State: AttentionDone, Generation: 1}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save over a corrupt record: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "s1.json"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var back Session
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Attention != sess.Attention {
		t.Errorf("Attention = %+v, want %+v", back.Attention, sess.Attention)
	}
}
