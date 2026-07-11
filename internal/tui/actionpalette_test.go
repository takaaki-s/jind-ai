package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/takaaki-s/jind-ai/internal/action"
)

// --- helpers ---

// sampleCore returns a compact core set used by most tests. Includes at
// least one NeedsSession action so label decoration is exercised.
func sampleCore() []action.Action {
	return []action.Action{
		{ID: action.IDNew, Kind: action.KindCore, Label: "new session", Shortcut: "n"},
		{ID: action.IDKill, Kind: action.KindCore, Label: "kill session", Shortcut: "x", NeedsSession: true},
		{ID: action.IDRefresh, Kind: action.KindCore, Label: "refresh list", Shortcut: "r"},
	}
}

func samplePlugins() []action.Action {
	return []action.Action{
		{ID: action.PluginIDPrefix + "notifier", Kind: action.KindPlugin, Label: "notifier"},
		{ID: action.PluginIDPrefix + "sync", Kind: action.KindPlugin, Label: "sync"},
	}
}

// typeInto feeds one rune at a time through the model's Update so the
// textinput observes real key events (matches how bubbletea drives it in
// production).
func typeInto(t *testing.T, m PaletteModel, s string) PaletteModel {
	t.Helper()
	for _, r := range s {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(PaletteModel)
	}
	return m
}

func actionIDs(rows []paletteRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.separator {
			out = append(out, "<sep>")
			continue
		}
		out = append(out, r.action.ID)
	}
	return out
}

// --- tests ---

func TestPaletteModel_EmptyQuery_ShowsAll(t *testing.T) {
	m := NewPaletteModel(sampleCore(), samplePlugins(), "", "")

	ids := actionIDs(m.filtered)
	// 3 core + separator + 2 plugin = 6 rows
	if len(ids) != 6 {
		t.Fatalf("filtered rows = %d (%v), want 6", len(ids), ids)
	}
	// Sanity: separator sits between core and plugin.
	if ids[3] != "<sep>" {
		t.Errorf("expected separator at index 3, got %v", ids)
	}
	// First 3 are core, last 2 plugin.
	for i, id := range ids[:3] {
		if !strings.HasPrefix(id, action.CoreIDPrefix) {
			t.Errorf("row %d: expected core prefix, got %q", i, id)
		}
	}
	for i, id := range ids[4:] {
		if !strings.HasPrefix(id, action.PluginIDPrefix) {
			t.Errorf("plugin row %d: expected plugin prefix, got %q", i, id)
		}
	}
}

func TestPaletteModel_SubstringFilter(t *testing.T) {
	m := NewPaletteModel(sampleCore(), samplePlugins(), "", "")
	m = typeInto(t, m, "kill")

	ids := actionIDs(m.filtered)
	if len(ids) != 1 || ids[0] != action.IDKill {
		t.Fatalf("filter 'kill' rows = %v, want [%s]", ids, action.IDKill)
	}
}

// TestPaletteModel_ShortcutNotSearched verifies Shortcut is not part of
// the match haystack. Query "x" is core:kill's shortcut but is not
// present in any Label — filtered must be empty.
func TestPaletteModel_ShortcutNotSearched(t *testing.T) {
	m := NewPaletteModel(sampleCore(), samplePlugins(), "", "")
	m = typeInto(t, m, "x")

	ids := actionIDs(m.filtered)
	if len(ids) != 0 {
		t.Fatalf("filter 'x' rows = %v, want empty (shortcut must not be searched)", ids)
	}
}

func TestPaletteModel_SeparatorSkipped(t *testing.T) {
	m := NewPaletteModel(sampleCore(), samplePlugins(), "", "")

	// Layout: [new, kill, refresh, <sep>, notifier, sync]
	// From cursor=0, Down repeatedly should never rest on the separator.
	for i := 0; i < 6; i++ {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = next.(PaletteModel)
		if r, ok := m.currentRow(); ok && r.separator {
			t.Fatalf("cursor landed on separator at step %d (cursor=%d)", i, m.cursor)
		}
	}

	// Going back up should also skip the separator.
	for i := 0; i < 6; i++ {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
		m = next.(PaletteModel)
		if r, ok := m.currentRow(); ok && r.separator {
			t.Fatalf("cursor landed on separator during Up at step %d", i)
		}
	}
}

