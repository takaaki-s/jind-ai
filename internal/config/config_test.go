package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// --- ValidateDetachKey ---

func TestValidateDetachKey_Valid(t *testing.T) {
	validKeys := []string{"ctrl+^", "ctrl+]", "ctrl+\\", "ctrl+g"}
	for _, key := range validKeys {
		if err := ValidateDetachKey(key); err != nil {
			t.Errorf("ValidateDetachKey(%q) returned error: %v", key, err)
		}
	}
}

func TestValidateDetachKey_Invalid(t *testing.T) {
	invalidKeys := []string{"ctrl+a", "", "x", "ctrl+z", "enter"}
	for _, key := range invalidKeys {
		if err := ValidateDetachKey(key); err == nil {
			t.Errorf("ValidateDetachKey(%q) expected error, got nil", key)
		}
	}
}

// --- DefaultKeybindings ---

func TestDefaultKeybindings_AllFieldsNonEmpty(t *testing.T) {
	kb := DefaultKeybindings()
	v := reflect.ValueOf(kb)
	typ := v.Type()

	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		name := typ.Field(i).Name
		if field.Kind() == reflect.Slice && field.Len() == 0 {
			t.Errorf("DefaultKeybindings().%s is empty, expected at least one entry", name)
		}
	}
}

// --- parseKeyToByte ---

func TestParseKeyToByte(t *testing.T) {
	cases := []struct {
		key  string
		want byte
	}{
		{"ctrl+^", 0x1e},
		{"ctrl+]", 0x1d},
		{"ctrl+\\", 0x1c},
		{"ctrl+g", 0x07},
		{"unknown", 0x1d}, // default falls back to ctrl+]
		{"", 0x1d},
	}
	for _, tc := range cases {
		got := parseKeyToByte(tc.key)
		if got != tc.want {
			t.Errorf("parseKeyToByte(%q) = 0x%02x, want 0x%02x", tc.key, got, tc.want)
		}
	}
}

// --- parseKeyToCSIu ---

