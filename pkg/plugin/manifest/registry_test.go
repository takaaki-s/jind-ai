package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sampleRegistryJSON returns a minimal valid registry payload with one entry.
// Tests mutate the returned bytes for "server sends a different body" cases.
func sampleRegistryJSON(t *testing.T, name, latest string) []byte {
	t.Helper()
	doc := RegistryDocument{
		SchemaVersion: CurrentSchemaVersion,
		GeneratedAt:   time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC),
		Plugins: []RegistryEntry{
			{
				Name:           name,
				Description:    "test plugin",
				Repo:           "example/" + name,
				License:        "MIT",
				JinCompat:      ">=0.7.0",
				LatestVersion:  latest,
				Versions:       []RegistryVersion{{Version: latest, SHA: "deadbeefcafebabefeedfacedeadbeefcafebabe", ManifestURL: "https://example.com/" + name + "/manifest", Tag: "v" + latest}},
				FirstClaimedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
				UpdatedAt:      time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal sample: %v", err)
	}
	return b
}

// fixedClock returns a deterministic time source for tests.
func fixedClock(base time.Time) func() time.Time {
	return func() time.Time { return base }
}

// preloadCache writes a doc body and metadata into cacheDir so tests can
// simulate an existing (fresh or expired) cache.
func preloadCache(t *testing.T, cacheDir string, body []byte, meta cacheMetadata) {
	t.Helper()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, cacheDocFilename), body, 0o644); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, cacheMetaFilename), metaBytes, 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}
}

func readMeta(t *testing.T, cacheDir string) cacheMetadata {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cacheDir, cacheMetaFilename))
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var m cacheMetadata
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	return m
}

func TestClient_Load_FetchesWhenNoCache(t *testing.T) {
	body := sampleRegistryJSON(t, "jind-ai-notifier", "0.3.1")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Last-Modified", "Mon, 13 Jul 2026 00:00:00 GMT")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	c, err := NewClient(ClientConfig{URL: srv.URL, CacheDir: dir, HTTPClient: srv.Client(), Now: fixedClock(time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	doc, res, err := c.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Outcome != OutcomeFetched {
		t.Fatalf("outcome: got %d want %d (Fetched)", res.Outcome, OutcomeFetched)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits: got %d want 1", got)
	}
	if doc.Find("jind-ai-notifier") == nil {
		t.Fatalf("entry not found in fetched doc")
	}

	meta := readMeta(t, dir)
	if meta.ETag != `"abc"` {
		t.Fatalf("etag: got %q want %q", meta.ETag, `"abc"`)
	}
	if meta.FetchedAt.IsZero() {
		t.Fatalf("fetched_at not set")
	}
}

func TestClient_Load_CacheHitWithinTTL(t *testing.T) {
	body := sampleRegistryJSON(t, "jind-ai-notifier", "0.3.1")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	preloadCache(t, dir, body, cacheMetadata{ETag: `"abc"`, FetchedAt: now.Add(-1 * time.Hour)})

	c, err := NewClient(ClientConfig{URL: srv.URL, CacheDir: dir, HTTPClient: srv.Client(), Now: fixedClock(now)})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	doc, res, err := c.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Outcome != OutcomeCacheHit {
		t.Fatalf("outcome: got %d want %d (CacheHit)", res.Outcome, OutcomeCacheHit)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("hits: got %d want 0 (no HTTP request expected)", got)
	}
	if doc.Find("jind-ai-notifier") == nil {
		t.Fatalf("entry not found in cached doc")
	}
}

