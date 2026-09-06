# TUI Guide

## Architecture

BubbleTea (Elm Architecture) based TUI.
The main screen (session list) is handled by `model.go`, while the create form, help, and switch-session picker are launched as independent processes via tmux popup.

```
internal/tui/
├─ model.go               ... Main Model (session list), Update(), View() (~1430 lines)
├─ createform.go          ... Session create form (for popup, ~540 lines)
├─ dirpicker.go           ... Directory picker (used within createform, ~730 lines)
├─ helpview.go            ... Help view (for popup, ~100 lines)
├─ session_filter_model.go ... Switch-session picker (for popup, sahilm/fuzzy)
├─ confirm_popup_model.go ... Kill/Delete confirmation dialog (for popup)
└─ styles.go              ... lipgloss style definitions (Tokyo Night color scheme)

cmd/jin/cmd/
├─ create_popup.go          ... jin create-popup (Hidden) → launches CreateFormModel
├─ help_popup.go            ... jin help-popup (Hidden)   → launches HelpModel
├─ session_filter_popup.go  ... jin session-filter-popup (Hidden) → launches SessionFilterModel
└─ confirm_popup.go         ... jin confirm-popup (Hidden) → launches ConfirmPopupModel
```

## Model Structure

`Model` in `model.go` holds the state of the session list screen:
- Session list + cursor position + pagination
- daemon.Client (for IPC communication)
- tmux.Client (for popup launch and pane control)
- Polling timer (tickMsg)

The left pane is a master-detail layout drawn entirely with lipgloss (no tmux
split — a split would mean a second process and IPC). It has three regions with
fixed budgets: a count header (`listHeaderLines`), the scrollable session list
(`sessionRowHeight` rows per session — the name on the first, repo and branch
on the second), and a detail pane for the session under the cursor
(`detailPaneLines`). `contentAreaLines()` is the parent budget
and `listAreaLines()` is what every scroll clamp and mouse hit-test measures
against — not `contentAreaLines()`. The detail pane is dropped whole rather
than shrunk when the list would fall below `minListLines`, which is what keeps
those heights constant and the geometry testable.

The detail pane follows the **cursor**, while the right-hand tmux pane follows
`currentSessionID`; the two are deliberately orthogonal, so moving the cursor
never switches panes.

Scrolling comes in two kinds and they align differently. Scroll-only input
(wheel, PageUp/PageDown) goes through `scrollBy`, which pulls the landing back
to the top of the session row it fell inside, so the list never opens on a
row's second line. Cursor-driven scrolling (`adjustScrollForCursor`) does
**not** align: it owes the cursor's row whole, and pulling back to a row top
would push that row's last line off the fold — the cursor's row wins. The two
exceptions where `scrollBy` also leaves the offset alone are the last page
(aligning there would put the final row out of reach for good) and a step
short enough that the pull-back would cancel it.

## Update/View Pattern

```go
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
    switch msg := msg.(type) {
    case tea.KeyMsg:
        // Delegates to updateListMode()
    case tickMsg:
        // Periodic polling (daemon.Client.List())
        // Detect popup completion (via environment variables)
    case sessionsMsg:
        // Update session list
    }
}

func (m Model) View() string {
    // processingMsg != "" → renderProcessingView()
    // Normal → renderListContent() + the chrome below it (`helpChromeLines`:
    //          a rule, then the help line)
}
```

## View Modes

### Main Screen (within model.go)

- **Session list**: Default screen, status display + pagination

### Popup (launched as independent process via tmux popup)

