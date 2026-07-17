package action

import (
	"testing"

	"github.com/takaaki-s/jind-ai/internal/plugin"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

func TestCoreActions_LabelStable(t *testing.T) {
	want := map[string]string{
		IDNew:           "new session",
		IDKill:          "kill session",
		IDDelete:        "delete session",
		IDRefresh:       "refresh list",
		IDVscode:        "open in vscode",
		IDHelp:          "shortcuts help",
		IDSessionFilter: "session filter",
		IDTogglePane:    "toggle sidebar",
	}

	actions := CoreActions(KeyBindings{})
	if len(actions) != len(want) {
		t.Fatalf("len(actions) = %d, want %d", len(actions), len(want))
	}
	for _, a := range actions {
		label, ok := want[a.ID]
		if !ok {
			t.Errorf("unexpected action ID %q", a.ID)
			continue
		}
		if a.Label != label {
			t.Errorf("action %q: Label = %q, want %q", a.ID, a.Label, label)
		}
	}
}

func TestCoreActions_ShortcutResolution(t *testing.T) {
	kb := KeyBindings{
		New:        []string{"n", "N"},
		Kill:       []string{"x", "X"},
		Delete:     []string{"d", "D"},
		Refresh:    []string{"r"},
		Vscode:     []string{"v"},
		Help:       []string{"?"},
		TogglePane: []string{"M-\\"},
		Search:     []string{"M-f"},
	}
	want := map[string]string{
		IDNew:           "n",
		IDKill:          "x",
		IDDelete:        "d",
		IDRefresh:       "r",
		IDVscode:        "v",
		IDHelp:          "?",
		IDTogglePane:    "Alt+\\",
		IDSessionFilter: "Alt+F",
	}

	for _, a := range CoreActions(kb) {
		if got, want := a.Shortcut, want[a.ID]; got != want {
			t.Errorf("action %q: Shortcut = %q, want %q", a.ID, got, want)
		}
	}
}

func TestCoreActions_EmptyKeyBindings(t *testing.T) {
	for _, a := range CoreActions(KeyBindings{}) {
		if a.Shortcut != "" {
			t.Errorf("action %q: Shortcut = %q, want empty", a.ID, a.Shortcut)
		}
	}
}

func TestCoreActions_NeedsSession(t *testing.T) {
	want := map[string]bool{
		IDNew:           false,
		IDKill:          true,
		IDDelete:        true,
		IDRefresh:       false,
		IDVscode:        true,
		IDHelp:          false,
		IDTogglePane:    false,
		IDSessionFilter: false,
	}

	for _, a := range CoreActions(KeyBindings{}) {
		if a.NeedsSession != want[a.ID] {
			t.Errorf("action %q: NeedsSession = %v, want %v", a.ID, a.NeedsSession, want[a.ID])
		}
	}
}

// singleActionManifest mirrors what manifest normalize() synthesizes for a
// v1 manifest: one action with ID "default" and Label = plugin name.
func singleActionManifest(name, desc string) *manifest.Manifest {
	return &manifest.Manifest{
		Description: desc,
		Actions: []manifest.Action{
			{ID: "default", Label: name, Entrypoint: "bin/run"},
		},
	}
}

func TestPluginActionID_RoundTrip(t *testing.T) {
	id := PluginActionID("notifier", "send-dm")
	if id != "plugin:notifier:send-dm" {
		t.Fatalf("PluginActionID = %q, want plugin:notifier:send-dm", id)
	}
	name, actionID, ok := ParsePluginActionID(id)
	if !ok || name != "notifier" || actionID != "send-dm" {
		t.Errorf("ParsePluginActionID(%q) = (%q, %q, %v), want (notifier, send-dm, true)", id, name, actionID, ok)
	}
}

func TestParsePluginActionID_Invalid(t *testing.T) {
	cases := []string{
		"core:new",         // non-plugin ID
		"plugin:notifier",  // legacy two-segment ID
		"plugin::send-dm",  // empty plugin name
		"plugin:notifier:", // empty action ID
		"plugin:",          // prefix only
		"",                 // empty
		"notifier:send-dm", // missing prefix
	}
	for _, id := range cases {
		if name, actionID, ok := ParsePluginActionID(id); ok {
			t.Errorf("ParsePluginActionID(%q) = (%q, %q, true), want ok=false", id, name, actionID)
		}
	}
}

func TestPluginActions_FanOut(t *testing.T) {
	entries := []plugin.Entry{
		{Name: "notifier", Manifest: &manifest.Manifest{
			Description: "slack notifications",
			Actions: []manifest.Action{
				{ID: "default", Label: "notifier", Entrypoint: "bin/notify"},
				{ID: "send-dm", Entrypoint: "bin/send-dm"},
			},
		}},
	}

	actions := PluginActions(entries, nil)
	if len(actions) != 2 {
		t.Fatalf("len(actions) = %d, want 2 (one row per action)", len(actions))
	}
	wantIDs := []string{"plugin:notifier:default", "plugin:notifier:send-dm"}
	for i, a := range actions {
		if a.ID != wantIDs[i] {
			t.Errorf("actions[%d].ID = %q, want %q", i, a.ID, wantIDs[i])
		}
		if a.Kind != KindPlugin {
			t.Errorf("actions[%d].Kind = %v, want KindPlugin", i, a.Kind)
		}
		if a.Description != "slack notifications" {
			t.Errorf("actions[%d].Description = %q, want manifest description", i, a.Description)
		}
	}
}

func TestPluginActions_LabelRules(t *testing.T) {
	entries := []plugin.Entry{
		{Name: "notifier", Manifest: &manifest.Manifest{
			Actions: []manifest.Action{
				// Default action with ID "default" (v1 normalize shape):
				// bare plugin name, even though Label is set.
				{ID: "default", Label: "notifier", Entrypoint: "bin/a"},
				// Non-empty label → plugin-name prefix + label.
				{ID: "send-dm", Label: "Send DM", Entrypoint: "bin/b"},
				// Empty label → plugin:action.
				{ID: "purge-cache", Entrypoint: "bin/c"},
			},
		}},
		{Name: "deploy", Manifest: &manifest.Manifest{
			Actions: []manifest.Action{
				// Default action with a non-"default" ID keeps the normal rules.
				{ID: "staging", Entrypoint: "bin/d"},
			},
		}},
	}

	want := []string{
		"notifier",
		"notifier: Send DM",
		"notifier:purge-cache",
		"deploy:staging",
	}
	actions := PluginActions(entries, nil)
	if len(actions) != len(want) {
		t.Fatalf("len(actions) = %d, want %d", len(actions), len(want))
	}
	for i, a := range actions {
		if a.Label != want[i] {
			t.Errorf("actions[%d].Label = %q, want %q", i, a.Label, want[i])
		}
	}
}

func TestPluginActions_Empty(t *testing.T) {
	actions := PluginActions(nil, nil)
	if len(actions) != 0 {
		t.Fatalf("len(actions) = %d, want 0", len(actions))
	}
}

func TestPluginActions_SkipsEntriesWithoutActions(t *testing.T) {
	entries := []plugin.Entry{
		{Name: "broken"}, // Manifest nil (StateBroken)
		{Name: "no-actions", Manifest: &manifest.Manifest{}}, // e.g. release_asset install
		{Name: "ok", Manifest: singleActionManifest("ok", "")},
	}
	actions := PluginActions(entries, nil)
	if len(actions) != 1 {
		t.Fatalf("len(actions) = %d, want 1 (only the entry with actions)", len(actions))
	}
	if actions[0].ID != "plugin:ok:default" {
		t.Errorf("actions[0].ID = %q, want plugin:ok:default", actions[0].ID)
	}
}

func TestPluginActions_NoPluginKeysMeansNoShortcut(t *testing.T) {
	entries := []plugin.Entry{
		{Name: "notifier", Manifest: singleActionManifest("notifier", "")},
	}
	for _, pluginKeys := range []map[string]map[string][]string{
		nil,
		{},
		{"notifier": nil},
		{"notifier": {}},
	} {
		for _, a := range PluginActions(entries, pluginKeys) {
			if a.Shortcut != "" {
				t.Errorf("pluginKeys=%v: Shortcut = %q, want empty", pluginKeys, a.Shortcut)
			}
		}
	}
}

func TestPluginActions_ShortcutResolution(t *testing.T) {
	entries := []plugin.Entry{
		{Name: "notifier", Manifest: &manifest.Manifest{
			Actions: []manifest.Action{
				{ID: "default", Label: "notifier", Entrypoint: "bin/a"},
				{ID: "send-dm", Entrypoint: "bin/b"}, // action-level binding only
				{ID: "unbound", Entrypoint: "bin/c"}, // no binding
			},
		}},
		{Name: "worktree-cleanup", Manifest: singleActionManifest("worktree-cleanup", "")},
	}
	pluginKeys := map[string]map[string][]string{
		"notifier": {
			"send-dm": {"M-d"},
		},
		"worktree-cleanup": {
			"default": {"M-w", "M-c"}, // multiple keys — first is used
		},
	}

	actions := PluginActions(entries, pluginKeys)
	if len(actions) != 4 {
		t.Fatalf("len(actions) = %d, want 4", len(actions))
	}

	want := map[string]string{
		"plugin:notifier:default":         "",
		"plugin:notifier:send-dm":         FormatKeyHint("M-d"),
		"plugin:notifier:unbound":         "",
		"plugin:worktree-cleanup:default": FormatKeyHint("M-w"),
	}
	for _, a := range actions {
		if a.Shortcut != want[a.ID] {
			t.Errorf("%s: Shortcut = %q, want %q", a.ID, a.Shortcut, want[a.ID])
		}
	}
}
