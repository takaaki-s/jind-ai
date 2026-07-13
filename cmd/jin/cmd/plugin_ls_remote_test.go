package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// runLsRemoteCmd invokes `plugin ls-remote` with captured I/O. Flags are reset
// each call because cobra retains their last-set value between Executes and
// leaking, say, --sort=updated into a later test would silently change the
// asserted order.
func runLsRemoteCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	_ = pluginLsRemoteCmd.Flags().Set("registry", "")
	_ = pluginLsRemoteCmd.Flags().Set("sort", "name")
	_ = pluginLsRemoteCmd.Flags().Set("search", "")
	_ = pluginLsRemoteCmd.Flags().Set("refresh", "false")
	jsonOutput = false

	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetIn(strings.NewReader(""))
	rootCmd.SetArgs(append([]string{"plugin", "ls-remote"}, args...))
	err := rootCmd.Execute()
	return stdout.String(), stderr.String(), err
}

// twoEntryRegistryJSON returns a two-entry registry payload with distinct
// names, descriptions, and updated_at timestamps — enough to exercise sort
// and search.
func twoEntryRegistryJSON(t *testing.T) []byte {
	t.Helper()
	doc := manifest.RegistryDocument{
		SchemaVersion: manifest.CurrentSchemaVersion,
		GeneratedAt:   time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		Plugins: []manifest.RegistryEntry{
			{
				Name:          "jind-ai-notifier",
				Description:   "Desktop notifier plugin",
				Repo:          "foo/jind-ai-notifier",
				LatestVersion: "0.3.1",
				Versions:      []manifest.RegistryVersion{{Version: "0.3.1", SHA: "abc123"}},
				UpdatedAt:     time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
			},
			{
				Name:          "another-plugin",
				Description:   "Something else entirely",
				Repo:          "bar/another-plugin",
				LatestVersion: "1.0.0",
				Versions:      []manifest.RegistryVersion{{Version: "1.0.0", SHA: "def456"}},
				UpdatedAt:     time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}
	return b
}

// registryTestServer serves the given body as registry.json. Callers use the
// returned URL as --registry.
func registryTestServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// setupIsolatedState points XDG_STATE_HOME at a temp dir so the registry
// cache does not leak between tests (or into the developer's real cache).
func setupIsolatedState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func TestPluginLsRemoteDefaultSortsByName(t *testing.T) {
	setupIsolatedState(t)
	srv := registryTestServer(t, twoEntryRegistryJSON(t))

	stdout, _, err := runLsRemoteCmd(t, "--registry", srv.URL)
	if err != nil {
		t.Fatalf("ls-remote: err=%v, out=%q", err, stdout)
	}
	// another-plugin comes before jind-ai-notifier alphabetically.
	a := strings.Index(stdout, "another-plugin")
	b := strings.Index(stdout, "jind-ai-notifier")
	if a == -1 || b == -1 {
		t.Fatalf("expected both entries in output, got %q", stdout)
	}
	if a > b {
		t.Errorf("expected alphabetical order (another before jind), got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "NAME") || !strings.Contains(stdout, "LATEST") || !strings.Contains(stdout, "REPO") {
		t.Errorf("expected header columns in output, got %q", stdout)
	}
}

func TestPluginLsRemoteSortByUpdatedIsDesc(t *testing.T) {
	setupIsolatedState(t)
	srv := registryTestServer(t, twoEntryRegistryJSON(t))

	stdout, _, err := runLsRemoteCmd(t, "--registry", srv.URL, "--sort", "updated")
	if err != nil {
		t.Fatalf("ls-remote: err=%v, out=%q", err, stdout)
	}
	// jind-ai-notifier is updated 2026-07-10, another-plugin 2026-07-08 —
	// desc sort puts jind first.
	j := strings.Index(stdout, "jind-ai-notifier")
	a := strings.Index(stdout, "another-plugin")
	if j == -1 || a == -1 {
		t.Fatalf("expected both entries, got %q", stdout)
	}
	if j > a {
		t.Errorf("expected updated-desc order (jind before another), got:\n%s", stdout)
	}
}

func TestPluginLsRemoteInvalidSortRejected(t *testing.T) {
	setupIsolatedState(t)
	srv := registryTestServer(t, twoEntryRegistryJSON(t))

	_, _, err := runLsRemoteCmd(t, "--registry", srv.URL, "--sort", "bogus")
	if err == nil {
		t.Fatalf("expected error for invalid --sort value")
	}
	if !strings.Contains(err.Error(), "invalid --sort") {
		t.Errorf("expected 'invalid --sort' in error, got %v", err)
	}
}

func TestPluginLsRemoteSearchMatchesName(t *testing.T) {
	setupIsolatedState(t)
	srv := registryTestServer(t, twoEntryRegistryJSON(t))

	stdout, _, err := runLsRemoteCmd(t, "--registry", srv.URL, "--search", "notif")
	if err != nil {
		t.Fatalf("ls-remote: err=%v, out=%q", err, stdout)
	}
	if !strings.Contains(stdout, "jind-ai-notifier") {
		t.Errorf("expected jind-ai-notifier in output, got %q", stdout)
	}
	if strings.Contains(stdout, "another-plugin") {
		t.Errorf("expected another-plugin filtered out, got %q", stdout)
	}
}

func TestPluginLsRemoteSearchMatchesDescription(t *testing.T) {
	setupIsolatedState(t)
	srv := registryTestServer(t, twoEntryRegistryJSON(t))

	// "entirely" appears only in another-plugin's description.
	stdout, _, err := runLsRemoteCmd(t, "--registry", srv.URL, "--search", "entirely")
	if err != nil {
		t.Fatalf("ls-remote: err=%v, out=%q", err, stdout)
	}
	if !strings.Contains(stdout, "another-plugin") {
		t.Errorf("expected another-plugin in output, got %q", stdout)
	}
	if strings.Contains(stdout, "jind-ai-notifier") {
		t.Errorf("expected jind-ai-notifier filtered out, got %q", stdout)
	}
}

func TestPluginLsRemoteSearchCaseInsensitive(t *testing.T) {
	setupIsolatedState(t)
	srv := registryTestServer(t, twoEntryRegistryJSON(t))

	stdout, _, err := runLsRemoteCmd(t, "--registry", srv.URL, "--search", "NOTIF")
	if err != nil {
		t.Fatalf("ls-remote: err=%v, out=%q", err, stdout)
	}
	if !strings.Contains(stdout, "jind-ai-notifier") {
		t.Errorf("expected case-insensitive match, got %q", stdout)
	}
}

func TestPluginLsRemoteSearchNoMatchesEmptyMessage(t *testing.T) {
	setupIsolatedState(t)
	srv := registryTestServer(t, twoEntryRegistryJSON(t))

	stdout, _, err := runLsRemoteCmd(t, "--registry", srv.URL, "--search", "nomatchxyz")
	if err != nil {
		t.Fatalf("ls-remote: err=%v, out=%q", err, stdout)
	}
	if !strings.Contains(stdout, "No plugins found") {
		t.Errorf("expected 'No plugins found' message, got %q", stdout)
	}
}

func TestPluginLsRemoteJSONOutputParses(t *testing.T) {
	setupIsolatedState(t)
	srv := registryTestServer(t, twoEntryRegistryJSON(t))

	stdout, _, err := runLsRemoteCmd(t, "--json", "--registry", srv.URL)
	if err != nil {
		t.Fatalf("ls-remote --json: err=%v, out=%q", err, stdout)
	}

	var entries []manifest.RegistryEntry
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// Default sort is by name, so another-plugin is first.
	if entries[0].Name != "another-plugin" {
		t.Errorf("expected first entry another-plugin, got %q", entries[0].Name)
	}
	if entries[0].Versions[0].SHA != "def456" {
		t.Errorf("expected SHA def456, got %q", entries[0].Versions[0].SHA)
	}
}

func TestPluginLsRemoteJSONEmptyIsArrayNotNull(t *testing.T) {
	setupIsolatedState(t)
	srv := registryTestServer(t, twoEntryRegistryJSON(t))

	stdout, _, err := runLsRemoteCmd(t, "--json", "--registry", srv.URL, "--search", "nomatchxyz")
	if err != nil {
		t.Fatalf("ls-remote --json --search: err=%v, out=%q", err, stdout)
	}
	// Empty result must serialise as `[]` so callers using `jq` don't hit
	// a null slice they have to guard.
	trimmed := strings.TrimSpace(stdout)
	if trimmed != "[]" {
		t.Errorf("expected empty JSON array, got %q", trimmed)
	}
}

func TestPluginLsRemoteFetchFailureWithCacheWarns(t *testing.T) {
	setupIsolatedState(t)

	// Round 1: prime the cache via a working server.
	body := twoEntryRegistryJSON(t)
	primeSrv := registryTestServer(t, body)
	if _, _, err := runLsRemoteCmd(t, "--registry", primeSrv.URL); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	primeSrv.Close()

	// Round 2: point at a server that always 500s. The cache from round 1
	// should serve the response and a warning should surface on stderr.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(failSrv.Close)

	stdout, stderr, err := runLsRemoteCmd(t, "--registry", failSrv.URL, "--refresh")
	if err != nil {
		t.Fatalf("cache fallback should succeed, err=%v, stdout=%q, stderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "jind-ai-notifier") {
		t.Errorf("expected cached entry in stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "registry fetch failed") {
		t.Errorf("expected cache-fallback warning on stderr, got %q", stderr)
	}
}
