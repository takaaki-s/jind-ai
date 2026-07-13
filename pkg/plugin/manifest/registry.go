package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultRegistryURL is the canonical MVP registry endpoint. Callers can
// override via ClientConfig.URL for staging or a self-hosted mirror.
const DefaultRegistryURL = "https://takaaki-s.github.io/jind-ai-plugin-registry/registry.json"

// DefaultCacheTTL is how long a cached registry.json is considered fresh
// before the client falls back to a conditional GET.
const DefaultCacheTTL = 24 * time.Hour

// cacheDocFilename and cacheMetaFilename are the sibling files the client
// maintains inside ClientConfig.CacheDir.
const (
	cacheDocFilename  = "registry.json"
	cacheMetaFilename = "registry.etag"
)

// RegistryDocument is the deserialised registry.json served by the crawler.
// Consumers include jin's plugin ls-remote/install commands and any external
// tooling that inspects the registry without going through jin.
type RegistryDocument struct {
	SchemaVersion int             `json:"schema_version"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Plugins       []RegistryEntry `json:"plugins"`
}

// RegistryEntry is one plugin row in registry.json. Field semantics live in
// docs/plugin-registry.md; the crawler's contract is that `first_claimed_at`
// is immutable per name and `versions` is capped at the newest three.
type RegistryEntry struct {
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	Repo           string            `json:"repo"`
	Homepage       string            `json:"homepage,omitempty"`
	License        string            `json:"license,omitempty"`
	JinCompat      string            `json:"jin_compat"`
	LatestVersion  string            `json:"latest_version"`
	Versions       []RegistryVersion `json:"versions"`
	FirstClaimedAt time.Time         `json:"first_claimed_at"`
	OrphanedSince  *time.Time        `json:"orphaned_since"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// RegistryVersion pins one version of a plugin to a specific commit. `SHA`
// is the decisive install key — tag-repointing attacks are defeated by
// checking out that SHA rather than the tag.
type RegistryVersion struct {
	Version     string `json:"version"`
	SHA         string `json:"sha"`
	ManifestURL string `json:"manifest_url"`
	Tag         string `json:"tag"`
}

// Find returns the entry matching name, or nil if the name is unregistered.
// Callers that need to satisfy RegistryLookup wrap this in a small adapter
// at the call site — the projection to (repo, latestVersion) is a
// validation-side concern, not a shared data-type concern.
func (d *RegistryDocument) Find(name string) *RegistryEntry {
	if d == nil {
		return nil
	}
	for i := range d.Plugins {
		if d.Plugins[i].Name == name {
			return &d.Plugins[i]
		}
	}
	return nil
}

// ClientConfig configures Client. Zero values apply sensible defaults except
// for CacheDir, which is required so we never accidentally write into the
// caller's CWD.
type ClientConfig struct {
	// URL is the registry endpoint. Empty means DefaultRegistryURL.
	URL string
	// CacheDir is where registry.json and its metadata are cached. jin CLI
	// wires this to paths.State(); tests pass t.TempDir(). Required.
	CacheDir string
	// TTL controls freshness. Zero means DefaultCacheTTL.
	TTL time.Duration
	// HTTPClient overrides the default (30s timeout). Tests inject
	// httptest.Server.Client() so redirects, timeouts, and TLS trust match.
	HTTPClient *http.Client
	// Now returns the current time. Tests inject a fixed clock so cache
	// freshness is deterministic.
	Now func() time.Time
}

// Client fetches and caches the registry document.
type Client struct {
	url        string
	cacheDir   string
	ttl        time.Duration
	httpClient *http.Client
	now        func() time.Time
}

// NewClient validates cfg and returns a ready Client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.CacheDir == "" {
		return nil, errors.New("registry client: CacheDir is required")
	}
	c := &Client{
		url:        cfg.URL,
		cacheDir:   cfg.CacheDir,
		ttl:        cfg.TTL,
		httpClient: cfg.HTTPClient,
		now:        cfg.Now,
	}
	if c.url == "" {
		c.url = DefaultRegistryURL
	}
	if c.ttl == 0 {
		c.ttl = DefaultCacheTTL
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// LoadOptions tunes a single Load call.
type LoadOptions struct {
	// Refresh bypasses the freshness check but still sends conditional
	// headers, so a `jin plugin ls-remote --refresh` against an unchanged
	// registry is a cheap 304.
	Refresh bool
}

// LoadOutcome tags what happened during Load so the CLI can render an
// appropriate hint and tests can assert the taken branch.
type LoadOutcome int

const (
	// OutcomeCacheHit means the on-disk cache was still fresh; no HTTP
	// request was made.
	OutcomeCacheHit LoadOutcome = iota
	// OutcomeNotModified means the server returned 304; the cache body was
	// reused and its fetched_at timestamp was refreshed.
	OutcomeNotModified
	// OutcomeFetched means the server returned 200 and the cache was
	// overwritten with the fresh body.
	OutcomeFetched
	// OutcomeCacheFallback means the network fetch failed but a cached
	// document (possibly beyond TTL) was returned instead. Callers should
	// warn the user by consulting Result.FetchErr.
	OutcomeCacheFallback
)

// LoadResult carries the outcome and — for OutcomeCacheFallback — the
// underlying network error so callers can log it verbatim.
type LoadResult struct {
	Outcome  LoadOutcome
	FetchErr error
}

// Load returns the registry document. It reads from cache when fresh,
// issues a conditional GET when the cache is stale, and falls back to
// cache (even expired) when the network is unreachable.
func (c *Client) Load(ctx context.Context, opts LoadOptions) (*RegistryDocument, LoadResult, error) {
	cached, meta, cacheErr := c.readCache()

	stale := meta.FetchedAt.IsZero() || c.now().Sub(meta.FetchedAt) > c.ttl
	if !opts.Refresh && cacheErr == nil && !stale {
		return cached, LoadResult{Outcome: OutcomeCacheHit}, nil
	}

	body, etag, lastMod, status, fetchErr := c.fetch(ctx, meta)

	if fetchErr != nil {
		if cacheErr == nil {
			return cached, LoadResult{Outcome: OutcomeCacheFallback, FetchErr: fetchErr}, nil
		}
		return nil, LoadResult{}, fmt.Errorf("registry fetch failed and no cache available: %w", fetchErr)
	}

	if status == http.StatusNotModified {
		if cacheErr != nil {
			return nil, LoadResult{}, errors.New("registry returned 304 but local cache is missing or unreadable")
		}
		refreshed := meta
		refreshed.FetchedAt = c.now()
		_ = c.writeMetadata(refreshed)
		return cached, LoadResult{Outcome: OutcomeNotModified}, nil
	}

	doc, err := parseRegistry(body)
	if err != nil {
		return nil, LoadResult{}, fmt.Errorf("parse registry: %w", err)
	}
	_ = c.writeCache(body, cacheMetadata{ETag: etag, LastModified: lastMod, FetchedAt: c.now()})
	return doc, LoadResult{Outcome: OutcomeFetched}, nil
}

// cacheMetadata records the last conditional-GET validators plus a wall
// clock stamp used for TTL checks. Persisted as JSON alongside the doc.
type cacheMetadata struct {
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	FetchedAt    time.Time `json:"fetched_at"`
}

func (c *Client) readCache() (*RegistryDocument, cacheMetadata, error) {
	body, err := os.ReadFile(filepath.Join(c.cacheDir, cacheDocFilename))
	if err != nil {
		return nil, cacheMetadata{}, err
	}
	doc, err := parseRegistry(body)
	if err != nil {
		return nil, cacheMetadata{}, err
	}
	var meta cacheMetadata
	if metaBytes, mErr := os.ReadFile(filepath.Join(c.cacheDir, cacheMetaFilename)); mErr == nil {
		_ = json.Unmarshal(metaBytes, &meta)
	}
	return doc, meta, nil
}

// writeMetadata ensures the cache dir exists and persists meta. Also the
// prerequisite for writeCache — the doc write assumes the dir is already
// ensured, so writeCache calls this first.
func (c *Client) writeMetadata(meta cacheMetadata) error {
	if err := os.MkdirAll(c.cacheDir, 0o755); err != nil {
		return err
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(c.cacheDir, cacheMetaFilename), metaBytes, 0o644)
}

func (c *Client) writeCache(body []byte, meta cacheMetadata) error {
	if err := c.writeMetadata(meta); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(c.cacheDir, cacheDocFilename), body, 0o644)
}

func (c *Client) fetch(ctx context.Context, meta cacheMetadata) (body []byte, etag, lastMod string, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, "", "", 0, err
	}
	if meta.ETag != "" {
		req.Header.Set("If-None-Match", meta.ETag)
	}
	if meta.LastModified != "" {
		req.Header.Set("If-Modified-Since", meta.LastModified)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "jind-ai/plugin-registry-client")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", "", 0, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		b, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, "", "", 0, readErr
		}
		return b, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), resp.StatusCode, nil
	case http.StatusNotModified:
		return nil, meta.ETag, meta.LastModified, resp.StatusCode, nil
	default:
		return nil, "", "", resp.StatusCode, fmt.Errorf("registry HTTP %d: %s", resp.StatusCode, strings.TrimSpace(resp.Status))
	}
}

func parseRegistry(body []byte) (*RegistryDocument, error) {
	var doc RegistryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	if doc.SchemaVersion == 0 {
		return nil, errors.New("registry: missing schema_version")
	}
	if doc.SchemaVersion != CurrentSchemaVersion {
		return nil, fmt.Errorf("registry: schema_version %d not supported (this build understands %d)", doc.SchemaVersion, CurrentSchemaVersion)
	}
	return &doc, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}