func TestParseKeyToCSIu(t *testing.T) {
	cases := []struct {
		key  string
		want []byte
	}{
		{"ctrl+^", []byte("\x1b[54;6u")},
		{"ctrl+]", []byte("\x1b[93;5u")},
		{"ctrl+\\", []byte("\x1b[92;5u")},
		{"ctrl+g", []byte("\x1b[103;5u")},
	}
	for _, tc := range cases {
		got := parseKeyToCSIu(tc.key)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("parseKeyToCSIu(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// --- formatKeyHint ---

func TestFormatKeyHint(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"ctrl+^", "Ctrl+^"},
		{"ctrl+]", "Ctrl+]"},
		{"ctrl+\\", "Ctrl+\\"},
		{"ctrl+g", "Ctrl+G"},
		{"unknown", "Ctrl+]"}, // default
		{"", "Ctrl+]"},
	}
	for _, tc := range cases {
		got := formatKeyHint(tc.key)
		if got != tc.want {
			t.Errorf("formatKeyHint(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// --- DefaultWorktreeConfig ---

func TestDefaultWorktreeConfig(t *testing.T) {
	cfg := DefaultWorktreeConfig()
	if cfg.BaseDir != "" {
		t.Errorf("BaseDir default = %q, want empty (resolved at runtime)", cfg.BaseDir)
	}
	if cfg.BranchPrefix != "jin/" {
		t.Errorf("BranchPrefix default = %q, want %q", cfg.BranchPrefix, "jin/")
	}
	if cfg.HookEnabled == nil || !*cfg.HookEnabled {
		t.Errorf("HookEnabled default = %v, want true", cfg.HookEnabled)
	}
	if cfg.HookTimeout != 300 {
		t.Errorf("HookTimeout default = %d, want 300", cfg.HookTimeout)
	}
}

// --- WorktreeConfig hook fields ---

func TestWorktreeConfig_HookDefaults(t *testing.T) {
	m := &Manager{config: &Config{}}
	got := m.GetWorktreeConfig()
	if got.HookEnabled == nil || !*got.HookEnabled {
		t.Errorf("HookEnabled = %v, want true", got.HookEnabled)
	}
	if got.HookTimeout != 300 {
		t.Errorf("HookTimeout = %d, want 300", got.HookTimeout)
	}
}

func TestWorktreeConfig_HookExplicitFalse(t *testing.T) {
	disabled := false
	m := &Manager{config: &Config{
		Worktree: WorktreeConfig{
			HookEnabled: &disabled,
		},
	}}
	got := m.GetWorktreeConfig()
	if got.HookEnabled == nil || *got.HookEnabled {
		t.Errorf("HookEnabled = %v, want false", got.HookEnabled)
	}
}

// --- Manager.GetWorktreeConfig ---

func TestManager_GetWorktreeConfig_FillsDefaults(t *testing.T) {
	m := &Manager{config: &Config{}}
	got := m.GetWorktreeConfig()
	if got.BranchPrefix != "jin/" {
		t.Errorf("BranchPrefix = %q, want %q", got.BranchPrefix, "jin/")
	}
}

func TestManager_GetWorktreeConfig_PreservesUserValues(t *testing.T) {
	m := &Manager{config: &Config{
		Worktree: WorktreeConfig{
			BaseDir:       "/tmp/custom/{name}",
			BranchPrefix:  "topic/",
			DefaultBranch: "develop",
			HookTimeout:   600,
		},
	}}
	got := m.GetWorktreeConfig()
	if got.BaseDir != "/tmp/custom/{name}" {
		t.Errorf("BaseDir = %q, want %q", got.BaseDir, "/tmp/custom/{name}")
	}
	if got.BranchPrefix != "topic/" {
		t.Errorf("BranchPrefix = %q, want %q", got.BranchPrefix, "topic/")
	}
	if got.DefaultBranch != "develop" {
		t.Errorf("DefaultBranch = %q, want %q", got.DefaultBranch, "develop")
	}
	if got.HookTimeout != 600 {
		t.Errorf("HookTimeout = %d, want %d", got.HookTimeout, 600)
	}
}

// --- DefaultPluginsConfig ---

func TestDefaultPluginsConfig(t *testing.T) {
	cfg := DefaultPluginsConfig()
	if cfg.Enabled == nil || !*cfg.Enabled {
		t.Errorf("Enabled default = %v, want true", cfg.Enabled)
	}
	if cfg.Disabled != nil {
		t.Errorf("Disabled default = %v, want nil", cfg.Disabled)
	}
	if cfg.BuildTimeout != 300 {
		t.Errorf("BuildTimeout default = %d, want 300", cfg.BuildTimeout)
	}
	if cfg.Debounce != 3 {
		t.Errorf("Debounce default = %d, want 3", cfg.Debounce)
	}
}

// --- Manager.GetPluginsConfig ---

func TestManager_GetPluginsConfig_FillsDefaults(t *testing.T) {
	m := &Manager{config: &Config{}}
	got := m.GetPluginsConfig()
	if got.Enabled == nil || !*got.Enabled {
		t.Errorf("Enabled = %v, want true", got.Enabled)
	}
	if got.BuildTimeout != 300 {
		t.Errorf("BuildTimeout = %d, want 300", got.BuildTimeout)
	}
	if got.Debounce != 3 {
		t.Errorf("Debounce = %d, want 3", got.Debounce)
	}
}

func TestManager_GetPluginsConfig_PreservesUserValues(t *testing.T) {
	disabled := false
	m := &Manager{config: &Config{
		Plugins: PluginsConfig{
			Enabled:      &disabled,
			Disabled:     []string{"notifier"},
			BuildTimeout: 600,
			Debounce:     10,
		},
	}}
	got := m.GetPluginsConfig()
	if got.Enabled == nil || *got.Enabled {
		t.Errorf("Enabled = %v, want false", got.Enabled)
	}
	if len(got.Disabled) != 1 || got.Disabled[0] != "notifier" {
		t.Errorf("Disabled = %v, want [notifier]", got.Disabled)
	}
	if got.BuildTimeout != 600 {
		t.Errorf("BuildTimeout = %d, want 600", got.BuildTimeout)
	}
	if got.Debounce != 10 {
		t.Errorf("Debounce = %d, want 10", got.Debounce)
	}
}

// --- Manager.GetDefaultAgent ---

func TestManager_GetDefaultAgent_DefaultsToClaude(t *testing.T) {
	m := &Manager{config: &Config{}}
	if got := m.GetDefaultAgent(); got != "claude" {
		t.Errorf("GetDefaultAgent() = %q, want %q", got, "claude")
	}
}

func TestManager_GetDefaultAgent_UsesConfiguredValue(t *testing.T) {
	m := &Manager{config: &Config{DefaultAgent: "codex"}}
	if got := m.GetDefaultAgent(); got != "codex" {
		t.Errorf("GetDefaultAgent() = %q, want %q", got, "codex")
	}
}

func TestManager_GetDefaultAgent_EmptyStringFallsBack(t *testing.T) {
	m := &Manager{config: &Config{DefaultAgent: ""}}
	if got := m.GetDefaultAgent(); got != "claude" {
		t.Errorf("GetDefaultAgent() = %q, want %q", got, "claude")
	}
}

// --- formatKeyForTmux ---

func TestFormatKeyForTmux(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"ctrl+^", "C-^"},
		{"ctrl+]", "C-]"},
		{"ctrl+\\", "C-\\"},
		{"ctrl+g", "C-g"},
		{"unknown", "C-]"}, // default
		{"", "C-]"},
	}
	for _, tc := range cases {
		got := formatKeyForTmux(tc.key)
		if got != tc.want {
			t.Errorf("formatKeyForTmux(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// --- GetTogglePaneKeys ---
// The nil ↔ empty-slice distinction is load-bearing: nil means "user did not
// set it, use default", explicit empty means "user disabled the feature".

func TestGetTogglePaneKeys_DefaultWhenNil(t *testing.T) {
	m := &Manager{config: &Config{}}
	got := m.GetTogglePaneKeys()
	want := []string{"M-\\"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetTogglePaneKeys() = %v, want %v", got, want)
	}
}

func TestGetTogglePaneKeys_UserSet(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{TogglePane: []string{"M-b"}},
	}}
	got := m.GetTogglePaneKeys()
	want := []string{"M-b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetTogglePaneKeys() = %v, want %v", got, want)
	}
}

func TestGetTogglePaneKeys_ExplicitEmptyDisables(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{TogglePane: []string{}},
	}}
	got := m.GetTogglePaneKeys()
	if len(got) != 0 {
		t.Errorf("GetTogglePaneKeys() = %v, want empty slice", got)
	}
}

func TestGetTogglePaneKeys_MultipleKeys(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{TogglePane: []string{"M-\\", "M-b"}},
	}}
	got := m.GetTogglePaneKeys()
	want := []string{"M-\\", "M-b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetTogglePaneKeys() = %v, want %v", got, want)
	}
}

