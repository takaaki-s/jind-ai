package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"
	"github.com/takaaki-s/jindaiko/internal/config"
)

func TestNewHelpModel(t *testing.T) {
	cfg := config.DefaultKeybindings()
	keys := NewKeyMap(cfg)
	detachHint := "Ctrl+]"

	m := NewHelpModel(keys, detachHint)

	// Verify keybindings are set (spot-check a few)
	if h := m.keys.Up.Help(); h.Key == "" || h.Desc == "" {
		t.Error("Up binding: Help().Key or Help().Desc is empty")
	}
	if h := m.keys.Quit.Help(); h.Key == "" || h.Desc == "" {
		t.Error("Quit binding: Help().Key or Help().Desc is empty")
	}
	if h := m.keys.New.Help(); h.Key == "" || h.Desc == "" {
		t.Error("New binding: Help().Key or Help().Desc is empty")
	}
	if h := m.keys.Enter.Help(); h.Key == "" || h.Desc == "" {
		t.Error("Enter binding: Help().Key or Help().Desc is empty")
	}
	if h := m.keys.Help.Help(); h.Key == "" || h.Desc == "" {
		t.Error("Help binding: Help().Key or Help().Desc is empty")
	}
	if m.detachKeyHint != detachHint {
		t.Errorf("detachKeyHint: got %q, want %q", m.detachKeyHint, detachHint)
	}
}

func TestNewHelpModel_EmptyKeyMap(t *testing.T) {
	// A zero-value KeyMap should still produce a valid model
	keys := KeyMap{}
	m := NewHelpModel(keys, "")

	if m.detachKeyHint != "" {
		t.Errorf("detachKeyHint: got %q, want empty", m.detachKeyHint)
	}
}

func TestHelpModel_View(t *testing.T) {
	cfg := config.DefaultKeybindings()
	keys := NewKeyMap(cfg)
	m := NewHelpModel(keys, "Ctrl+]")

	view := m.View()

	if view == "" {
		t.Fatal("View() returned empty string")
	}

	// Verify section headers are present
	for _, section := range []string{"Keyboard Shortcuts", "Navigation", "Actions", "General"} {
		if !strings.Contains(view, section) {
			t.Errorf("View() missing section header %q", section)
		}
	}

	// Verify keybinding labels are rendered (the Help().Desc values)
	expectedLabels := []string{
		"up", "down", "attach", "new session", "kill", "delete",
		"refresh", "quit", "help", "search", "notifications",
	}
	for _, label := range expectedLabels {
		if !strings.Contains(view, label) {
			t.Errorf("View() missing keybinding label %q", label)
		}
	}

	// Verify the detach key hint appears
	if !strings.Contains(view, "Ctrl+]") {
		t.Error("View() missing detach key hint \"Ctrl+]\"")
	}

	// Verify the close instruction appears
	if !strings.Contains(view, "Press any key to close") {
		t.Error("View() missing close instruction")
	}
}

func TestHelpModel_View_KeyLabels(t *testing.T) {
	cfg := config.DefaultKeybindings()
	keys := NewKeyMap(cfg)
	m := NewHelpModel(keys, "Ctrl+]")

	view := m.View()

	// Verify that key names from the default config appear in the view.
	// The default Help() key for Up is "up/k" and for Quit is "q/ctrl+c".
	expectedKeys := []string{"up/k", "q/ctrl+c", "n", "?"}
	for _, k := range expectedKeys {
		if !strings.Contains(view, k) {
			t.Errorf("View() missing key label %q", k)
		}
	}
}

func TestWriteBinding(t *testing.T) {
	keyStyle := lipglossTestStyle()
	descStyle := lipglossTestStyle()

	binding := key.NewBinding(
		key.WithKeys("x"),
		key.WithHelp("x", "test action"),
	)

	var b strings.Builder
	writeBinding(&b, keyStyle, descStyle, binding)

	result := b.String()
	if !strings.Contains(result, "x") {
		t.Error("writeBinding output missing key 'x'")
	}
	if !strings.Contains(result, "test action") {
		t.Error("writeBinding output missing description 'test action'")
	}
	if !strings.HasSuffix(result, "\n") {
		t.Error("writeBinding output should end with newline")
	}
}

func TestWriteShortcut(t *testing.T) {
	keyStyle := lipglossTestStyle()
	descStyle := lipglossTestStyle()

	var b strings.Builder
	writeShortcut(&b, keyStyle, descStyle, "ctrl+x", "exit")

	result := b.String()
	if !strings.Contains(result, "ctrl+x") {
		t.Error("writeShortcut output missing key 'ctrl+x'")
	}
	if !strings.Contains(result, "exit") {
		t.Error("writeShortcut output missing description 'exit'")
	}
	if !strings.HasPrefix(result, "  ") {
		t.Error("writeShortcut output should start with two-space indent")
	}
}

func TestWriteShortcut_Padding(t *testing.T) {
	keyStyle := lipglossTestStyle()
	descStyle := lipglossTestStyle()

	// Short key should be padded to keyColWidth (14)
	var b1 strings.Builder
	writeShortcut(&b1, keyStyle, descStyle, "x", "short")

	// Long key (>= 14 chars) should not be padded further
	var b2 strings.Builder
	writeShortcut(&b2, keyStyle, descStyle, "a-very-long-key", "long")

	r1 := b1.String()
	r2 := b2.String()

	if !strings.Contains(r1, "short") {
		t.Error("short key output missing description")
	}
	if !strings.Contains(r2, "long") {
		t.Error("long key output missing description")
	}
}

// lipglossTestStyle returns a no-op lipgloss style for testing.
func lipglossTestStyle() lipgloss.Style {
	return lipgloss.NewStyle()
}
