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
	if len(m.Actions) != 1 || m.Actions[0].ID != "default" {
		t.Errorf("Actions = %+v, want a single synthesized default action", m.Actions)
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

func TestNormalizeV1MinimalSynthesizesDefaultAction(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	if len(m.Actions) != 1 {
		t.Fatalf("Actions len = %d, want 1", len(m.Actions))
	}
	a := m.Actions[0]
	if a.ID != "default" {
		t.Errorf("Actions[0].ID = %q, want default", a.ID)
	}
	if a.Label != "hello-plugin" {
		t.Errorf("Actions[0].Label = %q, want hello-plugin", a.Label)
	}
	if a.Entrypoint != "./bin/hello" {
		t.Errorf("Actions[0].Entrypoint = %q, want ./bin/hello", a.Entrypoint)
	}
	if len(a.On) != 1 || a.On[0] != "status_changed" {
		t.Errorf("Actions[0].On = %v, want [status_changed]", a.On)
	}
}

func TestNormalizeV1FullSynthesizesDefaultActionWithOnAndPopup(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_runtime_full.yaml")
	if len(m.Actions) != 1 {
		t.Fatalf("Actions len = %d, want 1", len(m.Actions))
	}
	a := m.Actions[0]
	if len(a.On) != 2 || a.On[1] != "status_changed:permission" {
		t.Errorf("Actions[0].On = %v, unexpected", a.On)
	}
	if a.Popup == nil || a.Popup.Width != 40 || a.Popup.Height != 20 {
		t.Errorf("Actions[0].Popup = %+v, want {Width:40 Height:20}", a.Popup)
	}
}

func TestNormalizeV1WithoutEntrypointDoesNotSynthesize(t *testing.T) {
	yamlDoc := []byte(`schema_version: 1
name: hello
version: 0.1.0
description: v1 manifest without an entrypoint
jin: ">=0.7.0"
install:
  source:
    build: [go build]
`)
	m, _, err := Parse(yamlDoc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Actions) != 0 {
		t.Errorf("Actions len = %d, want 0 (no entrypoint to synthesize from)", len(m.Actions))
	}
}

func TestNormalizeIsNoOpForV2(t *testing.T) {
	m, _ := mustParse(t, "testdata/manifests/valid_v2_multi_action.yaml")
	if len(m.Actions) != 2 {
		t.Fatalf("Actions len = %d, want 2 (normalize must not touch explicit v2 actions)", len(m.Actions))
	}
	if m.Actions[0].ID != "notify" || m.Actions[0].Entrypoint != "./bin/notify" {
		t.Errorf("Actions[0] = %+v, unexpected", m.Actions[0])
	}
	if m.Actions[1].ID != "send-dm" || m.Actions[1].Entrypoint != "./bin/send-dm" {
		t.Errorf("Actions[1] = %+v, unexpected", m.Actions[1])
	}
}

func TestDefaultAction(t *testing.T) {
	var empty Manifest
	if got := empty.DefaultAction(); got != nil {
		t.Errorf("DefaultAction() on empty Actions = %+v, want nil", got)
	}
	m := Manifest{Actions: []Action{
		{ID: "default", Entrypoint: "./bin/hello"},
		{ID: "extra", Entrypoint: "./bin/extra"},
	}}
	got := m.DefaultAction()
	if got == nil || got.ID != m.Actions[0].ID || got.Entrypoint != m.Actions[0].Entrypoint {
		t.Errorf("DefaultAction() = %+v, want %+v", got, m.Actions[0])
	}
}

func TestFindAction(t *testing.T) {
	m := Manifest{Actions: []Action{
		{ID: "default", Entrypoint: "./bin/hello"},
		{ID: "send-dm", Entrypoint: "./bin/send-dm"},
	}}
	if got := m.FindAction("send-dm"); got == nil || got.Entrypoint != "./bin/send-dm" {
		t.Errorf("FindAction(send-dm) = %+v, want Entrypoint ./bin/send-dm", got)
	}
	if got := m.FindAction("missing"); got != nil {
		t.Errorf("FindAction(missing) = %+v, want nil", got)
	}
	if got := m.FindAction(""); got != nil {
		t.Errorf(`FindAction("") = %+v, want nil`, got)
	}
}

func TestActionIDs(t *testing.T) {
	m := Manifest{Actions: []Action{{ID: "notify"}, {ID: "send-dm"}}}
	got := m.ActionIDs()
	want := []string{"notify", "send-dm"}
	if !equalStrings(got, want) {
		t.Errorf("ActionIDs() = %v, want %v", got, want)
	}
}

func TestEntrypointFromDefaultAction(t *testing.T) {
	v1, _ := mustParse(t, "testdata/manifests/valid_minimal.yaml")
	if got, want := v1.Entrypoint(), v1.Actions[0].Entrypoint; got != want {
		t.Errorf("v1 Entrypoint() = %q, want %q (Actions[0].Entrypoint)", got, want)
	}
	v2, _ := mustParse(t, "testdata/manifests/valid_v2_multi_action.yaml")
	if got, want := v2.Entrypoint(), v2.Actions[0].Entrypoint; got != want {
		t.Errorf("v2 Entrypoint() = %q, want %q (Actions[0].Entrypoint)", got, want)
	}
}

func TestParseValidV2MultiAction(t *testing.T) {
	m, unknown := mustParse(t, "testdata/manifests/valid_v2_multi_action.yaml")
	if len(unknown) != 0 {
		t.Errorf("unexpected unknown fields: %v", unknown)
	}
	if m.SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d, want 2", m.SchemaVersion)
	}
	if len(m.Actions) != 2 {
		t.Fatalf("Actions len = %d, want 2", len(m.Actions))
	}
	notify := m.Actions[0]
	if notify.ID != "notify" || notify.Entrypoint != "./bin/notify" {
		t.Errorf("Actions[0] = %+v, unexpected", notify)
	}
	if len(notify.On) != 1 || notify.On[0] != "status_changed" {
		t.Errorf("Actions[0].On = %v, unexpected", notify.On)
	}
	if notify.Popup == nil || notify.Popup.Width != 40 || notify.Popup.Height != 20 {
		t.Errorf("Actions[0].Popup = %+v, want {Width:40 Height:20}", notify.Popup)
	}
	sendDM := m.Actions[1]
	if sendDM.ID != "send-dm" || sendDM.Entrypoint != "./bin/send-dm" {
		t.Errorf("Actions[1] = %+v, unexpected", sendDM)
	}
	if len(sendDM.On) != 0 {
		t.Errorf("Actions[1].On = %v, want empty (no on: declared)", sendDM.On)
	}
	if sendDM.Popup != nil {
		t.Errorf("Actions[1].Popup = %+v, want nil (no popup: declared)", sendDM.Popup)
	}
}

