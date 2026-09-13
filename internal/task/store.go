package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/takaaki-s/jind-ai/internal/atomicfile"
)

const (
	taskFileMode    os.FileMode = 0600
	taskTempPattern             = ".json.*.tmp"
)

// Store persists one aggregate per JSON file. Construction and reads are lazy:
// merely upgrading/restarting jind-ai does not rewrite an existing state tree.
type Store struct {
	dir string
	mu  sync.Mutex
}

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Save(value Task) error {
	if value.ID == "" || strings.ContainsAny(value.ID, `/\\`) {
		return fmt.Errorf("invalid task id")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(s.dir, value.ID+".json")
	mode := taskFileMode
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	return atomicfile.Write(path, data, mode, value.ID+taskTempPattern)
}

func (s *Store) LoadAll() ([]Task, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return []Task{}, nil
	}
	if err != nil {
		return nil, err
	}
	values := make([]Task, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var value Task
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, fmt.Errorf("load task %s: %w", entry.Name(), err)
		}
		if value.SchemaVersion > SchemaVersion {
			return nil, fmt.Errorf("load task %s: schema version %d is newer than supported version %d", entry.Name(), value.SchemaVersion, SchemaVersion)
		}
		normalize(&value)
		fileID := strings.TrimSuffix(entry.Name(), ".json")
		if value.ID == "" || value.ID != fileID {
			return nil, fmt.Errorf("load task %s: record id %q does not match filename", entry.Name(), value.ID)
		}
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].CreatedAt.Equal(values[j].CreatedAt) {
			return values[i].ID < values[j].ID
		}
		return values[i].CreatedAt.Before(values[j].CreatedAt)
	})
	return values, nil
}
