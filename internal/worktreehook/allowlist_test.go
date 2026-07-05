package worktreehook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestAllowlist_LoadEmpty(t *testing.T) {
	dir := t.TempDir()

	a, err := LoadAllowlist(dir)
	if err != nil {
		t.Fatalf("LoadAllowlist on empty dir: %v", err)
	}
	if got := a.All(); len(got) != 0 {
		t.Errorf("expected empty allowlist, got %v", got)
	}

	if _, err := os.Stat(filepath.Join(dir, allowlistFilename)); !os.IsNotExist(err) {
		t.Errorf("LoadAllowlist should not create the file, stat err = %v", err)
	}
}

func TestAllowlist_AllowAndGet(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadAllowlist(dir)
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}

	repo := t.TempDir()
	if err := a.Allow(repo, "sha-abc"); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	entry, ok := a.Get(repo)
	if !ok {
		t.Fatalf("Get after Allow returned ok=false")
	}
	if entry.SHA256 != "sha-abc" {
		t.Errorf("SHA256 = %q, want %q", entry.SHA256, "sha-abc")
	}
	if entry.AllowedAt.IsZero() {
		t.Error("AllowedAt should be non-zero")
	}
}

func TestAllowlist_Revoke(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadAllowlist(dir)
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}

	repo := t.TempDir()
	if err := a.Allow(repo, "sha-abc"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if err := a.Revoke(repo); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok := a.Get(repo); ok {
		t.Error("Get after Revoke should return ok=false")
	}

	// Revoking a missing entry should be a no-op, not an error.
	if err := a.Revoke(repo); err != nil {
		t.Errorf("Revoke on missing entry returned err: %v", err)
	}
}

func TestAllowlist_Persistence(t *testing.T) {
	dir := t.TempDir()
	repo := t.TempDir()

	first, err := LoadAllowlist(dir)
	if err != nil {
		t.Fatalf("LoadAllowlist#1: %v", err)
	}
	if err := first.Allow(repo, "sha-persist"); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	second, err := LoadAllowlist(dir)
	if err != nil {
		t.Fatalf("LoadAllowlist#2: %v", err)
	}
	entry, ok := second.Get(repo)
	if !ok {
		t.Fatalf("second load did not see persisted entry")
	}
	if entry.SHA256 != "sha-persist" {
		t.Errorf("SHA256 = %q, want %q", entry.SHA256, "sha-persist")
	}

	// The on-disk file should be valid JSON with the absolute repo path as key.
	b, err := os.ReadFile(filepath.Join(dir, allowlistFilename))
	if err != nil {
		t.Fatalf("read allowlist file: %v", err)
	}
	var raw map[string]AllowEntry
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal on-disk allowlist: %v", err)
	}
	absRepo, _ := filepath.Abs(repo)
	if _, ok := raw[absRepo]; !ok {
		t.Errorf("on-disk allowlist missing key %q, got keys %v", absRepo, keysOf(raw))
	}
}

// TestAllowlist_ReadsExternalWrites simulates the daemon+CLI split: the
// daemon holds a long-lived *Allowlist, while `jin worktree allow` writes to
// the same JSON file from a separate process. Get must observe the external
// write on the next call, not the map cached at Load time.
func TestAllowlist_ReadsExternalWrites(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadAllowlist(dir)
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}

	repo := t.TempDir()
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		t.Fatalf("abs repo: %v", err)
	}

	if _, ok := a.Get(repo); ok {
		t.Fatalf("Get before external write should return ok=false")
	}

	// Simulate a CLI process writing the allowlist directly.
	external := map[string]AllowEntry{
		absRepo: {SHA256: "sha-external", AllowedAt: time.Now().UTC()},
	}
	raw, err := json.MarshalIndent(external, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, allowlistFilename), raw, 0o644); err != nil {
		t.Fatalf("write external allowlist: %v", err)
	}

	entry, ok := a.Get(repo)
	if !ok {
		t.Fatalf("Get after external write returned ok=false; want to observe the new entry")
	}
	if entry.SHA256 != "sha-external" {
		t.Errorf("SHA256 = %q, want %q", entry.SHA256, "sha-external")
	}
}

func TestAllowlist_ConcurrentAllow(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadAllowlist(dir)
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			repo := filepath.Join(dir, "repo", "r"+strconv.Itoa(i))
			if err := os.MkdirAll(repo, 0o755); err != nil {
				t.Errorf("mkdir: %v", err)
				return
			}
			if err := a.Allow(repo, "sha-"+strconv.Itoa(i)); err != nil {
				t.Errorf("Allow: %v", err)
			}
		}()
	}
	wg.Wait()

	// After all concurrent writers finish, both the in-memory map and the
	// on-disk file must contain every entry.
	if got := len(a.All()); got != workers {
		t.Errorf("in-memory entries = %d, want %d", got, workers)
	}

	reloaded, err := LoadAllowlist(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(reloaded.All()); got != workers {
		t.Errorf("on-disk entries = %d, want %d", got, workers)
	}
}

func TestComputeSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.sh")
	content := []byte("echo hi\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ComputeSHA256(path)
	if err != nil {
		t.Fatalf("ComputeSHA256: %v", err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Errorf("ComputeSHA256 = %q, want %q", got, want)
	}

	if _, err := ComputeSHA256(filepath.Join(dir, "missing.sh")); err == nil {
		t.Error("ComputeSHA256 on missing file should error")
	}
}

func keysOf(m map[string]AllowEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
