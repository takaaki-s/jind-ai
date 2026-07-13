package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// writePluginDir creates <pluginsDir>/<name> with a valid manifest declaring
// the given jin compat range. name is inlined into the YAML so the registry
// sees the same name from disk that the caller expects.
func writePluginDir(t *testing.T, pluginsDir, name, jinRange string) {
	t.Helper()
	dir := filepath.Join(pluginsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	content := fmt.Sprintf(`schema_version: 1
name: %s
version: 0.1.0
description: test fixture
jin: %q
install:
  source:
    build: ["true"]
    entrypoint: ./run.sh
on:
  - status_changed
`, name, jinRange)
	if err := os.WriteFile(filepath.Join(dir, manifest.Filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// lockPlugin records name in the lock file under stateDir, mirroring what an
// install would write.
func lockPlugin(t *testing.T, stateDir, name string) {
	t.Helper()
	l, err := LoadLock(stateDir)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	if err := l.Set(name, LockEntry{Source: "github.com/owner/" + name, InstalledAt: time.Now().UTC()}); err != nil {
		t.Fatalf("lock Set: %v", err)
	}
}

func enabledConfig() config.PluginsConfig {
	tru := true
	return config.PluginsConfig{Enabled: &tru}
}

func TestRegistry_LoadEnabled(t *testing.T) {
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	writePluginDir(t, pluginsDir, "notifier", ">=0.0.0")
	lockPlugin(t, stateDir, "notifier")

	entries, err := NewRegistry(pluginsDir, stateDir, enabledConfig()).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Name != "notifier" {
		t.Errorf("Name = %q, want notifier", e.Name)
	}
	if e.State != StateEnabled {
		t.Errorf("State = %s, want enabled", e.State)
	}
	if e.Manifest == nil {
		t.Error("Manifest = nil, want non-nil for enabled plugin")
	}
	if e.Err != nil {
		t.Errorf("Err = %v, want nil", e.Err)
	}
	if e.Lock.Source != "github.com/owner/notifier" {
		t.Errorf("Lock.Source = %q, unexpected", e.Lock.Source)
	}
}

func TestRegistry_MissingManifestIsBroken(t *testing.T) {
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(pluginsDir, "notifier"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockPlugin(t, stateDir, "notifier")

	entries, err := NewRegistry(pluginsDir, stateDir, enabledConfig()).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.State != StateBroken {
		t.Errorf("State = %s, want broken", e.State)
	}
	if e.Manifest != nil {
		t.Error("Manifest should be nil for broken plugin")
	}
	if e.Err == nil {
		t.Error("Err should be set for broken plugin")
	}
}

func TestRegistry_IncompatibleJinRange(t *testing.T) {
	restore := setJinVersionForTest(t, "0.5.0")
	defer restore()

	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	writePluginDir(t, pluginsDir, "notifier", ">=99.0.0")
	lockPlugin(t, stateDir, "notifier")

	entries, err := NewRegistry(pluginsDir, stateDir, enabledConfig()).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := entries[0]
	if e.State != StateIncompatible {
		t.Errorf("State = %s, want incompatible", e.State)
	}
	if e.Manifest == nil {
		t.Error("Manifest should be present even when incompatible")
	}
	if e.Err == nil {
		t.Fatal("Err should explain the incompatibility")
	}
}

func TestRegistry_DisabledByName(t *testing.T) {
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	writePluginDir(t, pluginsDir, "notifier", ">=0.0.0")
	lockPlugin(t, stateDir, "notifier")

	tru := true
	cfg := config.PluginsConfig{Enabled: &tru, Disabled: []string{"notifier"}}
	entries, err := NewRegistry(pluginsDir, stateDir, cfg).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if entries[0].State != StateDisabled {
		t.Errorf("State = %s, want disabled", entries[0].State)
	}
}

func TestRegistry_GloballyDisabled(t *testing.T) {
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	writePluginDir(t, pluginsDir, "alpha", ">=0.0.0")
	writePluginDir(t, pluginsDir, "beta", ">=0.0.0")
	lockPlugin(t, stateDir, "alpha")
	lockPlugin(t, stateDir, "beta")

	no := false
	entries, err := NewRegistry(pluginsDir, stateDir, config.PluginsConfig{Enabled: &no}).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range entries {
		if e.State != StateDisabled {
			t.Errorf("%s: State = %s, want disabled", e.Name, e.State)
		}
	}
}

func TestRegistry_UnlockedDirIgnored(t *testing.T) {
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	writePluginDir(t, pluginsDir, "locked", ">=0.0.0")
	writePluginDir(t, pluginsDir, "unlocked", ">=0.0.0") // on disk but never installed via lock
	lockPlugin(t, stateDir, "locked")

	entries, err := NewRegistry(pluginsDir, stateDir, enabledConfig()).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1 (unlocked dir must be ignored)", len(entries))
	}
	if entries[0].Name != "locked" {
		t.Errorf("Name = %q, want locked", entries[0].Name)
	}
}

func TestRegistry_MissingDirIsBroken(t *testing.T) {
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	lockPlugin(t, stateDir, "ghost") // locked but no directory on disk

	entries, err := NewRegistry(pluginsDir, stateDir, enabledConfig()).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].State != StateBroken {
		t.Errorf("State = %s, want broken", entries[0].State)
	}
	if entries[0].Err == nil {
		t.Error("Err should be set for a missing directory")
	}
}

func TestRegistry_RunnableOnlyEnabled(t *testing.T) {
	restore := setJinVersionForTest(t, "0.5.0")
	defer restore()

	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	writePluginDir(t, pluginsDir, "good", ">=0.0.0")
	writePluginDir(t, pluginsDir, "old", ">=99.0.0") // incompatible
	writePluginDir(t, pluginsDir, "off", ">=0.0.0")  // disabled by config
	if err := os.MkdirAll(filepath.Join(pluginsDir, "bad"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err) // broken: dir without manifest
	}
	for _, n := range []string{"good", "old", "off", "bad"} {
		lockPlugin(t, stateDir, n)
	}

	tru := true
	cfg := config.PluginsConfig{Enabled: &tru, Disabled: []string{"off"}}
	runnable, err := NewRegistry(pluginsDir, stateDir, cfg).Runnable()
	if err != nil {
		t.Fatalf("Runnable: %v", err)
	}
	if len(runnable) != 1 {
		t.Fatalf("len(runnable) = %d, want 1", len(runnable))
	}
	if runnable[0].Name != "good" || runnable[0].State != StateEnabled {
		t.Errorf("runnable[0] = %s/%s, want good/enabled", runnable[0].Name, runnable[0].State)
	}
}

func TestRegistry_SortedByName(t *testing.T) {
	pluginsDir, stateDir := t.TempDir(), t.TempDir()
	for _, n := range []string{"gamma", "alpha", "beta"} {
		writePluginDir(t, pluginsDir, n, ">=0.0.0")
		lockPlugin(t, stateDir, n)
	}

	entries, err := NewRegistry(pluginsDir, stateDir, enabledConfig()).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := []string{entries[0].Name, entries[1].Name, entries[2].Name}
	want := []string{"alpha", "beta", "gamma"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestPluginState_String(t *testing.T) {
	cases := map[PluginState]string{
		StateEnabled:      "enabled",
		StateDisabled:     "disabled",
		StateIncompatible: "incompatible",
		StateBroken:       "broken",
	}
	for state, want := range cases {
		if got := state.String(); got != want {
			t.Errorf("PluginState(%d).String() = %q, want %q", int(state), got, want)
		}
	}
}
