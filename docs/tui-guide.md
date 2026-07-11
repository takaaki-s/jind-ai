# TUI Guide

## Architecture

BubbleTea (Elm Architecture) based TUI.
The main screen (session list) is handled by `model.go`, while the create form, help, and notification history are launched as independent processes via tmux popup.

```
internal/tui/
├─ model.go       ... Main Model (session list), Update(), View() (~1430 lines)
├─ createform.go  ... Session create form (for popup, ~540 lines)
├─ dirpicker.go   ... Directory picker (used within createform, ~730 lines)
├─ notifyview.go  ... Notification history view (for popup, ~180 lines)
├─ helpview.go    ... Help view (for popup, ~100 lines)
└─ styles.go      ... lipgloss style definitions (Tokyo Night color scheme)

cmd/jin/cmd/
├─ create_popup.go  ... jin create-popup (Hidden) → launches CreateFormModel
├─ help_popup.go    ... jin help-popup (Hidden)   → launches HelpModel
└─ notify_popup.go  ... jin notify-popup (Hidden) → launches NotifyModel
```

## Model Structure

`Model` in `model.go` holds the state of the session list screen:
- Session list + cursor position + pagination
- Search mode (filtering)
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
        // Mode checks: confirmDelete/confirmKill/searching etc.
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
- **Search mode**: Triggered by `/` key, incremental filtering by session name
- **Confirmation dialog**: Shows confirmation message in help line for Kill/Delete

### Popup (launched as independent process via tmux popup)

- **Create form**: `CreateFormModel` in `createform.go` (WorkDir → Agent → Fleet → Worktree; Agent step is skipped when only one adapter is registered)
- **Help**: `HelpModel` in `helpview.go` (keybind list)
- **Notification history**: `NotifyModel` in `notifyview.go` (notification list + session selection)

After popup completion, results are returned to the parent TUI via environment variables (`JIN_CREATED_SESSION`, `JIN_NOTIFY_SESSION`). The parent TUI detects them during tickMsg polling.

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
`action_panel` (default `M-p`) is another outer-tmux root binding, same shape as `toggle_pane` below — see [Action Palette](#action-palette).

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
might want to trigger from the TUI: the 8 built-in actions (new / kill /
delete / refresh / vscode / notifications / help / toggle sidebar) plus any
`plugin:*` action from installed plugins, all in one substring-searchable
list. Like `toggle_pane`, it's bound at the outer tmux (`jin-mgr`) root key
table, so it can be launched from either the session list (left) or an
attached agent (right) pane.

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
