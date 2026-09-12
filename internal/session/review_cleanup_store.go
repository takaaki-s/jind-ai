package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/takaaki-s/jind-ai/internal/atomicfile"
)

var cleanupSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type ReviewCleanupStore struct {
	dataDir string
	mu      sync.Mutex
}

func NewReviewCleanupStore(dataDir string) (*ReviewCleanupStore, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	s := &ReviewCleanupStore{dataDir: dataDir}
	s.cleanupTempFiles()
	return s, nil
}

func (s *ReviewCleanupStore) cleanupTempFiles() {
	matches, err := filepath.Glob(filepath.Join(s.dataDir, "*.json.*.tmp"))
	if err != nil {
		return
	}
	for _, match := range matches {
		if err := os.Remove(match); err != nil {
			debugLog("[CLEANUP] stale temp file %s: %v", match, err)
		}
	}
}

func (s *ReviewCleanupStore) Save(journal ReviewCleanupJournal) error {
	if !cleanupSessionIDPattern.MatchString(journal.Plan.SessionID) {
		return fmt.Errorf("invalid cleanup session id %q", journal.Plan.SessionID)
	}
	if err := validateReviewCleanupJournal(journal); err != nil {
		return err
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name := journal.Plan.SessionID + ".json"
	return atomicfile.Write(filepath.Join(s.dataDir, name), data, sessionFileMode, name+".*.tmp")
}

func (s *ReviewCleanupStore) Load(sessionID string) (ReviewCleanupJournal, error) {
	if !cleanupSessionIDPattern.MatchString(sessionID) {
		return ReviewCleanupJournal{}, fmt.Errorf("invalid cleanup session id %q", sessionID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.dataDir, sessionID+".json"))
	if err != nil {
		return ReviewCleanupJournal{}, err
	}
	var journal ReviewCleanupJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return ReviewCleanupJournal{}, err
	}
	if journal.Plan.SessionID != sessionID {
		return ReviewCleanupJournal{}, fmt.Errorf("cleanup journal identity mismatch for %q", sessionID)
	}
	if err := validateReviewCleanupJournal(journal); err != nil {
		return ReviewCleanupJournal{}, err
	}
	return journal, nil
}

func (s *ReviewCleanupStore) LoadAll() ([]ReviewCleanupJournal, error) {
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		return nil, err
	}
	var journals []ReviewCleanupJournal
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !cleanupSessionIDPattern.MatchString(id) {
			continue
		}
		journal, err := s.Load(id)
		if err != nil {
			debugLog("[CLEANUP] skip journal %s: %v", id, err)
			continue
		}
		journals = append(journals, journal)
	}
	return journals, nil
}
