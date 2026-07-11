// Package action defines the action palette's data model: the built-in
// ("core") actions the TUI exposes plus the dynamic set contributed by
// installed plugins. It holds no behavior — Run is dispatched by ID from the
// parent TUI so both direct key presses and palette selection share one code
// path.
package action

import "github.com/takaaki-s/jind-ai/internal/plugin"

// Kind classifies an Action's origin. Core actions are built into the TUI;
// Plugin actions come from installed plugin manifests.
type Kind int

const (
	KindCore Kind = iota
	KindPlugin
)

// Action is one palette row.
type Action struct {
	ID           string // "core:new" | "plugin:notifier-slack"
	Kind         Kind
	Label        string // display + search text
	Description  string // optional secondary search text
	Shortcut     string // right-column hint, empty for plugin or unbound core
	NeedsSession bool   // core: whether cursor session is required
}

// Core action IDs. Kept as exported consts so parent TUI dispatch and popup
// registry cannot drift.
const (
	IDNew           = "core:new"
	IDKill          = "core:kill"
	IDDelete        = "core:delete"
	IDRefresh       = "core:refresh"
	IDVscode        = "core:vscode"
	IDNotifications = "core:notifications"
	IDHelp          = "core:help"
	IDTogglePane    = "core:toggle-pane"
	IDSessionFilter = "core:session-filter"
)

const (
	CoreIDPrefix   = "core:"
	PluginIDPrefix = "plugin:"
)

// KeyBindings is a narrow subset of config.KeybindingsConfig used to resolve
// Shortcut. Kept as an internal struct so this package does not import
// config (avoiding a cycle if config later needs action).
type KeyBindings struct {
	New, Kill, Delete, Refresh, Vscode, Notifications, Help, TogglePane, Search []string
}

// CoreActions returns the built-in action set, with Shortcut resolved from
// the caller's key config. Passing a zero KeyBindings returns actions with
// empty Shortcut fields — safe for tests that don't need shortcut display.
func CoreActions(kb KeyBindings) []Action {
	first := func(keys []string) string {
		if len(keys) == 0 {
			return ""
		}
		return FormatKeyHint(keys[0])
	}
	return []Action{
		{ID: IDNew, Kind: KindCore, Label: "new session", Shortcut: first(kb.New)},
		{ID: IDKill, Kind: KindCore, Label: "kill session", Shortcut: first(kb.Kill), NeedsSession: true},
		{ID: IDDelete, Kind: KindCore, Label: "delete session", Shortcut: first(kb.Delete), NeedsSession: true},
		{ID: IDRefresh, Kind: KindCore, Label: "refresh list", Shortcut: first(kb.Refresh)},
		{ID: IDVscode, Kind: KindCore, Label: "open in vscode", Shortcut: first(kb.Vscode), NeedsSession: true},
		{ID: IDNotifications, Kind: KindCore, Label: "notification history", Shortcut: first(kb.Notifications)},
		{ID: IDHelp, Kind: KindCore, Label: "shortcuts help", Shortcut: first(kb.Help)},
		{ID: IDSessionFilter, Kind: KindCore, Label: "session filter", Description: "Fuzzy-filter and switch to a session", Shortcut: first(kb.Search)},
		{ID: IDTogglePane, Kind: KindCore, Label: "toggle sidebar", Shortcut: first(kb.TogglePane)},
	}
}

// PluginActions maps enabled plugin entries to palette actions. Callers pass
// the result of Registry.Runnable, which already filters to StateEnabled
// entries.
func PluginActions(entries []plugin.Entry) []Action {
	out := make([]Action, 0, len(entries))
	for _, e := range entries {
		out = append(out, Action{
			ID:    PluginIDPrefix + e.Name,
			Kind:  KindPlugin,
			Label: e.Name,
		})
	}
	return out
}
