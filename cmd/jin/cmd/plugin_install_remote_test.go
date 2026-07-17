package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/plugin"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// setupInstallEnv wires all XDG homes to fresh temp dirs so
// `jin plugin install` reads and writes nothing outside the test's control,
// and returns the state and data dirs the CLI will use. XDG_CONFIG_HOME is
// pointed at a scratch dir so the config manager's YAML file (used to
// resolve plugins.build_timeout) is isolated from the developer's real config.
func setupInstallEnv(t *testing.T) (stateDir, dataDir string) {
	t.Helper()
	stateDir = t.TempDir()
	dataDir = t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Pin jin's version so consent-screen compat checks are deterministic
	// regardless of ldflags at test time. SetJinVersion returns the value it
	// replaced, so we capture that once at setup and restore it at cleanup.
	prev := plugin.SetJinVersion("0.7.0")
	t.Cleanup(func() { plugin.SetJinVersion(prev) })
	return stateDir, dataDir
}

// runInstallCmd invokes `jin plugin install <args…>` with captured I/O.
// Every install flag is reset each call so a leftover --yes or --pin from a
// previous test doesn't leak into the next one.
func runInstallCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	_ = pluginInstallCmd.Flags().Set("link", "")
	_ = pluginInstallCmd.Flags().Set("yes", "false")
	_ = pluginInstallCmd.Flags().Set("pin", "")
	_ = pluginInstallCmd.Flags().Set("force", "false")
	_ = pluginInstallCmd.Flags().Set("refresh", "false")
	_ = pluginInstallCmd.Flags().Set("registry", "")

	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetIn(strings.NewReader(""))
	rootCmd.SetArgs(append([]string{"plugin", "install"}, args...))
	err := rootCmd.Execute()
	return stdout.String(), stderr.String(), err
}

// initGitRepoWithManifest builds a local git repo with body as the plugin
// manifest and returns its filesystem path plus the HEAD SHA. The registry
// fixture uses file:// + this path as the entry's Repo, so the install path
// clones from the local disk without needing a real network.
func initGitRepoWithManifest(t *testing.T, body string) (path, sha string) {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, manifest.Filename), []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "initial")

	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return dir, strings.TrimSpace(string(out))
}

