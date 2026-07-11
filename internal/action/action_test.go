package action

import (
	"testing"

	"github.com/takaaki-s/jind-ai/internal/plugin"
)

func TestCoreActions_LabelStable(t *testing.T) {
	want := map[string]string{
		IDNew:           "new session",
		IDKill:          "kill session",
		IDDelete:        "delete session",
		IDRefresh:       "refresh list",
		IDVscode:        "open in vscode",
		IDNotifications: "notification history",
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
		New:           []string{"n", "N"},
		Kill:          []string{"x", "X"},
		Delete:        []string{"d", "D"},
		Refresh:       []string{"r"},
		Vscode:        []string{"v"},
		Notifications: []string{"!"},
		Help:          []string{"?"},
		TogglePane:    []string{"M-\\"},
		Search:        []string{"M-f"},
	}
	want := map[string]string{
		IDNew:           "n",
		IDKill:          "x",
		IDDelete:        "d",
		IDRefresh:       "r",
		IDVscode:        "v",
		IDNotifications: "!",
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
		IDNotifications: false,
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

func TestPluginActions_Basic(t *testing.T) {
	entries := []plugin.Entry{
		{Name: "notifier-slack"},
		{Name: "vscode-open"},
	}

	actions := PluginActions(entries)
	if len(actions) != len(entries) {
		t.Fatalf("len(actions) = %d, want %d", len(actions), len(entries))
	}
	for i, a := range actions {
		if a.Kind != KindPlugin {
			t.Errorf("actions[%d].Kind = %v, want KindPlugin", i, a.Kind)
		}
		if a.Label != entries[i].Name {
			t.Errorf("actions[%d].Label = %q, want %q", i, a.Label, entries[i].Name)
		}
		if want := PluginIDPrefix + entries[i].Name; a.ID != want {
			t.Errorf("actions[%d].ID = %q, want %q", i, a.ID, want)
		}
	}
}

func TestPluginActions_Empty(t *testing.T) {
	actions := PluginActions(nil)
	if len(actions) != 0 {
		t.Fatalf("len(actions) = %d, want 0", len(actions))
	}
}
