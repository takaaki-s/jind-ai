package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PluginBindingHint is a single plugin-action-shortcut line displayed in the
// help popup's Plugins section. Callers pre-format KeyHint via
// action.FormatKeyHint — the help popup itself is presentation-only.
type PluginBindingHint struct {
	KeyHint string // e.g. "Alt+N" (already formatted for display)
	Name    string // plugin + action, e.g. "notifier / send-dm"
}

// HelpModel is a standalone Bubble Tea model for displaying keyboard shortcuts.
// Designed to run inside a tmux popup and exits on any key press.
type HelpModel struct {
	keys                 KeyMap
	detachKeyHint        string
	actionPanelKeyHint   string
	sessionFilterKeyHint string
	pluginHints          []PluginBindingHint
	width                int
	height               int
}

// NewHelpModel creates a new HelpModel with the given KeyMap and outer-tmux
// binding hints (detach, action panel, session filter popup). pluginHints
// lists per-plugin shortcuts; pass nil / empty when no plugin bindings are
// configured — the Plugins section is then omitted entirely.
func NewHelpModel(keys KeyMap, detachKeyHint, actionPanelKeyHint, sessionFilterKeyHint string, pluginHints []PluginBindingHint) HelpModel {
	return HelpModel{
		keys:                 keys,
		detachKeyHint:        detachKeyHint,
		actionPanelKeyHint:   actionPanelKeyHint,
		sessionFilterKeyHint: sessionFilterKeyHint,
		pluginHints:          pluginHints,
	}
}

func (m HelpModel) Init() tea.Cmd {
	return nil
}

func (m HelpModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		return m, tea.Quit
	}
	return m, nil
}

func (m HelpModel) View() string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(primaryColor)
	keyStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255"))
	descStyle := lipgloss.NewStyle().Foreground(secondaryColor)

	k := m.keys
	var b strings.Builder

	b.WriteString(sectionStyle.Render("Keyboard Shortcuts"))
	b.WriteString("\n\n")

	b.WriteString(sectionStyle.Render("Navigation"))
	b.WriteString("\n")
	writeBinding(&b, keyStyle, descStyle, k.Up)
	writeBinding(&b, keyStyle, descStyle, k.Down)
	writeBinding(&b, keyStyle, descStyle, k.PrevPage)
	writeBinding(&b, keyStyle, descStyle, k.NextPage)
	b.WriteString("\n")

	b.WriteString(sectionStyle.Render("Actions"))
	b.WriteString("\n")
	writeBinding(&b, keyStyle, descStyle, k.Enter)
	writeBinding(&b, keyStyle, descStyle, k.New)
	writeBinding(&b, keyStyle, descStyle, k.Kill)
	writeBinding(&b, keyStyle, descStyle, k.Delete)
	writeBinding(&b, keyStyle, descStyle, k.Refresh)
	writeBinding(&b, keyStyle, descStyle, k.Vscode)
	b.WriteString("\n")

	b.WriteString(sectionStyle.Render("General"))
	b.WriteString("\n")
	writeBinding(&b, keyStyle, descStyle, k.Quit)
	writeShortcut(&b, keyStyle, descStyle, m.detachKeyHint, "return to TUI pane")
	writeBinding(&b, keyStyle, descStyle, k.Help)
	if m.actionPanelKeyHint != "" {
		writeShortcut(&b, keyStyle, descStyle, m.actionPanelKeyHint, "open action palette")
	}
	if m.sessionFilterKeyHint != "" {
		writeShortcut(&b, keyStyle, descStyle, m.sessionFilterKeyHint, "session filter (fuzzy)")
	}
	b.WriteString("\n")

	if len(m.pluginHints) > 0 {
		b.WriteString(sectionStyle.Render("Plugins"))
		b.WriteString("\n")
		for _, ph := range m.pluginHints {
			writeShortcut(&b, keyStyle, descStyle, ph.KeyHint, "plugin: "+ph.Name)
		}
		b.WriteString("\n")
	}

	b.WriteString(helpStyle.Render("Press any key to close"))

	return b.String()
}

// writeBinding writes a shortcut line from a key.Binding's Help() metadata.
func writeBinding(b *strings.Builder, keyStyle, descStyle lipgloss.Style, binding key.Binding) {
	h := binding.Help()
	writeShortcut(b, keyStyle, descStyle, h.Key, h.Desc)
}

// writeShortcut writes a single shortcut line with aligned columns.
func writeShortcut(b *strings.Builder, keyStyle, descStyle lipgloss.Style, k, desc string) {
	const keyColWidth = 14
	padded := k
	if len(k) < keyColWidth {
		padded = k + strings.Repeat(" ", keyColWidth-len(k))
	}
	b.WriteString("  ")
	b.WriteString(keyStyle.Render(padded))
	b.WriteString(descStyle.Render(desc))
	b.WriteString("\n")
}