// --- DefaultKeybindings ActionPanel ---

func TestDefaultKeybindings_ActionPanel(t *testing.T) {
	kb := DefaultKeybindings()
	want := []string{"M-p"}
	if !reflect.DeepEqual(kb.ActionPanel, want) {
		t.Errorf("DefaultKeybindings().ActionPanel = %v, want %v", kb.ActionPanel, want)
	}
}

// --- GetActionPanelKeys ---
// The nil ↔ empty-slice distinction is load-bearing: nil means "user did not
// set it, use default", explicit empty means "user disabled the feature".

func TestGetActionPanelKeys_DefaultWhenNil(t *testing.T) {
	m := &Manager{config: &Config{}}
	got := m.GetActionPanelKeys()
	want := []string{"M-p"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetActionPanelKeys() = %v, want %v", got, want)
	}
}

func TestGetActionPanelKeys_UserSet(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{ActionPanel: []string{"M-x"}},
	}}
	got := m.GetActionPanelKeys()
	want := []string{"M-x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetActionPanelKeys() = %v, want %v", got, want)
	}
}

func TestGetActionPanelKeys_ExplicitEmptyDisables(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{ActionPanel: []string{}},
	}}
	got := m.GetActionPanelKeys()
	if len(got) != 0 {
		t.Errorf("GetActionPanelKeys() = %v, want empty slice", got)
	}
}

