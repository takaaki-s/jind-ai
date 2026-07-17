// Package action defines the action palette's data model: the built-in
// ("core") actions the TUI exposes plus the dynamic set contributed by
// installed plugins. It holds no behavior — Run is dispatched by ID from the
// parent TUI so both direct key presses and palette selection share one code
// path.
package action

import (
	"strings"

	"github.com/takaaki-s/jind-ai/internal/plugin"
	"github.com/takaaki-s/jind-ai/pkg/plugin/manifest"
)

// Kind classifies an Action's origin. Core actions are built into the TUI;
// Plugin actions come from installed plugin manifests.
type Kind int

const (
	KindCore Kind = iota
	KindPlugin
)

// Action is one palette row.
type Action struct {
	ID           string // "core:new" | "plugin:notifier:send-dm"
	Kind         Kind
	Label        string // display + search text
	Description  string // optional secondary search text
	Shortcut     string // right-column hint, empty when the action has no key bound
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
	IDHelp          = "core:help"
	IDTogglePane    = "core:toggle-pane"
	IDSessionFilter = "core:session-filter"
)

const (
	CoreIDPrefix = "core:"
	// PluginIDPrefix marks a plugin-contributed action ID. The full format
	// is three segments — "plugin:<name>:<action>" (see PluginActionID) —
	// so the prefix alone only classifies the ID's origin; use
	// ParsePluginActionID to extract the parts.
	PluginIDPrefix = "plugin:"
)

// PluginActionID builds the palette ID for one plugin action:
// "plugin:<name>:<action>".
func PluginActionID(pluginName, actionID string) string {
	return PluginIDPrefix + pluginName + ":" + actionID
}

// ParsePluginActionID splits a three-segment plugin action ID back into its
// plugin name and action ID. ok is false for anything else — a non-plugin
// ID, a legacy two-segment "plugin:<name>" ID, or empty segments — so
// callers can drop stale IDs (e.g. from a tmux env var written by an older
// binary) instead of dispatching them.
func ParsePluginActionID(id string) (pluginName, actionID string, ok bool) {
	rest, found := strings.CutPrefix(id, PluginIDPrefix)
	if !found {
		return "", "", false
	}
	pluginName, actionID, found = strings.Cut(rest, ":")
	if !found || pluginName == "" || actionID == "" {
		return "", "", false
	}
	return pluginName, actionID, true
}

// KeyBindings is a narrow subset of config.KeybindingsConfig used to resolve
// Shortcut. Kept as an internal struct so this package does not import
// config (avoiding a cycle if config later needs action).
type KeyBindings struct {
	New, Kill, Delete, Refresh, Vscode, Help, TogglePane, Search []string
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
		{ID: IDHelp, Kind: KindCore, Label: "shortcuts help", Shortcut: first(kb.Help)},
		{ID: IDSessionFilter, Kind: KindCore, Label: "session filter", Description: "Fuzzy-filter and switch to a session", Shortcut: first(kb.Search)},
		{ID: IDTogglePane, Kind: KindCore, Label: "toggle sidebar", Shortcut: first(kb.TogglePane)},
	}
}

// PluginActions fans enabled plugin entries out to one palette row per
// declared action. Callers pass the result of Registry.Runnable, which
// already filters to StateEnabled entries; entries without a manifest (or
// with no actions — e.g. release_asset installs) contribute no rows.
// Description (when the manifest declares one) rides along so the palette
// fuzzy haystack treats plugin rows like core rows. pluginKeys maps plugin
// name → action ID → tmux keys (the shape returned by
// config.Manager.GetPluginKeybindings). When an action has one or more keys,
// the first is formatted via FormatKeyHint and shown in the Shortcut column —
// the same `first(keys)` convention CoreActions uses. A nil / empty
// pluginKeys map produces rows with empty Shortcut.
func PluginActions(entries []plugin.Entry, pluginKeys map[string]map[string][]string) []Action {
	out := make([]Action, 0, len(entries))
	for _, e := range entries {
		if e.Manifest == nil {
			continue
		}
		for i, act := range e.Manifest.Actions {
			a := Action{
				ID:          PluginActionID(e.Name, act.ID),
				Kind:        KindPlugin,
				Label:       pluginActionLabel(e.Name, i == 0, act),
				Description: e.Manifest.Description,
			}
			if keys := pluginKeys[e.Name][act.ID]; len(keys) > 0 {
				a.Shortcut = FormatKeyHint(keys[0])
			}
			out = append(out, a)
		}
	}
	return out
}

// pluginActionLabel decides a plugin action's palette label:
//   - the default action (Actions[0]) with ID "default" shows the bare
//     plugin name — v1 manifests normalize to exactly that shape, so
//     single-action plugins keep their pre-multi-action look;
//   - a non-empty Label is shown with the plugin name as prefix;
//   - otherwise the label is "<plugin>:<action>".
func pluginActionLabel(pluginName string, isDefault bool, act manifest.Action) string {
	if isDefault && act.ID == "default" {
		return pluginName
	}
	if act.Label != "" {
		return pluginName + ": " + act.Label
	}
	return pluginName + ":" + act.ID
}