// registryDoc builds a one-plugin registry document whose only entry points
// at the given file:// repo and SHA. The plugin name in the manifest and in
// the registry must match — the crawler enforces this by construction, so
// the fixture is a tighter mirror of the real system.
func registryDoc(t *testing.T, name, repoPath, sha, jinCompat string) []byte {
	t.Helper()
	doc := manifest.RegistryDocument{
		SchemaVersion: manifest.CurrentRegistrySchemaVersion,
		Plugins: []manifest.RegistryEntry{
			{
				Name:          name,
				Description:   "notifier fixture",
				Repo:          "file://" + repoPath,
				JinCompat:     jinCompat,
				LatestVersion: "0.1.0",
				Versions: []manifest.RegistryVersion{
					{Version: "0.1.0", SHA: sha},
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}
	return b
}

const remoteInstallManifest = `schema_version: 1
name: jind-ai-notifier
version: 0.1.0
description: fixture
jin: ">=0.7.0"
install:
  source:
    build:
      - "true"
    entrypoint: ./notify.sh
on:
  - status_changed
`

func TestPluginInstallByNameSucceedsWithYes(t *testing.T) {
	_, dataDir := setupInstallEnv(t)
	repo, sha := initGitRepoWithManifest(t, remoteInstallManifest)
	srv := registryTestServer(t, registryDoc(t, "jind-ai-notifier", repo, sha, ">=0.7.0"))

	stdout, stderr, err := runInstallCmd(t,
		"jind-ai-notifier", "--registry", srv.URL, "--yes", "--refresh")
	if err != nil {
		t.Fatalf("install: err=%v\nstdout=%q\nstderr=%q", err, stdout, stderr)
	}

	// Consent screen — the four UX principles, each mapped to a substring:
	//   1) name/version + repo@short-sha + install path all in one block
	//   2) exec'd shell command is visible ("run: true")
	//   3) unverified marker is always present
	//   4) compat verdict is shown (✓ path here)
	for _, want := range []string{
		"Plugin:  jind-ai-notifier @ 0.1.0",
		"Source:  file://" + repo + "@" + shortSHA(sha),
		"Kind:    (unverified community plugin)",
		"Compat:  jin >=0.7.0",
		"✓",
		"Installation will:",
		"clone file://" + repo,
		"run: true",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("consent screen missing %q, full stdout:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "Installed jind-ai-notifier @ "+shortSHA(sha)) {
		t.Errorf("install summary missing, stdout:\n%s", stdout)
	}

	dest := filepath.Join(dataDir, "jind-ai", "plugins", "jind-ai-notifier")
	if _, err := os.Stat(filepath.Join(dest, manifest.Filename)); err != nil {
		t.Errorf("plugin not placed at %s: %v", dest, err)
	}
}

func TestPluginInstallByNameHonorsPin(t *testing.T) {
	_, _ = setupInstallEnv(t)
	repo, sha := initGitRepoWithManifest(t, remoteInstallManifest)

	// Two versions in the doc; pin the older one to prove --pin is used.
	doc := manifest.RegistryDocument{
		SchemaVersion: manifest.CurrentRegistrySchemaVersion,
		Plugins: []manifest.RegistryEntry{
			{
				Name:          "jind-ai-notifier",
				Repo:          "file://" + repo,
				JinCompat:     ">=0.7.0",
				LatestVersion: "0.2.0",
				Versions: []manifest.RegistryVersion{
					{Version: "0.1.0", SHA: sha},
					{Version: "0.2.0", SHA: "0000000000000000"},
				},
			},
		},
	}
	body, _ := json.Marshal(doc)
	srv := registryTestServer(t, body)

	stdout, stderr, err := runInstallCmd(t,
		"jind-ai-notifier", "--registry", srv.URL, "--yes", "--refresh",
		"--pin", "0.1.0")
	if err != nil {
		t.Fatalf("install: err=%v\nstdout=%q\nstderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "Plugin:  jind-ai-notifier @ 0.1.0") {
		t.Errorf("expected the pinned version in consent screen, got:\n%s", stdout)
	}
}

func TestPluginInstallByNameCompatMismatchAbortsWithoutForce(t *testing.T) {
	_, dataDir := setupInstallEnv(t)
	// Manifest requires jin >=99.0.0; the pinned test binary is 0.7.0.
	incompatibleManifest := strings.Replace(remoteInstallManifest,
		`jin: ">=0.7.0"`, `jin: ">=99.0.0"`, 1)
	repo, sha := initGitRepoWithManifest(t, incompatibleManifest)
	srv := registryTestServer(t, registryDoc(t, "jind-ai-notifier", repo, sha, ">=99.0.0"))

	stdout, _, err := runInstallCmd(t,
		"jind-ai-notifier", "--registry", srv.URL, "--yes", "--refresh")
	if err == nil {
		t.Fatalf("expected compat mismatch to abort, stdout:\n%s", stdout)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should hint at --force, got: %v", err)
	}
	// Consent screen must still be shown before the abort, and it must show
	// the ✗ verdict so the user sees what --force would override.
	if !strings.Contains(stdout, "✗") {
		t.Errorf("consent screen missing ✗ verdict, stdout:\n%s", stdout)
	}
	dest := filepath.Join(dataDir, "jind-ai", "plugins", "jind-ai-notifier")
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("plugin dir must not exist after abort: %v", statErr)
	}
}

func TestPluginInstallByNameForceOverridesCompatMismatch(t *testing.T) {
	_, dataDir := setupInstallEnv(t)
	incompatibleManifest := strings.Replace(remoteInstallManifest,
		`jin: ">=0.7.0"`, `jin: ">=99.0.0"`, 1)
	repo, sha := initGitRepoWithManifest(t, incompatibleManifest)
	srv := registryTestServer(t, registryDoc(t, "jind-ai-notifier", repo, sha, ">=99.0.0"))

	stdout, stderr, err := runInstallCmd(t,
		"jind-ai-notifier", "--registry", srv.URL, "--yes", "--refresh", "--force")
	if err != nil {
		t.Fatalf("install --force: err=%v\nstdout=%q\nstderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "✗") {
		t.Errorf("consent screen missing ✗ verdict under --force, stdout:\n%s", stdout)
	}
	dest := filepath.Join(dataDir, "jind-ai", "plugins", "jind-ai-notifier")
	if _, err := os.Stat(filepath.Join(dest, manifest.Filename)); err != nil {
		t.Errorf("plugin not placed under --force at %s: %v", dest, err)
	}
}

func TestPluginInstallByNameUnknownAborts(t *testing.T) {
	setupInstallEnv(t)
	srv := registryTestServer(t, registryDoc(t, "other-plugin", "/tmp/x", strings.Repeat("a", 12), ">=0.7.0"))

	_, _, err := runInstallCmd(t, "jind-ai-notifier", "--registry", srv.URL, "--yes", "--refresh")
	if err == nil {
		t.Fatal("expected error for unknown plugin name")
	}
	if !strings.Contains(err.Error(), "not in the registry") {
		t.Errorf("error should mention 'not in the registry', got: %v", err)
	}
}

// TestPluginInstallGitSourcePathStillWorks guards the existing github.com/…
// path against regressions from the arg-shape dispatch. The clone is served
// off the filesystem so the test stays network-free.
func TestPluginInstallGitSourcePathStillWorks(t *testing.T) {
	_, dataDir := setupInstallEnv(t)
	repo, _ := initGitRepoWithManifest(t, remoteInstallManifest)
	arg := "file://" + repo // contains ":/" — not a bare name, so hits the source path

	stdout, stderr, err := runInstallCmd(t, arg, "--yes")
	if err != nil {
		t.Fatalf("install by URL: err=%v\nstdout=%q\nstderr=%q", err, stdout, stderr)
	}
	dest := filepath.Join(dataDir, "jind-ai", "plugins", "jind-ai-notifier")
	if _, err := os.Stat(filepath.Join(dest, manifest.Filename)); err != nil {
		t.Errorf("git-URL path failed to place plugin at %s: %v", dest, err)
	}
}
