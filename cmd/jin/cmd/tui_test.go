package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/agent/agenttest"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/tmux"
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

func TestApplyActionPanelBinding_DefaultKey(t *testing.T) {
	fb := &fakeBinder{}
	applyActionPanelBinding(fb, mgrWithYAML(t, ""), "/usr/local/bin/jin")
	want := [][]string{
		{"M-p", "display-popup", "-w", "70%", "-h", "70%", "-T", " Action Palette ", "-E", "'/usr/local/bin/jin' action-popup"},
	}
	if !reflect.DeepEqual(fb.calls, want) {
		t.Errorf("BindKey calls mismatch\n got: %v\nwant: %v", fb.calls, want)
	}
}

func TestApplyActionPanelBinding_NoConfig(t *testing.T) {
	fb := &fakeBinder{}
	applyActionPanelBinding(fb, nil, "/usr/local/bin/jin")
	if len(fb.calls) != 0 {
		t.Errorf("expected 0 BindKey calls, got %d: %v", len(fb.calls), fb.calls)
	}
}

func TestApplyActionPanelBinding_EmptySelfBin(t *testing.T) {
	fb := &fakeBinder{}
	applyActionPanelBinding(fb, mgrWithYAML(t, ""), "")
	if len(fb.calls) != 0 {
		t.Errorf("expected 0 BindKey calls, got %d: %v", len(fb.calls), fb.calls)
	}
}

func TestApplyActionPanelBinding_ExplicitEmpty(t *testing.T) {
	fb := &fakeBinder{}
	yaml := "keybindings:\n  action_panel: []\n"
	applyActionPanelBinding(fb, mgrWithYAML(t, yaml), "/usr/local/bin/jin")
	if len(fb.calls) != 0 {
		t.Errorf("expected 0 BindKey calls when user set action_panel=[], got %d: %v", len(fb.calls), fb.calls)
	}
}

// --- validateTuiAgentFlag / setTransientAgentEnv ---

// withRegistry snapshots the current process-global agent registry (which
// root.go's blank import of internal/agent/register has already populated
// with the real adapters at init time), wipes it, and installs the given
// kinds as stubs. Cleanup restores the pre-test snapshot so later tests
// running in the same binary still see the real adapters — otherwise the
// registry would be permanently empty and any subsequent test that touches
// agent.Lookup would silently fail.
func withRegistry(t *testing.T, kinds ...string) {
	t.Helper()
	orig := agenttest.Snapshot()
	agenttest.Reset()
	for _, k := range kinds {
		agent.Register(&agenttest.StubAgent{KindStr: k})
	}
	t.Cleanup(func() { agenttest.Restore(orig) })
}

func TestValidateTuiAgentFlag_EmptyAllowed(t *testing.T) {
	withRegistry(t, "claude", "codex")
	if err := validateTuiAgentFlag(""); err != nil {
		t.Errorf("empty flag returned error: %v", err)
	}
}

func TestValidateTuiAgentFlag_KnownKind(t *testing.T) {
	withRegistry(t, "claude", "codex")
	if err := validateTuiAgentFlag("codex"); err != nil {
		t.Errorf("known kind returned error: %v", err)
	}
}

func TestValidateTuiAgentFlag_UnknownKind(t *testing.T) {
	withRegistry(t, "claude", "codex")
	err := validateTuiAgentFlag("nonsense")
	if err == nil {
		t.Fatal("unknown kind should error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown agent kind") {
		t.Errorf("error = %q, want to contain 'unknown agent kind'", msg)
	}
	if !strings.Contains(msg, "available") {
		t.Errorf("error = %q, want to contain 'available'", msg)
	}
}

// fakeAgentEnvSetter records SetEnvironment / UnsetEnvironment calls so tests
// can assert what setTransientAgentEnv would have issued, without spawning tmux.
type fakeAgentEnvSetter struct {
	sets   [][3]string // [session, name, value]
	unsets [][2]string // [session, name]
}

func (f *fakeAgentEnvSetter) SetEnvironment(session, name, value string) error {
	f.sets = append(f.sets, [3]string{session, name, value})
	return nil
}

func (f *fakeAgentEnvSetter) UnsetEnvironment(session, name string) error {
	f.unsets = append(f.unsets, [2]string{session, name})
	return nil
}

func TestSetTransientAgentEnv_SetsWhenNonEmpty(t *testing.T) {
	fe := &fakeAgentEnvSetter{}
	setTransientAgentEnv(fe, "codex")

	wantSet := [][3]string{{tmux.SessionName, "JIN_UI_AGENT", "codex"}}
	if !reflect.DeepEqual(fe.sets, wantSet) {
		t.Errorf("sets = %v, want %v", fe.sets, wantSet)
	}
	if len(fe.unsets) != 0 {
		t.Errorf("unsets = %v, want none", fe.unsets)
	}
}

func TestSetTransientAgentEnv_UnsetsWhenEmpty(t *testing.T) {
	fe := &fakeAgentEnvSetter{}
	setTransientAgentEnv(fe, "")

	wantUnset := [][2]string{{tmux.SessionName, "JIN_UI_AGENT"}}
	if !reflect.DeepEqual(fe.unsets, wantUnset) {
		t.Errorf("unsets = %v, want %v", fe.unsets, wantUnset)
	}
	if len(fe.sets) != 0 {
		t.Errorf("sets = %v, want none", fe.sets)
	}
}

func TestApplyActionPanelBinding_MultipleKeys(t *testing.T) {
	fb := &fakeBinder{}
	yaml := "keybindings:\n  action_panel: [\"M-p\", \"M-x\"]\n"
	applyActionPanelBinding(fb, mgrWithYAML(t, yaml), "/usr/local/bin/jin")
	want := [][]string{
		{"M-p", "display-popup", "-w", "70%", "-h", "70%", "-T", " Action Palette ", "-E", "'/usr/local/bin/jin' action-popup"},
		{"M-x", "display-popup", "-w", "70%", "-h", "70%", "-T", " Action Palette ", "-E", "'/usr/local/bin/jin' action-popup"},
	}
	if !reflect.DeepEqual(fb.calls, want) {
		t.Errorf("BindKey calls mismatch\n got: %v\nwant: %v", fb.calls, want)
	}
}