func TestGetActionPanelKeys_MultipleKeys(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{ActionPanel: []string{"M-p", "M-x"}},
	}}
	got := m.GetActionPanelKeys()
	want := []string{"M-p", "M-x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetActionPanelKeys() = %v, want %v", got, want)
	}
}

// --- GetSessionFilterKeys ---
// Same nil ↔ empty-slice semantics as GetActionPanelKeys, sourced from
// keybindings.search (repurposed from the removed inline substring filter).

func TestGetSessionFilterKeys_DefaultWhenNil(t *testing.T) {
	m := &Manager{config: &Config{}}
	got := m.GetSessionFilterKeys()
	want := []string{"M-f"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetSessionFilterKeys() = %v, want %v", got, want)
	}
}

func TestGetSessionFilterKeys_UserSet(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{Search: []string{"ctrl+p"}},
	}}
	got := m.GetSessionFilterKeys()
	// "+"-notation input is normalized to tmux bind-key notation so tmux
	// actually accepts the binding (see normalizeTmuxKey).
	want := []string{"C-p"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetSessionFilterKeys() = %v, want %v", got, want)
	}
}

func TestGetSessionFilterKeys_ExplicitEmptyDisables(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{Search: []string{}},
	}}
	got := m.GetSessionFilterKeys()
	if len(got) != 0 {
		t.Errorf("GetSessionFilterKeys() = %v, want empty slice", got)
	}
}

// --- GetPluginKeybindings ---
// nil config field ⇒ empty map (never nil). Maps / Keys slices are
// defensively copied so callers cannot mutate Manager state via the
// returned value.

// pluginKB is shorthand for building the nested per-plugin config value.
func pluginKB(actions map[string][]string) PluginKeybindings {
	out := PluginKeybindings{Actions: make(map[string]PluginActionKeybinding, len(actions))}
	for id, keys := range actions {
		out.Actions[id] = PluginActionKeybinding{Keys: keys}
	}
	return out
}

func TestGetPluginKeybindings_DefaultWhenNil(t *testing.T) {
	m := &Manager{config: &Config{}}
	got := m.GetPluginKeybindings()
	if got == nil {
		t.Fatalf("GetPluginKeybindings() = nil, want empty map")
	}
	if len(got) != 0 {
		t.Errorf("GetPluginKeybindings() = %v, want empty map", got)
	}
}

func TestGetPluginKeybindings_SingleAction(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{
			Plugins: map[string]PluginKeybindings{
				"notifier": pluginKB(map[string][]string{"default": {"M-n"}}),
			},
		},
	}}
	got := m.GetPluginKeybindings()
	want := map[string]map[string][]string{
		"notifier": {"default": {"M-n"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetPluginKeybindings() = %#v, want %#v", got, want)
	}
}

func TestGetPluginKeybindings_MultiplePluginsAndActions(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{
			Plugins: map[string]PluginKeybindings{
				"notifier": pluginKB(map[string][]string{
					"default": {"M-n"},
					"send-dm": {"M-d", "M-!"},
				}),
				"worktree-cleanup": pluginKB(map[string][]string{"default": {"M-w"}}),
			},
		},
	}}
	got := m.GetPluginKeybindings()
	want := map[string]map[string][]string{
		"notifier": {
			"default": {"M-n"},
			"send-dm": {"M-d", "M-!"},
		},
		"worktree-cleanup": {"default": {"M-w"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetPluginKeybindings() = %#v, want %#v", got, want)
	}
}

func TestGetPluginKeybindings_EmptyActionsTreatedAsNoBindings(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{
			Plugins: map[string]PluginKeybindings{
				"no-actions": {},                                            // actions: {} or absent
				"empty-keys": pluginKB(map[string][]string{"default": nil}), // keys: []
				"only-real":  pluginKB(map[string][]string{"default": {"M-n"}}),
			},
		},
	}}
	got := m.GetPluginKeybindings()
	if len(got["no-actions"]) != 0 {
		t.Errorf("no-actions = %v, want no bindings", got["no-actions"])
	}
	if len(got["empty-keys"]) != 0 {
		t.Errorf("empty-keys = %v, want no bindings (empty Keys omitted)", got["empty-keys"])
	}
	if !reflect.DeepEqual(got["only-real"], map[string][]string{"default": {"M-n"}}) {
		t.Errorf("only-real = %v, want default:[M-n]", got["only-real"])
	}
}