func TestPaletteModel_EnterOnSeparator(t *testing.T) {
	m := NewPaletteModel(sampleCore(), samplePlugins(), "", "")

	// Force cursor onto the separator row (index 3 in default layout).
	sepIdx := -1
	for i, r := range m.filtered {
		if r.separator {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		t.Fatal("test setup: no separator in default filtered set")
	}
	m.cursor = sepIdx

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(PaletteModel)

	if m.Selected() != "" {
		t.Errorf("Selected() on separator = %q, want empty", m.Selected())
	}
	if cmd == nil {
		t.Errorf("expected tea.Quit cmd on Enter, got nil")
	}
}

func TestPaletteModel_NeedsSessionLabel(t *testing.T) {
	t.Run("desc suffix appears for NeedsSession actions", func(t *testing.T) {
		m := NewPaletteModel(sampleCore(), samplePlugins(), "sess-1", "foo")
		m.width, m.height = 120, 20 // ensure the row fits without truncation
		out := m.View()
		if !strings.Contains(out, "(foo)") {
			t.Errorf("View output missing (foo) suffix:\n%s", out)
		}
	})

	t.Run("desc suffix absent when NeedsSession=false actions only match", func(t *testing.T) {
		// Filter to only "new session" (NeedsSession=false).
		m := NewPaletteModel(sampleCore(), samplePlugins(), "sess-1", "foo")
		m.width, m.height = 120, 20
		m = typeInto(t, m, "new")
		out := m.View()
		if strings.Contains(out, "(foo)") {
			t.Errorf("View output should not contain (foo) for non-NeedsSession match:\n%s", out)
		}
	})
}

func TestPaletteModel_EnterSelects(t *testing.T) {
	m := NewPaletteModel(sampleCore(), samplePlugins(), "", "")
	// cursor=0 → "new session" in default layout.
	if r, ok := m.currentRow(); !ok || r.action.ID != action.IDNew {
		t.Fatalf("test setup: expected cursor on IDNew, got %+v ok=%v", r, ok)
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(PaletteModel)

	if m.Selected() != action.IDNew {
		t.Errorf("Selected() = %q, want %q", m.Selected(), action.IDNew)
	}
	if cmd == nil {
		t.Errorf("expected tea.Quit cmd on Enter, got nil")
	}
}

func TestPaletteModel_ShortcutColWidth(t *testing.T) {
	t.Run("clamps to minimum with only single-char shortcuts", func(t *testing.T) {
		m := NewPaletteModel(sampleCore(), samplePlugins(), "", "")
		if m.shortcutColWidth != minShortcutColWidth {
			t.Errorf("shortcutColWidth = %d, want %d", m.shortcutColWidth, minShortcutColWidth)
		}
	})

	t.Run("expands to accommodate a longer shortcut", func(t *testing.T) {
		core := append(sampleCore(), action.Action{
			ID: "core:custom", Kind: action.KindCore, Label: "custom", Shortcut: "Ctrl+Alt+P",
		})
		m := NewPaletteModel(core, nil, "", "")
		if m.shortcutColWidth != 10 {
			t.Errorf("shortcutColWidth = %d, want 10", m.shortcutColWidth)
		}
		m.width, m.height = 120, 20
		if !strings.Contains(m.View(), "Ctrl+Alt+P") {
			t.Errorf("View() missing full 10-char shortcut, got:\n%s", m.View())
		}
	})

	t.Run("clamps to maximum for pathological input", func(t *testing.T) {
		core := append(sampleCore(), action.Action{
			ID: "core:huge", Kind: action.KindCore, Label: "huge", Shortcut: strings.Repeat("A", 30),
		})
		m := NewPaletteModel(core, nil, "", "")
		if m.shortcutColWidth != maxShortcutColWidth {
			t.Errorf("shortcutColWidth = %d, want %d", m.shortcutColWidth, maxShortcutColWidth)
		}
	})

	t.Run("fits three-modifier hint without truncation", func(t *testing.T) {
		// Widest realistic FormatKeyHint output: "Shift+Ctrl+Alt+P" (16 runes).
		// The maxShortcutColWidth bound must accommodate it verbatim.
		hint := "Shift+Ctrl+Alt+P"
		core := append(sampleCore(), action.Action{
			ID: "core:tri-mod", Kind: action.KindCore, Label: "tri-mod", Shortcut: hint,
		})
		m := NewPaletteModel(core, nil, "", "")
		if m.shortcutColWidth != len(hint) {
			t.Errorf("shortcutColWidth = %d, want %d", m.shortcutColWidth, len(hint))
		}
		m.width, m.height = 120, 20
		if !strings.Contains(m.View(), hint) {
			t.Errorf("View() missing full 3-modifier hint %q, got:\n%s", hint, m.View())
		}
	})
}

func TestPaletteModel_EscQuits(t *testing.T) {
	m := NewPaletteModel(sampleCore(), samplePlugins(), "", "")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(PaletteModel)

	if m.Selected() != "" {
		t.Errorf("Selected() after Esc = %q, want empty", m.Selected())
	}
	if cmd == nil {
		t.Fatalf("expected non-nil cmd on Esc")
	}
	// tea.Quit is a func that returns tea.QuitMsg{}. Invoke to verify.
	if msg := cmd(); msg != (tea.QuitMsg{}) {
		t.Errorf("Esc cmd returned %T, want tea.QuitMsg", msg)
	}
}
