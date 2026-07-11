# TUI Guide

## Architecture

BubbleTea (Elm Architecture) based TUI.
The main screen (session list) is handled by `model.go`, while the create form, help, notification history, and session filter are launched as independent processes via tmux popup.

```
internal/tui/
├─ model.go               ... Main Model (session list), Update(), View() (~1430 lines)
├─ createform.go          ... Session create form (for popup, ~540 lines)
├─ dirpicker.go           ... Directory picker (used within createform, ~730 lines)
├─ notifyview.go          ... Notification history view (for popup, ~180 lines)
├─ helpview.go            ... Help view (for popup, ~100 lines)
├─ session_filter_model.go ... Session filter picker (for popup, sahilm/fuzzy)
└─ styles.go              ... lipgloss style definitions (Tokyo Night color scheme)

cmd/jin/cmd/
├─ create_popup.go          ... jin create-popup (Hidden) → launches CreateFormModel
├─ help_popup.go            ... jin help-popup (Hidden)   → launches HelpModel
├─ notify_popup.go          ... jin notify-popup (Hidden) → launches NotifyModel
└─ session_filter_popup.go  ... jin session-filter-popup (Hidden) → launches SessionFilterModel
```

## Model Structure

`Model` in `model.go` holds the state of the session list screen:
- Session list + cursor position + pagination
- Confirmation dialog (Kill/Delete)
- daemon.Client (for IPC communication)
- tmux.Client (for popup launch and pane control)
- Polling timer (tickMsg)

## Update/View Pattern

```go
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
    switch msg := msg.(type) {
    case tea.KeyMsg:
        // Delegates to updateListMode()
        // Mode checks: confirmDelete/confirmKill etc.
    case tickMsg:
        // Periodic polling (daemon.Client.List())
        // Detect popup completion (via environment variables)
    case sessionsMsg:
        // Update session list
    }
}

func (m Model) View() string {
    // processingMsg != "" → renderProcessingView()
    // Normal → renderListContent() + renderHelpLine()
}
```

## View Modes

### Main Screen (within model.go)

- **Session list**: Default screen, status display + pagination
- **Confirmation dialog**: Shows confirmation message in help line for Kill/Delete

### Popup (launched as independent process via tmux popup)

- **Create form**: `CreateFormModel` in `createform.go` (WorkDir → Agent → Fleet → Worktree; Agent step is skipped when only one adapter is registered)
- **Help**: `HelpModel` in `helpview.go` (keybind list)
- **Notification history**: `NotifyModel` in `notifyview.go` (notification list + session selection)
- **Session filter**: `SessionFilterModel` in `session_filter_model.go` (fuzzy session picker, see [Session Filter Popup](#session-filter-popup) below)

After popup completion, results are returned to the parent TUI via environment variables (`JIN_CREATED_SESSION`, `JIN_NOTIFY_SESSION`, `JIN_FOCUS_SESSION`). The parent TUI detects them during tickMsg polling.

## Styling

- lipgloss styles are defined in `styles.go` (Tokyo Night color scheme)
- Do not use raw ANSI codes
- Specify colors with lipgloss.Color()

## Adding a New Popup

1. Create a new `.go` file in `internal/tui/` and implement an independent `tea.Model`
2. Create `xxx_popup.go` in `cmd/jin/cmd/` (register as a Hidden command)
3. Use `tea.NewProgram()` inside the popup to run as an independent BubbleTea program
4. If returning results via environment variables, add detection logic in `model.go`'s `tick()`
5. Add a keybind for popup launch in `model.go`'s `updateListMode()`
6. Refer to existing create_popup.go / help_popup.go / notify_popup.go as patterns

## Keybindings

Keybindings are retrieved from `config.GetKeybindings()`.
Default values are defined in `config.DefaultKeybindings()`.
Users can customize them in the `keybindings` section of `~/.config/jind-ai/config.yaml` (or wherever `$XDG_CONFIG_HOME/jind-ai/config.yaml` resolves to).
`action_panel` (default `M-p`) and `search` (default `M-f`) are two more
outer-tmux root bindings, same shape as `toggle_pane` below — see
[Action Palette](#action-palette) and [Session Filter Popup](#session-filter-popup).

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
delete / refresh / vscode / notifications / help / session filter / toggle
sidebar) plus any `plugin:*` action from installed plugins, all in one
fuzzy-searchable list (via [sahilm/fuzzy](https://github.com/sahilm/fuzzy),
same engine as the session filter popup — matched runes are underlined in
the label column). Like `toggle_pane`, it's bound at the outer tmux
(`jin-mgr`) root key table, so it can be launched from either the session
list (left) or an attached agent (right) pane.

The default trigger is `M-p` (Alt+p). Once open, each row shows its Label
alongside a Shortcut column — this doubles as a live reference for the
direct keys documented above, so users don't need to keep checking this doc
once they've learned a shortcut from the palette itself.

Override or disable the trigger the same way as `toggle_pane`:

```yaml
keybindings:
  action_panel: ["M-x"]  # rebind to Alt+x
  # action_panel: []       # disable entirely (no bind-key issued)
```

## Session Filter Popup

The session filter is a fuzzy-search popup for jumping straight to a
session: press `M-f` (default, configurable via `keybindings.search`), type
a few characters, and hit `Enter` to attach. It replaced the old inline
substring filter that used to live directly in the session list — like
`action_panel`, it's bound at the outer tmux (`jin-mgr`) root key table, so
`M-f` opens the popup from either the session list (left) or an attached
agent (right) pane, not just from the list itself. It is also reachable via
the action palette (`M-p` → "session filter"), so a shortcut isn't required.

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