func TestGetPluginKeybindings_NormalizesPlusNotation(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{
			Plugins: map[string]PluginKeybindings{
				"notifier": pluginKB(map[string][]string{"send-dm": {"ctrl+d", "alt+n"}}),
			},
		},
	}}
	got := m.GetPluginKeybindings()
	want := map[string][]string{"send-dm": {"C-d", "M-n"}}
	if !reflect.DeepEqual(got["notifier"], want) {
		t.Errorf("notifier = %v, want %v", got["notifier"], want)
	}
}

func TestGetPluginKeybindings_ReturnsDefensiveCopy(t *testing.T) {
	m := &Manager{config: &Config{
		Keybindings: KeybindingsConfig{
			Plugins: map[string]PluginKeybindings{
				"notifier": pluginKB(map[string][]string{"default": {"M-n"}}),
			},
		},
	}}
	got := m.GetPluginKeybindings()

	// Mutating the returned maps / slices must not touch Manager internals.
	got["notifier"]["default"] = []string{"M-x"}
	got["notifier"]["injected-action"] = []string{"M-y"}
	got["injected"] = map[string][]string{"default": {"M-z"}}

	fresh := m.GetPluginKeybindings()
	if len(fresh) != 1 {
		t.Errorf("post-mutation fresh copy has %d entries, want 1", len(fresh))
	}
	if !reflect.DeepEqual(fresh["notifier"], map[string][]string{"default": {"M-n"}}) {
		t.Errorf("post-mutation fresh notifier = %#v, want default:[M-n]", fresh["notifier"])
	}
	if _, ok := fresh["injected"]; ok {
		t.Errorf("post-mutation fresh copy leaked 'injected' entry from caller mutation")
	}

	// Same guarantee for the Keys slice itself.
	firstCall := m.GetPluginKeybindings()
	firstCall["notifier"]["default"][0] = "MUTATED"
	secondCall := m.GetPluginKeybindings()
	if secondCall["notifier"]["default"][0] != "M-n" {
		t.Errorf("Keys slice not defensively copied: got %q, want M-n", secondCall["notifier"]["default"][0])
	}
}

// --- plugin keybindings YAML decode + deprecated v1 shape ---