func TestParseListenerAction(t *testing.T) {
	m, unknown := mustParse(t, "testdata/manifests/valid_v2_listener_action.yaml")
	if len(unknown) != 0 {
		t.Errorf("unexpected unknown fields: %v", unknown)
	}
	if len(m.Actions) != 2 {
		t.Fatalf("Actions len = %d, want 2", len(m.Actions))
	}
	if m.Actions[0].Listener {
		t.Errorf("Actions[0].Listener = true, want false (list is user-facing)")
	}
	if !m.Actions[1].Listener {
		t.Errorf("Actions[1].Listener = false, want true (listen is the listener)")
	}
}

func TestParseHandoffAction(t *testing.T) {
	m, unknown, err := Parse([]byte(`schema_version: 2
name: pr-provider
version: 0.1.0
description: structured PR provider
jin: ">=0.11.0"
install:
  source: {}
actions:
  - id: create
    entrypoint: ./create
    handoff: true
`))
	if err != nil || len(unknown) != 0 {
		t.Fatalf("Parse = unknown %v, err %v", unknown, err)
	}
	if len(m.Actions) != 1 || !m.Actions[0].Handoff {
		t.Fatalf("Actions = %+v", m.Actions)
	}
}

func TestParseUnknownActionField(t *testing.T) {
	yamlDoc := []byte(`schema_version: 2
name: hello
version: 0.1.0
description: action with an unknown field
jin: ">=0.7.0"
install:
  release_asset:
    pattern: "hello-{os}-{arch}"
actions:
  - id: default
    entrypoint: ./bin/hello
    future_field: x
`)
	_, unknown, err := Parse(yamlDoc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := []string{"actions[default].future_field"}; !equalStrings(unknown, want) {
		t.Errorf("unknown = %v, want %v", unknown, want)
	}
}

func TestParseUnknownActionFieldWithoutID(t *testing.T) {
	yamlDoc := []byte(`schema_version: 2
name: hello
version: 0.1.0
description: action without an id has an unknown field
jin: ">=0.7.0"
install:
  release_asset:
    pattern: "hello-{os}-{arch}"
actions:
  - entrypoint: ./bin/hello
    future_field: x
`)
	_, unknown, err := Parse(yamlDoc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := []string{"actions[].future_field"}; !equalStrings(unknown, want) {
		t.Errorf("unknown = %v, want %v", unknown, want)
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
