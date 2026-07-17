package cmd

import (
	"reflect"
	"testing"

	"github.com/takaaki-s/jind-ai/internal/tui"
)

// --- pluginBindingHints ---
// Flattens the plugin → action → keys map into one sorted help line per
// bound action; actions with no keys contribute nothing.

func TestPluginBindingHints_Empty(t *testing.T) {
	for _, bindings := range []map[string]map[string][]string{
		nil,
		{},
		{"notifier": nil},
		{"notifier": {}},
	} {
		if hints := pluginBindingHints(bindings); len(hints) != 0 {
			t.Errorf("pluginBindingHints(%v) = %v, want empty", bindings, hints)
		}
	}
}

func TestPluginBindingHints_OneLinePerAction(t *testing.T) {
	bindings := map[string]map[string][]string{
		"notifier": {
			"send-dm": {"M-d"},
			"default": {"M-n", "M-!"}, // first key wins the hint
		},
		"worktree-cleanup": {
			"default": {"C-w"},
		},
	}
	got := pluginBindingHints(bindings)
	want := []tui.PluginBindingHint{
		{KeyHint: "Alt+N", Name: "notifier / default"},
		{KeyHint: "Alt+D", Name: "notifier / send-dm"},
		{KeyHint: "Ctrl+W", Name: "worktree-cleanup / default"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pluginBindingHints mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestPluginBindingHints_SkipsKeylessActions(t *testing.T) {
	bindings := map[string]map[string][]string{
		"notifier": {
			"send-dm": {},
			"default": {"M-n"},
		},
	}
	got := pluginBindingHints(bindings)
	want := []tui.PluginBindingHint{
		{KeyHint: "Alt+N", Name: "notifier / default"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pluginBindingHints mismatch\n got: %#v\nwant: %#v", got, want)
	}
}