// newManagerWithYAML writes config.yaml into a temp dir and loads it through
// the real viper pipeline (same helper pattern as writePopupYAML).
func newManagerWithYAML(t *testing.T, body string) *Manager {
	t.Helper()
	m, err := NewManager(writePopupYAML(t, body))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestPluginKeybindings_YAMLDecode_NewShape(t *testing.T) {
	body := `
keybindings:
  plugins:
    notifier:
      actions:
        default:
          keys: ["M-n"]
        send-dm:
          keys: ["ctrl+d", "M-!"]
`
	m := newManagerWithYAML(t, body)
	got := m.GetPluginKeybindings()
	want := map[string]map[string][]string{
		"notifier": {
			"default": {"M-n"},
			"send-dm": {"C-d", "M-!"}, // "+" notation normalized to tmux form
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetPluginKeybindings() = %#v, want %#v", got, want)
	}
}

func TestPluginKeybindings_YAMLDecode_EmptyActions(t *testing.T) {
	body := `
keybindings:
  plugins:
    notifier:
      actions: {}
`
	m := newManagerWithYAML(t, body)
	got := m.GetPluginKeybindings()
	if len(got["notifier"]) != 0 {
		t.Errorf("notifier = %v, want no bindings for empty actions map", got["notifier"])
	}
}

func TestPluginKeybindings_DeprecatedV1Shape_WarnsAndDrops(t *testing.T) {
	body := `
keybindings:
  plugins:
    notifier:
      keys: ["M-n"]
    modern:
      actions:
        default:
          keys: ["M-m"]
`
	var m *Manager
	logged := captureLog(t, func() {
		m = newManagerWithYAML(t, body)
	})

	if !strings.Contains(logged, "notifier uses deprecated v1 shape") {
		t.Errorf("expected deprecation warning for notifier, got log: %q", logged)
	}
	if strings.Contains(logged, "modern") {
		t.Errorf("modern (new shape) must not be warned about, got log: %q", logged)
	}

	got := m.GetPluginKeybindings()
	if _, ok := got["notifier"]; ok {
		t.Errorf("notifier binding = %v, want dropped entirely", got["notifier"])
	}
	if !reflect.DeepEqual(got["modern"], map[string][]string{"default": {"M-m"}}) {
		t.Errorf("modern = %v, want default:[M-m]", got["modern"])
	}
}

func TestPluginKeybindings_DeprecatedV1Shape_WarnsOncePerPlugin(t *testing.T) {
	body := `
keybindings:
  plugins:
    notifier:
      keys: ["M-n"]
`
	var m *Manager
	first := captureLog(t, func() {
		m = newManagerWithYAML(t, body)
	})
	if !strings.Contains(first, "deprecated v1 shape") {
		t.Fatalf("expected deprecation warning on first load, got: %q", first)
	}

	second := captureLog(t, func() {
		if err := m.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
	})
	if strings.Contains(second, "deprecated v1 shape") {
		t.Errorf("expected no repeated warning on Reload, got: %q", second)
	}

	// The binding stays dropped after Reload even though the warning is muted.
	if _, ok := m.GetPluginKeybindings()["notifier"]; ok {
		t.Errorf("notifier binding resurfaced after Reload")
	}
}

// --- PopupsConfig YAML decode ---

// writePopupYAML writes a config.yaml into a fresh temp dir and returns the
// dir. Callers pass it straight to NewManager to exercise viper decode.
func writePopupYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

func TestPopupsConfig_YAMLDecode_ParsesFullSchema(t *testing.T) {
	body := `
popups:
  create:         { width: 80, height: 80 }
  session_filter: { width: 65, height: 65 }
  help:           { width: 60, height: 55 }
  action:         { width: 75, height: 75 }
  plugin_default: { width: 50, height: 50 }
  plugins:
    my-plugin:    { width: 40, height: 20 }
    other:        { width: 30, height: 15 }
`
	m, err := NewManager(writePopupYAML(t, body))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	p := m.Get().Popups

	cases := []struct {
		name         string
		got          *PopupSizeConfig
		wantW, wantH int
	}{
		{"create", p.Create, 80, 80},
		{"session_filter", p.SessionFilter, 65, 65},
		{"help", p.Help, 60, 55},
		{"action", p.Action, 75, 75},
		{"plugin_default", p.PluginDefault, 50, 50},
	}
	for _, c := range cases {
		if c.got == nil {
			t.Errorf("Popups.%s = nil, want non-nil", c.name)
			continue
		}
		if c.got.Width != c.wantW || c.got.Height != c.wantH {
			t.Errorf("Popups.%s = {W:%d H:%d}, want {W:%d H:%d}", c.name, c.got.Width, c.got.Height, c.wantW, c.wantH)
		}
	}

	if p.Plugins == nil {
		t.Fatalf("Popups.Plugins = nil, want populated map")
	}
	mp, ok := p.Plugins["my-plugin"]
	if !ok || mp == nil {
		t.Fatalf("Popups.Plugins[my-plugin] missing")
	}
	if mp.Width != 40 || mp.Height != 20 {
		t.Errorf("Popups.Plugins[my-plugin] = {W:%d H:%d}, want {W:40 H:20}", mp.Width, mp.Height)
	}
	other, ok := p.Plugins["other"]
	if !ok || other == nil {
		t.Fatalf("Popups.Plugins[other] missing")
	}
	if other.Width != 30 || other.Height != 15 {
		t.Errorf("Popups.Plugins[other] = {W:%d H:%d}, want {W:30 H:15}", other.Width, other.Height)
	}
}

func TestPopupsConfig_YAMLDecode_MissingKeepsBackwardCompat(t *testing.T) {
	body := `
keybindings:
  up: ["k"]
`
	m, err := NewManager(writePopupYAML(t, body))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	p := m.Get().Popups
	if p.Create != nil || p.SessionFilter != nil || p.Help != nil || p.Action != nil {
		t.Errorf("expected all Popups core fields nil, got %+v", p)
	}
	if p.PluginDefault != nil {
		t.Errorf("expected PluginDefault nil, got %+v", p.PluginDefault)
	}
	if len(p.Plugins) != 0 {
		t.Errorf("expected Plugins nil/empty, got %+v", p.Plugins)
	}
}

// --- DefaultPopupSizes ---

func TestDefaultPopupSizes_ContainsExpectedKeys(t *testing.T) {
	sizes := DefaultPopupSizes()
	wantKeys := []string{"create", "session_filter", "help", "action", "plugin_default", "default"}
	for _, k := range wantKeys {
		v, ok := sizes[k]
		if !ok {
			t.Errorf("DefaultPopupSizes missing key %q", k)
			continue
		}
		if v.Width < 1 || v.Width > 100 || v.Height < 1 || v.Height > 100 {
			t.Errorf("DefaultPopupSizes[%q] = %+v, want dims in [1,100]", k, v)
		}
	}
}

// --- GetPopupSize ---

func TestGetPopupSize_NilReturnsDefault(t *testing.T) {
	m := &Manager{config: &Config{}}
	cases := []struct {
		name         string
		wantW, wantH string
	}{
		{"create", "80%", "80%"},
		{"session_filter", "70%", "70%"},
		{"help", "60%", "60%"},
		{"action", "70%", "70%"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotW, gotH := m.GetPopupSize(c.name)
			if gotW != c.wantW || gotH != c.wantH {
				t.Errorf("GetPopupSize(%q) = (%q, %q), want (%q, %q)", c.name, gotW, gotH, c.wantW, c.wantH)
			}
		})
	}
}

func TestGetPopupSize_UserOverride_UsesConfigValue(t *testing.T) {
	m := &Manager{config: &Config{
		Popups: PopupsConfig{
			Create: &PopupSizeConfig{Width: 50, Height: 40},
		},
	}}
	gotW, gotH := m.GetPopupSize("create")
	if gotW != "50%" || gotH != "40%" {
		t.Errorf("GetPopupSize(create) = (%q, %q), want (50%%, 40%%)", gotW, gotH)
	}
}

func TestGetPopupSize_UnknownName_ReturnsFallbackDefault(t *testing.T) {
	m := &Manager{config: &Config{}}
	gotW, gotH := m.GetPopupSize("does-not-exist")
	if gotW != "70%" || gotH != "70%" {
		t.Errorf("GetPopupSize(unknown) = (%q, %q), want (70%%, 70%%)", gotW, gotH)
	}
}

// captureLog redirects the default logger to a buffer for the duration of the
// callback, restoring the original writer afterwards.
func captureLog(t *testing.T, f func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	f()
	return buf.String()
}

func TestGetPopupSize_OutOfRange_LogsWarnAndFallsBack(t *testing.T) {
	cases := []struct {
		label    string
		width    int
		wantWarn bool
	}{
		{"negative", -10, true},
		{"zero", 0, false},
		{"over max", 101, true},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			m := &Manager{config: &Config{
				Popups: PopupsConfig{
					Create: &PopupSizeConfig{Width: c.width, Height: 80},
				},
			}}
			var gotW, gotH string
			out := captureLog(t, func() {
				gotW, gotH = m.GetPopupSize("create")
			})
			if gotW != "80%" {
				t.Errorf("width = %q, want fallback %q", gotW, "80%")
			}
			if gotH != "80%" {
				t.Errorf("height = %q, want %q", gotH, "80%")
			}
			hasWarn := strings.Contains(out, "out of range")
			if hasWarn != c.wantWarn {
				t.Errorf("warn present = %v, want %v (log=%q)", hasWarn, c.wantWarn, out)
			}
		})
	}
}