- **Create form**: `CreateFormModel` in `createform.go` (WorkDir → Agent → Fleet → Worktree; Agent step is skipped when only one adapter is registered)
- **Help**: `HelpModel` in `helpview.go` (keybind list)
- **Switch session**: `SessionFilterModel` in `session_filter_model.go` (fuzzy session picker, see [Switch Session Popup](#switch-session-popup) below)
- **Kill/Delete confirmation**: `ConfirmPopupModel` in `confirm_popup_model.go`. A popup rather than an in-pane dialog because a popup owns keyboard focus while open — when the action palette launched the action, focus sits on the display pane, so an in-pane prompt could not be answered without switching panes first. The parent passes the prompt in through `JIN_CONFIRM_MODE` / `JIN_CONFIRM_TARGET_ID` / `JIN_CONFIRM_TARGET_DESC` and reads the answer back from `JIN_CONFIRM_RESULT`; dismissing the popup writes nothing, so no answer means nothing happens. Clearing the handshake is the parent's job, at both ends: it wipes all four keys before writing a new prompt, and clears whatever a startup env snapshot still shows — the outer tmux server outlives the TUI process, so an answer left behind would otherwise be replayed against a different target. The write is fail-closed for the same reason: a failure at any point leaves the whole handshake empty rather than half-written, and the popup is not opened at all, since a prompt that cannot be described in full must not be shown.

After popup completion, core popups return results to the parent TUI via environment variables (`JIN_CREATED_SESSION`, `JIN_FOCUS_SESSION`). `JIN_NOTIFY_SESSION` is set by `jin session focus` — invoked from external plugins such as `jind-ai-notifier` — and consumed via the same env-tick polling path. The parent TUI detects them during tickMsg polling.

## Styling

- lipgloss styles are defined in `styles.go` (Tokyo Night color scheme)
- Do not use raw ANSI codes
- Specify colors with lipgloss.Color()

## Session Row Layout

A session row is exactly `sessionRowHeight` (2) lines over a fixed
`sessionRowLead` prefix; the column figure is on `renderSession` in
`model.go`, next to the code that draws it.

Every scroll offset, viewport clamp and mouse hit-test is derived from
`sessionRowHeight`, and every name column from `sessionRowLead`. A change to
either moves both `renderSession` and the geometry helpers together — the
tests in `model_test.go` are written against the constants rather than against
literals so they follow.

The list is also partitioned so that, inside each fleet, sessions holding an
unseen completion come first (`partitionUnseenFirst`, applied where the poll's
result is installed). Because that reorders rows under the cursor on a
two-second poll, the `sessionsMsg` branch captures the cursor's session ID
before installing the new slice and restores the cursor by ID afterwards. The
startup `pendingCursorRestore` keeps its precedence over that.

## Adding a New Popup

1. Create a new `.go` file in `internal/tui/` and implement an independent `tea.Model`
2. Create `xxx_popup.go` in `cmd/jin/cmd/` (register as a Hidden command)
3. Use `tea.NewProgram()` inside the popup to run as an independent BubbleTea program
4. If returning results via environment variables, add detection logic in `model.go`'s `tick()`
5. Add a keybind for popup launch in `model.go`'s `updateListMode()`
6. Refer to existing create_popup.go / help_popup.go as patterns

## Keybindings

Keybindings are retrieved from `config.GetKeybindings()`.
Default values are defined in `config.DefaultKeybindings()`.
Users can customize them in the `keybindings` section of `~/.config/jind-ai/config.yaml` (or wherever `$XDG_CONFIG_HOME/jind-ai/config.yaml` resolves to).
`action_panel` (default `M-p`) and `search` (default `M-f`) are two more
outer-tmux root bindings, same shape as `toggle_pane` below — see
[Action Palette](#action-palette) and [Switch Session Popup](#switch-session-popup).

### Outer tmux — sidebar toggle

The outer tmux session (`jin-mgr`) binds `toggle_pane` keys at the root key
table to zoom/unzoom the display (right) pane. Zooming hides the session list
so you get the full width for focus mode; on narrow terminals the same key
reveals the session list from a collapsed state.

Defaults to `M-\` (Alt+Backslash). Override in `config.yaml`:

```yaml
keybindings:
  toggle_pane: ["M-b"]              # use Alt+b instead
  # toggle_pane: []                  # disable entirely (no bind-key issued)
  # toggle_pane: ["M-\\", "M-b"]     # bind multiple keys
```

Keys use tmux `bind-key` notation (`M-` = Alt, `C-` = Ctrl). The binding is
applied on `jin ui` startup and re-applied on reattach, so it survives outer
tmux server restarts.

Note: an omitted `toggle_pane` field uses the default, while an explicit empty
list (`toggle_pane: []`) disables the feature — the nil/empty distinction is
intentional.

## Action Palette

The action palette is a searchable popup that unifies every action a user
might want to trigger from the TUI: the 9 built-in actions (new / kill /
delete / refresh / vscode / mark completion seen / help / switch session /
toggle sidebar) plus
any `plugin:*` action from installed plugins, all in one
fuzzy-searchable list (via [sahilm/fuzzy](https://github.com/sahilm/fuzzy),
same engine as the switch-session picker — matched runes are underlined in
the label column). Like `toggle_pane`, it's bound at the outer tmux
(`jin-mgr`) root key table, so it can be launched from either the session
list (left) or an attached agent (right) pane.

The default trigger is `M-p` (Alt+p). Once open, each row shows its Label
alongside a Shortcut column — this doubles as a live reference for the
direct keys documented above, so users don't need to keep checking this doc
once they've learned a shortcut from the palette itself.

"mark completion seen" is the one core action with no key of its own — see
`CoreActions` for why — and the only one that refetches the list rather than
waiting for the next poll.

Override or disable the trigger the same way as `toggle_pane`:

```yaml
keybindings:
  action_panel: ["M-x"]  # rebind to Alt+x
  # action_panel: []       # disable entirely (no bind-key issued)
```

## Switch Session Popup

The switch-session picker is a fuzzy-search popup for jumping straight to a
session: press `M-f` (default, configurable via `keybindings.search`), type
a few characters, and hit `Enter` to attach. It replaced the old inline
substring filter that used to live directly in the session list — like
`action_panel`, it's bound at the outer tmux (`jin-mgr`) root key table, so
`M-f` opens the popup from either the session list (left) or an attached
agent (right) pane, not just from the list itself. It is also reachable via
the action palette (`M-p` → "switch session"), so a shortcut isn't required.

The default changed from `/` to `M-f` (Alt+f) because a bare-letter binding
at the outer tmux root also captures `/` typed in the display pane, breaking
agent slash-commands (Claude Code `/help`, less/vim `/search`, etc.). To
restore the old behavior, set `keybindings.search: ["/"]` explicitly.

- **Engine**: [sahilm/fuzzy](https://github.com/sahilm/fuzzy) subsequence
  matching with smart-case and score-based ranking (`SessionFilterModel` in
  `internal/tui/session_filter_model.go`). An empty query shows every
  session in the daemon-provided order instead of ranking.
- **Matched fields**: `Description`, `WorkDir`, `CurrentWorkDir`,
  `CurrentBranch`, `Fleet`, and `AgentKind` — six fields per session, joined
  into one haystack per row. Matched characters are underlined in the row.
- **Selection**: `Enter` sets the picked session as the parent TUI's focus
  and attaches immediately (the right pane switches to that session on the
  next poll, ~250ms). `Esc` (or `Ctrl+C`) closes the popup without changing
  parent TUI state.
- **Navigation**: `↑`/`↓` or `Ctrl+P`/`Ctrl+N` move the cursor.

Override or disable the trigger the same way as `toggle_pane`:

```yaml
keybindings:
  search: ["ctrl+p"]  # rebind to Ctrl+p
  # search: []          # disable entirely (no bind-key issued)
```

## Per-plugin Shortcuts

Any installed plugin's action can be bound to an outer-tmux root key, so
pressing the key from either pane fires `jin plugin run <name> <action>`
immediately — no action palette detour, no CLI. The 4th outer-tmux root
binding after `toggle_pane` / `action_panel` / `search`, with the same
left/right-pane symmetry.

Unlike the core three, there is **no default binding** — the user opts in
per plugin×action under `keybindings.plugins.<name>.actions.<id>.keys`:

```yaml
keybindings:
  plugins:
    notifier:
      actions:
        default:
          keys: ["M-n"]                        # single key on the default action
        send-dm:
          keys: ["M-d", "C-M-d"]               # multiple keys on a named action
    worktree-cleanup:
      actions:
        default:
          keys: ["M-w"]
    # slack-sync:
    #   actions:
    #     default:
    #       keys: []                            # explicit empty ⇒ no binding
```

The pre-0.8.0 shape (`keybindings.plugins.<name>.keys` at the plugin
level, no `actions:` nesting) is **rejected**: a single WARN per plugin
is logged at startup and that plugin's bindings are dropped. The TUI
still starts — config drift never blocks it — but the shortcuts stay
silent until the user migrates to the new shape. See
[gotchas.md](gotchas.md) for the deprecated-shape entry.

Each key must be a **modifier-prefixed** binding. Both notations are
accepted and normalized to tmux `bind-key` form at load time:

- tmux style — `M-n`, `C-f`, `S-Tab`, `C-M-p`
- "+" style — `alt+n`, `ctrl+f`, `shift+tab`, `ctrl+alt+p`

Modifier names are matched case-insensitively; the trailing key token is
preserved verbatim so tmux's own case sensitivity (e.g. `M-p` vs `M-P`) is
not smoothed away. Symbols like `M-\` stay as-is. Bare-letter keys are
captured before reaching the display pane and would starve the agent of
input — the same constraint the other outer-tmux bindings enforce.

The `keybindings` block ends up mixing the two styles: inner-TUI keys
(`quit`, `detach`, form submit) always use the bubbletea "+" form
(`ctrl+c`, `ctrl+]`) because bubbletea itself only understands that
notation; outer-tmux keys (`toggle_pane`, `action_panel`, `search`,
`plugins.*.actions.*.keys`) travel through the normalizer above, so pick
whichever form you find easier to read.

The tmux command issued is
`run-shell -b '<jin>' plugin run <name> <action> >/dev/null 2>&1`, which returns
immediately (the daemon dispatches the plugin asynchronously). It resolves which
jin to reach from the outer tmux session environment, which `jin ui` rewrites —
along with these bindings — on every start. Note the consequence of `-b` plus
the redirect: this is the one caller of `jin plugin run` whose failures reach no
one (`internal/plugin/dispatcher.go` has what survives).

Even a plugin with only a default action gets the
action ID rendered explicitly — the CLI accepts it and the daemon
resolves it back to `actions[0]`. If the action wants to render a popup
it does so itself via `jin pane popup --here` — the shortcut path is
deliberately transparent to the plugin's UI, matching how the action
palette invokes plugins today.

**Bindings appear in two other places automatically:**

- The action palette (`M-p`) shows one row per plugin action, **except**
  actions declared with `listener: true` in the manifest — those are
  event-only endpoints and stay hidden from the palette on purpose (a
  plugin author uses the flag to split an internal listener from the
  user-facing action so the palette does not gain a row that does
  nothing when clicked). The Shortcut column carries the first
  configured key for that action, mirroring how core actions display
  theirs. When a default action's ID is `default` and it has no label,
  the row displays the bare plugin name (preserving the pre-0.8.0 look
  for single-action plugins).
- The help popup (`?`) grows a `Plugins` section listing every bound
  plugin×action as its own line (`plugin: notifier / send-dm`). The
  section is hidden entirely when no plugin bindings are configured, so
  the help view stays quiet for default installs.

**Edge cases** (handled with fail-open policies, matching `PluginsConfig.Disabled`):

- Plugin listed in config but not installed → bind-key is skipped and one
  line is logged (`plugin key binding skipped: <name> not installed or
  disabled`). TUI startup is never blocked by config vs. installed set
  drift.
- Action ID configured but not in the plugin's current manifest (e.g. a
  v2 plugin was edited to remove an action) → the tmux key still binds
  and fires `jin plugin run <name> <action>`, but the daemon rejects the
  run with `plugin <name> has no action "<id>"`. Removing the stale
  config entry is left to the user; the TUI does not warn at startup
  because config-driven flexibility is preferred over speculative checks.
- Same key bound to a core action (`M-p` etc.) → a collision warning is
  logged. Both bindings are issued and tmux's last-write-wins semantics
  decide which fires; the plugin binding is typically issued last (see
  `cmd/jin/cmd/tui.go`), so it wins when both are eligible.
- Cursor session is **not** passed to the plugin — the shortcut path
  mirrors action-palette-launched plugins (global action, `--session` empty).
  Session-aware invocation is left for a future extension.

## Popup Sizes

Every tmux popup opened by the TUI has a configurable width and height,
specified as percentages of the outer tmux client area. Sizes are
`int` values in the range 1-100; out-of-range values in user config are
warned and fall back to the default. See `Manager.GetPopupSize` in
`internal/config/config.go` for the resolver.

Defaults:

| Popup name       | Width | Height | Trigger                    |
|------------------|-------|--------|----------------------------|
| `create`         | 80    | 80     | `keybindings.new`          |
| `session_filter` | 70    | 70     | `keybindings.search`       | <!-- switch-session picker; key name kept for backward compat -->
| `help`           | 60    | 60     | `keybindings.help`         |
| `action`         | 70    | 70     | `keybindings.action_panel` |
| `confirm`        | 48†   | 10†    | `keybindings.kill` / `keybindings.delete` |
| `plugin_default` | 70    | 70     | Plugin `jin pane popup --here` fallback |

† `confirm` is the one popup whose contents have fixed dimensions, so its
default is an absolute cell count rather than a percentage — a share of a wide
client would leave a six-line dialog swimming in empty space. Setting either
`popups.confirm` dimension switches the whole popup back to percentages (50/50
by default), which is also the automatic fallback on a client too small for
48x10, since tmux rejects an oversized absolute popup instead of shrinking it.

Override in `~/.config/jind-ai/config.yaml`:

```yaml
popups:
  create:         { width: 90, height: 90 }
  session_filter: { width: 80, height: 80 }
  help:           { width: 60, height: 60 }
  action:         { width: 70, height: 70 }
  confirm:        { width: 50, height: 50 }  # percent; overrides the 48x10 cell default
  plugin_default: { width: 70, height: 70 }
  plugins:
    my-notifier:  { width: 40, height: 20 }   # override per-plugin
```

Two delivery paths for the resolved size:

- **Inner path** (BubbleTea → `tmuxClient.DisplayPopup`): `create`,
  `session_filter`, `help`, `confirm` are opened from inside the TUI on each keypress.
  Config changes take effect the next time the popup opens — no TUI restart
  needed.
- **Outer path** (tmux `bind-key display-popup`): `action` and `session_filter`
  are also bound directly at the outer tmux (`jin-mgr`) root key table so
  they open even when focus is on the display pane. These bindings are
  written once at `jin ui` startup and are not re-issued when config changes.
  Restart `jin ui` for changes to `action` or `session_filter` outer-path
  sizes to reach tmux.

For plugin popups (`jin pane popup --here`), the priority chain is:
CLI flag > `JIN_PLUGIN_POPUP_*` env (dispatcher-resolved from user config >
manifest > `plugin_default`) > empty (tmux built-in default).
