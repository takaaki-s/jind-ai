package config

import (
	"bytes"
	"reflect"
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