func TestGetPopupSize_WarnEmittedOncePerKey(t *testing.T) {
	m := &Manager{config: &Config{
		Popups: PopupsConfig{
			Create: &PopupSizeConfig{Width: 200, Height: 80},
		},
	}}
	out := captureLog(t, func() {
		m.GetPopupSize("create")
		m.GetPopupSize("create")
		m.GetPopupSize("create")
	})
	count := strings.Count(out, "out of range")
	if count != 1 {
		t.Errorf("warn emitted %d times, want 1 (log=%q)", count, out)
	}
}

// --- GetPluginPopupSize ---

func TestGetPluginPopupSize_ConfigOverrideManifest(t *testing.T) {
	m := &Manager{config: &Config{
		Popups: PopupsConfig{
			Plugins: map[string]*PopupSizeConfig{
				"notifier": {Width: 60, Height: 50},
			},
			PluginDefault: &PopupSizeConfig{Width: 30, Height: 30},
		},
	}}
	manifest := &PopupSizeConfig{Width: 40, Height: 40}
	gotW, gotH := m.GetPluginPopupSize("notifier", manifest)
	if gotW != "60%" || gotH != "50%" {
		t.Errorf("got (%q, %q), want (60%%, 50%%)", gotW, gotH)
	}
}

func TestGetPluginPopupSize_ManifestOnly(t *testing.T) {
	m := &Manager{config: &Config{}}
	manifest := &PopupSizeConfig{Width: 40, Height: 40}
	gotW, gotH := m.GetPluginPopupSize("notifier", manifest)
	if gotW != "40%" || gotH != "40%" {
		t.Errorf("got (%q, %q), want (40%%, 40%%)", gotW, gotH)
	}
}