func TestClient_Load_Conditional304RefreshesFetchedAt(t *testing.T) {
	body := sampleRegistryJSON(t, "jind-ai-notifier", "0.3.1")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("If-None-Match") != `"abc"` {
			t.Errorf("If-None-Match: got %q want %q", r.Header.Get("If-None-Match"), `"abc"`)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	preloadCache(t, dir, body, cacheMetadata{ETag: `"abc"`, FetchedAt: now.Add(-25 * time.Hour)})

	c, err := NewClient(ClientConfig{URL: srv.URL, CacheDir: dir, HTTPClient: srv.Client(), Now: fixedClock(now)})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	doc, res, err := c.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Outcome != OutcomeNotModified {
		t.Fatalf("outcome: got %d want %d (NotModified)", res.Outcome, OutcomeNotModified)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits: got %d want 1", got)
	}
	if doc.Find("jind-ai-notifier") == nil {
		t.Fatalf("entry not found in cached doc after 304")
	}
	meta := readMeta(t, dir)
	if !meta.FetchedAt.Equal(now) {
		t.Fatalf("fetched_at not refreshed: got %v want %v", meta.FetchedAt, now)
	}
	if meta.ETag != `"abc"` {
		t.Fatalf("etag lost across 304: got %q want %q", meta.ETag, `"abc"`)
	}
}

func TestClient_Load_ConditionalGet200Updates(t *testing.T) {
	oldBody := sampleRegistryJSON(t, "jind-ai-notifier", "0.3.1")
	newBody := sampleRegistryJSON(t, "jind-ai-notifier", "0.4.0")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("If-None-Match") != `"old"` {
			t.Errorf("If-None-Match: got %q want %q", r.Header.Get("If-None-Match"), `"old"`)
		}
		w.Header().Set("ETag", `"new"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(newBody)
	}))
	defer srv.Close()

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	preloadCache(t, dir, oldBody, cacheMetadata{ETag: `"old"`, FetchedAt: now.Add(-25 * time.Hour)})

	c, err := NewClient(ClientConfig{URL: srv.URL, CacheDir: dir, HTTPClient: srv.Client(), Now: fixedClock(now)})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	doc, res, err := c.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Outcome != OutcomeFetched {
		t.Fatalf("outcome: got %d want %d (Fetched)", res.Outcome, OutcomeFetched)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits: got %d want 1", got)
	}
	entry := doc.Find("jind-ai-notifier")
	if entry == nil {
		t.Fatalf("entry missing")
	}
	if entry.LatestVersion != "0.4.0" {
		t.Fatalf("latest_version: got %q want 0.4.0", entry.LatestVersion)
	}
	meta := readMeta(t, dir)
	if meta.ETag != `"new"` {
		t.Fatalf("etag not updated: got %q want %q", meta.ETag, `"new"`)
	}
}

func TestClient_Load_FetchFailsFallsBackToCache(t *testing.T) {
	body := sampleRegistryJSON(t, "jind-ai-notifier", "0.3.1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	preloadCache(t, dir, body, cacheMetadata{ETag: `"abc"`, FetchedAt: now.Add(-25 * time.Hour)})

	c, err := NewClient(ClientConfig{URL: srv.URL, CacheDir: dir, HTTPClient: srv.Client(), Now: fixedClock(now)})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	doc, res, err := c.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Outcome != OutcomeCacheFallback {
		t.Fatalf("outcome: got %d want %d (CacheFallback)", res.Outcome, OutcomeCacheFallback)
	}
	if res.FetchErr == nil {
		t.Fatalf("FetchErr should carry the underlying network error")
	}
	if doc.Find("jind-ai-notifier") == nil {
		t.Fatalf("entry not found in fallback doc")
	}
}

func TestClient_Load_FetchFailsWithoutCacheReturnsError(t *testing.T) {
	c, err := NewClient(ClientConfig{
		URL:        "http://127.0.0.1:1", // deterministic connection refused
		CacheDir:   t.TempDir(),
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
		Now:        fixedClock(time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, _, err = c.Load(context.Background(), LoadOptions{})
	if err == nil {
		t.Fatalf("expected error when fetch fails and cache is absent")
	}
	if !strings.Contains(err.Error(), "no cache available") {
		t.Fatalf("error should mention missing cache, got: %v", err)
	}
}

func TestClient_Load_RefreshBypassesFreshCache(t *testing.T) {
	body := sampleRegistryJSON(t, "jind-ai-notifier", "0.3.1")
	newBody := sampleRegistryJSON(t, "jind-ai-notifier", "0.4.0")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("ETag", `"new"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(newBody)
	}))
	defer srv.Close()

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	preloadCache(t, dir, body, cacheMetadata{ETag: `"old"`, FetchedAt: now.Add(-1 * time.Hour)})

	c, err := NewClient(ClientConfig{URL: srv.URL, CacheDir: dir, HTTPClient: srv.Client(), Now: fixedClock(now)})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	doc, res, err := c.Load(context.Background(), LoadOptions{Refresh: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Outcome != OutcomeFetched {
		t.Fatalf("outcome: got %d want %d (Fetched)", res.Outcome, OutcomeFetched)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits: got %d want 1", got)
	}
	if entry := doc.Find("jind-ai-notifier"); entry == nil || entry.LatestVersion != "0.4.0" {
		t.Fatalf("expected refreshed doc with 0.4.0")
	}
}

func TestNewClient_RequiresCacheDir(t *testing.T) {
	_, err := NewClient(ClientConfig{URL: "http://example.com"})
	if err == nil {
		t.Fatalf("expected error when CacheDir is empty")
	}
}

func TestRegistryDocument_Find(t *testing.T) {
	doc := &RegistryDocument{
		SchemaVersion: CurrentSchemaVersion,
		Plugins: []RegistryEntry{
			{Name: "foo", Repo: "acme/foo", LatestVersion: "1.2.3"},
		},
	}
	if entry := doc.Find("foo"); entry == nil || entry.Repo != "acme/foo" || entry.LatestVersion != "1.2.3" {
		t.Fatalf("find foo: got %+v want repo=acme/foo latest=1.2.3", entry)
	}
	if entry := doc.Find("missing"); entry != nil {
		t.Fatalf("find missing should return nil, got %+v", entry)
	}
	if entry := (*RegistryDocument)(nil).Find("foo"); entry != nil {
		t.Fatalf("find on nil doc should return nil, got %+v", entry)
	}
}

func TestClient_Load_RejectsUnsupportedSchema(t *testing.T) {
	body := []byte(`{"schema_version": 99, "plugins": []}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c, err := NewClient(ClientConfig{URL: srv.URL, CacheDir: t.TempDir(), HTTPClient: srv.Client(), Now: fixedClock(time.Now())})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, _, err = c.Load(context.Background(), LoadOptions{})
	if err == nil {
		t.Fatalf("expected schema mismatch error")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("error should mention schema_version, got: %v", err)
	}
}

// TestClient_Load_ShareCacheAcrossInstances documents the design invariant
// that ls-remote and install must see the same cache slice: two clients
// pointed at the same CacheDir observe each other's writes without going
// back to the network.
func TestClient_Load_ShareCacheAcrossInstances(t *testing.T) {
	body := sampleRegistryJSON(t, "jind-ai-notifier", "0.3.1")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()

	first, err := NewClient(ClientConfig{URL: srv.URL, CacheDir: dir, HTTPClient: srv.Client(), Now: fixedClock(now)})
	if err != nil {
		t.Fatalf("NewClient first: %v", err)
	}
	if _, _, err := first.Load(context.Background(), LoadOptions{}); err != nil {
		t.Fatalf("first Load: %v", err)
	}

	second, err := NewClient(ClientConfig{URL: srv.URL, CacheDir: dir, HTTPClient: srv.Client(), Now: fixedClock(now)})
	if err != nil {
		t.Fatalf("NewClient second: %v", err)
	}
	_, res, err := second.Load(context.Background(), LoadOptions{})
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if res.Outcome != OutcomeCacheHit {
		t.Fatalf("second Load outcome: got %d want %d (CacheHit)", res.Outcome, OutcomeCacheHit)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits: got %d want 1 (second Load must not touch network)", got)
	}
}

// Ensure the client honors context cancellation.
func TestClient_Load_HonorsContext(t *testing.T) {
	// Server never responds during the test body. Defer close(blocker) BEFORE
	// srv.Close() so LIFO order unblocks the handler goroutine first — else
	// srv.Close() would deadlock waiting for the handler to return.
	blocker := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocker
	}))
	defer srv.Close()
	defer close(blocker)

	c, err := NewClient(ClientConfig{URL: srv.URL, CacheDir: t.TempDir(), HTTPClient: srv.Client(), Now: fixedClock(time.Now())})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err = c.Load(ctx, LoadOptions{})
	if err == nil {
		t.Fatalf("expected error from canceled context")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context") {
		t.Fatalf("expected context error, got: %v", err)
	}
}

