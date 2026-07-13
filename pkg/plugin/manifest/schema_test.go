package manifest

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestParseValidMinimal(t *testing.T) {
	data := mustRead(t, "testdata/manifests/valid_minimal.yaml")

	m, unknown, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unexpected unknown fields: %v", unknown)
	}
	if m.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", m.SchemaVersion)
	}
	if m.Name != "hello-plugin" {
		t.Errorf("Name = %q, want hello-plugin", m.Name)
	}
	if m.Jin != ">=0.7.0" {
		t.Errorf("Jin = %q, want >=0.7.0", m.Jin)
	}
	if m.Install.Source == nil {
		t.Fatalf("Install.Source is nil")
	}
	if m.Install.ReleaseAsset != nil {
		t.Errorf("Install.ReleaseAsset should be nil for source install")
	}
	if got, want := m.Install.Source.Entrypoint, "./bin/hello"; got != want {
		t.Errorf("Entrypoint = %q, want %q", got, want)
	}
	if len(m.Install.Source.Build) != 1 {
		t.Errorf("Build len = %d, want 1", len(m.Install.Source.Build))
	}
	if got, want := m.Entrypoint(), "./bin/hello"; got != want {
		t.Errorf("Entrypoint() = %q, want %q", got, want)
	}
	if len(m.BuildCommands()) != 1 {
		t.Errorf("BuildCommands() len = %d, want 1", len(m.BuildCommands()))
	}
	if len(m.On) != 1 || m.On[0] != "status_changed" {
		t.Errorf("On = %v, want [status_changed]", m.On)
	}
	if m.Timeout != 0 {
		t.Errorf("Timeout = %s, want 0 (unset)", m.Timeout)
	}
	if got := m.EffectiveTimeout(); got != DefaultTimeout {
		t.Errorf("EffectiveTimeout = %s, want %s", got, DefaultTimeout)
	}
}

func TestParseValidReleaseAsset(t *testing.T) {
	data := mustRead(t, "testdata/manifests/valid_release_asset.yaml")

	m, unknown, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unexpected unknown fields: %v", unknown)
	}
	if m.Install.ReleaseAsset == nil {
		t.Fatalf("Install.ReleaseAsset is nil")
	}
	if m.Install.Source != nil {
		t.Errorf("Install.Source should be nil for release_asset install")
	}
	if m.License != "MIT" {
		t.Errorf("License = %q, want MIT", m.License)
	}
	if m.Homepage == "" {
		t.Errorf("Homepage should not be empty")
	}
	if m.Timeout != 45*time.Second {
		t.Errorf("Timeout = %s, want 45s", m.Timeout)
	}
	if m.Popup == nil || m.Popup.Width != 60 || m.Popup.Height != 40 {
		t.Errorf("Popup = %+v, want {Width:60 Height:40}", m.Popup)
	}
	if got := m.Entrypoint(); got != "" {
		t.Errorf("Entrypoint() = %q, want empty for release_asset", got)
	}
	if got := m.BuildCommands(); got != nil {
		t.Errorf("BuildCommands() = %v, want nil for release_asset", got)
	}
}

func TestParseUnknownFields(t *testing.T) {
	data := mustRead(t, "testdata/manifests/valid_unknown_field.yaml")

	_, unknown, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	sort.Strings(unknown)
	if got, want := unknown, []string{"future_field"}; !equalStrings(got, want) {
		t.Errorf("unknown fields = %v, want %v", got, want)
	}
}

func TestParseUnknownInstallField(t *testing.T) {
	yamlDoc := []byte(`schema_version: 1
name: hello
version: 0.1.0
description: nested unknown
jin: ">=0.7.0"
install:
  source:
    build: [go build]
    entrypoint: ./bin/hello
  future_channel: something
`)
	_, unknown, err := Parse(yamlDoc)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	sort.Strings(unknown)
	if got, want := unknown, []string{"install.future_channel"}; !equalStrings(got, want) {
		t.Errorf("unknown fields = %v, want %v", got, want)
	}
}

func TestParseBadYAML(t *testing.T) {
	data := mustRead(t, "testdata/manifests/invalid_bad_yaml.yaml")

	_, _, err := Parse(data)
	if err == nil {
		t.Fatalf("Parse should have failed on malformed YAML")
	}
}

func TestParseTimeoutParseError(t *testing.T) {
	yamlDoc := []byte(`schema_version: 1
name: hello
version: 0.1.0
description: bad timeout
jin: ">=0.7.0"
install:
  source:
    build: [go build]
    entrypoint: ./bin/hello
timeout: "not-a-duration"
`)
	if _, _, err := Parse(yamlDoc); err == nil {
		t.Fatal("Parse with bad timeout: want error, got nil")
	}
}

func TestParseRuntimeFullManifest(t *testing.T) {
	data := mustRead(t, "testdata/manifests/valid_runtime_full.yaml")
	m, _, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != "notifier" {
		t.Errorf("Name = %q, want notifier", m.Name)
	}
	if len(m.On) != 2 || m.On[1] != "status_changed:permission" {
		t.Errorf("On = %v, unexpected", m.On)
	}
	if m.Timeout != 45*time.Second {
		t.Errorf("Timeout = %s, want 45s", m.Timeout)
	}
	if m.Popup == nil || m.Popup.Width != 40 || m.Popup.Height != 20 {
		t.Errorf("Popup = %+v, want {Width:40 Height:20}", m.Popup)
	}
	if got := m.EffectiveTimeout(); got != 45*time.Second {
		t.Errorf("EffectiveTimeout = %s, want 45s", got)
	}
}

func TestLoadFileMissing(t *testing.T) {
	dir := t.TempDir()
	_, _, err := LoadFile(dir)
	if err == nil {
		t.Fatalf("LoadFile should have failed for missing manifest")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got: %v", err)
	}
}

func TestLoadFileHappyPath(t *testing.T) {
	dir := t.TempDir()
	data := mustRead(t, "testdata/manifests/valid_minimal.yaml")
	if err := os.WriteFile(filepath.Join(dir, Filename), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	m, unknown, err := LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile error: %v", err)
	}
	if m.Name != "hello-plugin" || len(unknown) != 0 {
		t.Errorf("LoadFile returned unexpected state: name=%q unknown=%v", m.Name, unknown)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
