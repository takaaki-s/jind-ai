package session

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Store handles session persistence
type Store struct {
	dataDir string
}

// tmpSuffixPattern is appended to a session id to form the os.CreateTemp
// pattern used by Save. The trailing ".tmp" keeps LoadAll from picking the
// file up mid-write, since LoadAll only considers a ".json" extension.
const tmpSuffixPattern = ".json.*.tmp"

// sessionFileMode is the permission new session files are created with.
// Session records live under XDG state and are per-user data, so they are not
// world-readable.
const sessionFileMode os.FileMode = 0600

// NewStore creates a new store
func NewStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	s := &Store{dataDir: dataDir}
	s.cleanupTempFiles()
	return s, nil
}

// cleanupTempFiles removes temp files stranded by a Save that was interrupted
// between CreateTemp and Rename (daemon killed, power loss). They are inert —
// LoadAll ignores them — but nothing else would ever reclaim them.
//
// Only safe to call at construction: it would delete the in-flight temp file of
// a Save running concurrently in another process. The daemon socket keeps a
// single daemon per state dir, so that does not arise in practice.
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
// The write goes to a temp file in the same directory and is then renamed over
// the target. Several goroutines reach Save without holding a shared lock, so a
// plain os.WriteFile could interleave two truncate/write pairs and leave a
// half-written record — which LoadAll then skips, making the session disappear.
// Rename is atomic against a concurrent writer, so a reader only ever sees a
// complete file.
//
// This buys atomicity, not durability: the data is not fsynced, so a machine
// crash can still lose the most recent save. Making that guarantee would need
// the file and the parent directory synced, which costs roughly an order of
// magnitude more per save than the whole write does today.
//
// Note that Save marshals every field of session, so a caller reading a live
// session must either hold the manager lock or pass a copy. Most callers
// currently pass the live pointer after unlocking; TryUpgradeDescription is
// the one that passes a copy.
func (s *Store) Save(session *Session) error {
	path := filepath.Join(s.dataDir, session.ID+".json")
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}

	// Preserve the mode of an existing record so a user who tightened (or
	// loosened) it does not have it reset on every save.
	mode := sessionFileMode
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm()
	}

	tmp, err := os.CreateTemp(s.dataDir, session.ID+tmpSuffixPattern)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// CreateTemp always makes the file 0600, so the mode has to be set
	// explicitly rather than left to the umask the way os.WriteFile did.
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
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
		if err := s.Save(&session); err != nil {
			return nil, err
		}
	}
	return &session, nil
}

// LoadAll loads all sessions.
//
// Files that fail Load (unparseable JSON, migration write-back failure, missing
// permissions, ...) are skipped instead of aborting so a single corrupt file
// doesn't strand every session. The individual failure is emitted via
// debugLog so it still surfaces under JIN_DEBUG=1.
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