func TestGetPluginPopupSize_PluginDefaultConfig(t *testing.T) {
	m := &Manager{config: &Config{
		Popups: PopupsConfig{
			PluginDefault: &PopupSizeConfig{Width: 90, Height: 90},
		},
	}}
	gotW, gotH := m.GetPluginPopupSize("notifier", nil)
	if gotW != "90%" || gotH != "90%" {
		t.Errorf("got (%q, %q), want (90%%, 90%%)", gotW, gotH)
	}
}

func TestGetPluginPopupSize_HardcodedDefault(t *testing.T) {
	m := &Manager{config: &Config{}}
	gotW, gotH := m.GetPluginPopupSize("notifier", nil)
	if gotW != "70%" || gotH != "70%" {
		t.Errorf("got (%q, %q), want (70%%, 70%%)", gotW, gotH)
	}
}

func TestGetPluginPopupSize_EmptyManifestAndConfig_UsesPluginDefault(t *testing.T) {
	m := &Manager{config: &Config{}}
	// An explicit but zero-valued manifest should be treated the same as
	// no manifest (silent fallback), landing on the hardcoded default.
	gotW, gotH := m.GetPluginPopupSize("notifier", &PopupSizeConfig{})
	if gotW != "70%" || gotH != "70%" {
		t.Errorf("got (%q, %q), want (70%%, 70%%)", gotW, gotH)
	}
}
