package worktreehook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const allowlistFilename = "worktree-allowlist.json"

// AllowEntry is one row in the allowlist: the trusted SHA256 of the script
// contents and when the trust was granted.
type AllowEntry struct {
	SHA256    string    `json:"sha256"`
	AllowedAt time.Time `json:"allowed_at"`
}

// Allowlist is the in-memory view of the persisted worktree-allowlist.json.
// All mutations flush to disk under an exclusive flock so that concurrent
// daemon + CLI processes never corrupt the file.
type Allowlist struct {
	path    string
	mu      sync.Mutex
	entries map[string]AllowEntry
}

// LoadAllowlist reads worktree-allowlist.json from stateDir. A missing file
// is not an error — it returns an empty allowlist so first-run callers can
// use the returned pointer immediately.
func LoadAllowlist(stateDir string) (*Allowlist, error) {
	path := filepath.Join(stateDir, allowlistFilename)
	a := &Allowlist{path: path, entries: map[string]AllowEntry{}}

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return a, nil
		}
		return nil, fmt.Errorf("read allowlist: %w", err)
	}
	if len(b) == 0 {
		return a, nil
	}
	if err := json.Unmarshal(b, &a.entries); err != nil {
		return nil, fmt.Errorf("decode allowlist %s: %w", path, err)
	}
	return a, nil
}

// Get returns the entry for repoRoot (normalized via filepath.Abs) and
// whether it exists. Abs failures fall through to a false result so callers
// can treat them as "not allowed".
//
// The on-disk file is re-read under a shared flock on every call so that
// entries written by another process (e.g. `jin worktree allow` from the CLI
// while the daemon is running) are visible without restarting the daemon.
// Re-read failures fall back to the in-memory map so a transient I/O error
// cannot make previously-visible entries disappear.
func (a *Allowlist) Get(repoRoot string) (AllowEntry, bool) {
	key, err := filepath.Abs(repoRoot)
	if err != nil {
		return AllowEntry{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Reload errors are intentionally swallowed: if the file becomes
	// temporarily unreadable we prefer serving the last-known map over
	// treating every entry as revoked. Callers get a stale-but-safe answer.
	_ = a.reloadLocked()
	entry, ok := a.entries[key]
	return entry, ok
}

// reloadLocked refreshes a.entries from disk under a shared flock. Called by
// Get before every lookup. Missing file is treated as an empty allowlist
// (mirrors LoadAllowlist). Caller must hold a.mu.
func (a *Allowlist) reloadLocked() error {
	f, err := os.Open(a.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			a.entries = map[string]AllowEntry{}
			return nil
		}
		return fmt.Errorf("open allowlist: %w", err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return fmt.Errorf("flock allowlist (shared): %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	b, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read allowlist: %w", err)
	}
	fresh := map[string]AllowEntry{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &fresh); err != nil {
			return fmt.Errorf("decode allowlist: %w", err)
		}
	}
	a.entries = fresh
	return nil
}

// Allow inserts or overwrites the entry for repoRoot and persists the file.
// AllowedAt is set to time.Now().UTC() at call time.
func (a *Allowlist) Allow(repoRoot, sha string) error {
	key, err := filepath.Abs(repoRoot)
	if err != nil {
		return fmt.Errorf("abs repo root: %w", err)
	}
	return a.mutate(func(entries map[string]AllowEntry) {
		entries[key] = AllowEntry{SHA256: sha, AllowedAt: time.Now().UTC()}
	})
}

// Revoke removes the entry for repoRoot if present. Missing entries are a
// no-op (not an error) — callers just want the post-condition "no entry".
func (a *Allowlist) Revoke(repoRoot string) error {
	key, err := filepath.Abs(repoRoot)
	if err != nil {
		return fmt.Errorf("abs repo root: %w", err)
	}
	return a.mutate(func(entries map[string]AllowEntry) {
		delete(entries, key)
	})
}

// All returns a deep copy of the current entries so callers can iterate
// without holding the Allowlist mutex.
func (a *Allowlist) All() map[string]AllowEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]AllowEntry, len(a.entries))
	for k, v := range a.entries {
		out[k] = v
	}
	return out
}

// mutate serializes read-modify-write against both intra-process goroutines
// (a.mu) and other processes (flock on the JSON file). The apply callback
// runs on a *disk-fresh* map so writes from other processes since Load are
// preserved.
func (a *Allowlist) mutate(apply func(map[string]AllowEntry)) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return fmt.Errorf("mkdir allowlist dir: %w", err)
	}

	f, err := os.OpenFile(a.path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open allowlist: %w", err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock allowlist: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	fresh := map[string]AllowEntry{}
	b, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read allowlist: %w", err)
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &fresh); err != nil {
			return fmt.Errorf("decode allowlist: %w", err)
		}
	}

	apply(fresh)

	out, err := json.MarshalIndent(fresh, "", "  ")
	if err != nil {
		return fmt.Errorf("encode allowlist: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek allowlist: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate allowlist: %w", err)
	}
	if _, err := f.Write(out); err != nil {
		return fmt.Errorf("write allowlist: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync allowlist: %w", err)
	}

	a.entries = fresh
	return nil
}

// ComputeSHA256 hashes the file at path and returns the lowercase hex digest.
// Used both by the allow CLI (to record trust) and by Verify (to check for
// drift since trust was granted).
func ComputeSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
