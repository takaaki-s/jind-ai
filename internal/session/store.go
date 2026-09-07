package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/takaaki-s/jind-ai/internal/atomicfile"
)

// Store handles session persistence
type Store struct {
	dataDir string
	// saveMu serializes Save's read-merge-write section. It orders nothing
	// about the callers — two goroutines may still take it in the opposite
	// order to the mutations they represent — it only makes each merge see a
	// whole file rather than half of another Save's.
	//
	// It is taken under Manager.mu (startSessionTmux saves with the lock
	// held), so Store must stay a leaf: nothing reached from here may call
	// back into Manager, or the two locks close a cycle.
	saveMu sync.Mutex
}

// tmpSuffixPattern is appended to a session id to form the os.CreateTemp
// pattern Save hands to atomicfile.Write. The trailing ".tmp" keeps LoadAll
// (which only considers ".json") from picking the file up mid-write, and
// cleanupTempFiles globs the same suffix to reclaim strays. It is a contract
// between three places, which is why atomicfile.Write takes it rather than
// choosing a name.
const tmpSuffixPattern = ".json.*.tmp"

// sessionFileMode is the permission new session files are created with.
// Session records live under XDG state and are per-user data, so they are not
// world-readable.
const sessionFileMode os.FileMode = 0600

// atomicWrite is a variable so tests can capture the temp pattern Save builds.
// That pattern is a contract with LoadAll and cleanupTempFiles, and now that it
// crosses a package boundary as an argument, nothing else would catch it
// drifting.
var atomicWrite = atomicfile.Write

// NewStore creates a new store
func NewStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	s := &Store{dataDir: dataDir}
	s.cleanupTempFiles()
	return s, nil
}

// cleanupTempFiles removes temp files stranded by a Save interrupted between
// CreateTemp and Rename (daemon killed, power loss). They are inert — LoadAll
// ignores them — but nothing else would ever reclaim them.
//
// Only safe to call at construction: it would delete the in-flight temp file of
// a Save running concurrently in another process.
func (s *Store) cleanupTempFiles() {
	matches, err := filepath.Glob(filepath.Join(s.dataDir, "*"+tmpSuffixPattern))
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			debugLog("[STORE] stale temp file %s: %v", m, err)
		}
	}
}

// Save persists a session.
//
// Attention is merged with what is already on disk rather than overwritten, so
// a stale snapshot cannot roll a receipt back; every other field is
// last-writer-wins. A caller cannot lower attention through Save.
//
// The write is atomic (see atomicfile.Write). Several goroutines reach Save
// without holding a shared lock, and a half-written record is one LoadAll
// skips — the session disappears. The rename buys atomicity, not durability.
//
// Save takes session by value so the copy happens at the call site: a caller
// reading a live *Session outside a lock would otherwise race with concurrent
// mutators. See Manager.snapshotAndUnlock and its callers for the pattern.
func (s *Store) Save(session Session) error {
	path := filepath.Join(s.dataDir, session.ID+".json")

	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	// Preserve the mode of an existing record so a user who tightened (or
	// loosened) it does not have it reset on every save.
	mode := sessionFileMode
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}

	session.Attention = mergeAttention(session.Attention, persistedAttention(path))

	data, err := json.MarshalIndent(&session, "", "  ")
	if err != nil {
		return err
	}

	return atomicWrite(path, data, mode, session.ID+tmpSuffixPattern)
}

// persistedAttention reads just the attention member of an existing session
// file. Anything that stops it — no file yet, unreadable, unparseable — yields
// the zero value, which merges as "no information" and leaves the candidate
// untouched. Save must not fail because the record it is replacing is broken.
func persistedAttention(path string) Attention {
	data, err := os.ReadFile(path)
	if err != nil {
		// A record that does not exist yet is the ordinary case on first save
		// and says nothing. Anything else is a read that should have worked,
		// and it costs a receipt silently — the merge then takes the
		// candidate's attention whole, stale or not.
		if !os.IsNotExist(err) {
			debugLog("[STORE] attention probe failed for %s: %v", path, err)
		}
		return Attention{}
	}
	var probe struct {
		Attention Attention `json:"attention"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		debugLog("[STORE] attention probe could not parse %s: %v", path, err)
		return Attention{}
	}
	return probe.Attention
}

// Load loads a session by ID. Legacy schema (top-level "name") is migrated
// in-place to the current schema; the migrated JSON is written back to disk
// so we only pay the cost once per session file.
func (s *Store) Load(id string) (*Session, error) {
	path := filepath.Join(s.dataDir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	migrated, changed, err := migrateSessionJSON(data)
	if err != nil {
		return nil, err
	}

	var session Session
	if err := json.Unmarshal(migrated, &session); err != nil {
		return nil, err
	}

	if changed {
		if err := s.Save(session); err != nil {
			return nil, err
		}
	}
	return &session, nil
}

// LoadAll loads all sessions. A file that fails Load (unparseable JSON,
// migration write-back failure, missing permissions, ...) is skipped rather
// than aborting the lot, so one corrupt file cannot strand every session; the
// individual failure still surfaces via debugLog.
func (s *Store) LoadAll() ([]*Session, error) {
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sessions []*Session
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-5] // Remove .json
		session, err := s.Load(id)
		if err != nil {
			debugLog("[LOAD] skip %s: %v", id, err)
			continue
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

// Delete removes a session file
func (s *Store) Delete(id string) error {
	path := filepath.Join(s.dataDir, id+".json")
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}
