package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/config"
)

// fakeBinder records BindKey invocations so tests can assert what tmux commands
// applyTogglePaneBinding would have issued, without spawning tmux.
type fakeBinder struct {
	calls [][]string // each entry: [key, cmdArg0, cmdArg1, ...]
	err   error
}

func (f *fakeBinder) BindKey(key string, cmdArgs ...string) error {
	entry := append([]string{key}, cmdArgs...)
	f.calls = append(f.calls, entry)
	return f.err
}

// mgrWithYAML builds a config.Manager from an inline YAML fragment. Using the
// real loader keeps these tests honest about viper's decode behaviour instead
// of hand-constructing Manager internals from another package.
func mgrWithYAML(t *testing.T, yamlContent string) *config.Manager {
	t.Helper()
	dir := t.TempDir()
	if yamlContent != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yamlContent), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	m, err := config.NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestApplyTogglePaneBinding_NilConfigMgrIsNoOp(t *testing.T) {
	fb := &fakeBinder{}
	applyTogglePaneBinding(fb, nil, "%42")
	if len(fb.calls) != 0 {
		t.Errorf("expected 0 BindKey calls, got %d: %v", len(fb.calls), fb.calls)
	}
}

func TestApplyTogglePaneBinding_EmptyPaneIDIsNoOp(t *testing.T) {
	fb := &fakeBinder{}
	// Default config (no config.yaml → default M-\ binding would apply if paneID were set).
	applyTogglePaneBinding(fb, mgrWithYAML(t, ""), "")
	if len(fb.calls) != 0 {
		t.Errorf("expected 0 BindKey calls, got %d: %v", len(fb.calls), fb.calls)
	}
}

func TestApplyTogglePaneBinding_DefaultBindsBackslash(t *testing.T) {
	fb := &fakeBinder{}
	applyTogglePaneBinding(fb, mgrWithYAML(t, ""), "%42")
	want := [][]string{
		{"M-\\", "resize-pane", "-Z", "-t", "%42"},
	}
	if !reflect.DeepEqual(fb.calls, want) {
		t.Errorf("BindKey calls mismatch\n got: %v\nwant: %v", fb.calls, want)
	}
}

func TestApplyTogglePaneBinding_ExplicitEmptyDisables(t *testing.T) {
	fb := &fakeBinder{}
	yaml := "keybindings:\n  toggle_pane: []\n"
	applyTogglePaneBinding(fb, mgrWithYAML(t, yaml), "%42")
	if len(fb.calls) != 0 {
		t.Errorf("expected 0 BindKey calls when user set toggle_pane=[], got %d: %v", len(fb.calls), fb.calls)
	}
}

func TestApplyTogglePaneBinding_MultipleKeys(t *testing.T) {
	fb := &fakeBinder{}
	yaml := "keybindings:\n  toggle_pane: [\"M-\\\\\", \"M-b\"]\n"
	applyTogglePaneBinding(fb, mgrWithYAML(t, yaml), "%7")
	want := [][]string{
		{"M-\\", "resize-pane", "-Z", "-t", "%7"},
		{"M-b", "resize-pane", "-Z", "-t", "%7"},
	}
	if !reflect.DeepEqual(fb.calls, want) {
		t.Errorf("BindKey calls mismatch\n got: %v\nwant: %v", fb.calls, want)
	}
}

func TestApplyTogglePaneBinding_EmptyKeyIsSkipped(t *testing.T) {
	fb := &fakeBinder{}
	yaml := "keybindings:\n  toggle_pane: [\"\", \"M-b\"]\n"
	applyTogglePaneBinding(fb, mgrWithYAML(t, yaml), "%9")
	want := [][]string{
		{"M-b", "resize-pane", "-Z", "-t", "%9"},
	}
	if !reflect.DeepEqual(fb.calls, want) {
		t.Errorf("BindKey calls mismatch\n got: %v\nwant: %v", fb.calls, want)
	}
}
