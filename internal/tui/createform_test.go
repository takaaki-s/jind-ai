package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/takaaki-s/jindaiko/internal/config"
)

// --- stepWorktree ---

// newWorktreeStepModel builds a minimal CreateFormModel already advanced to
// stepFleet with a dirPicker whose Result() reports dir. Fleet input is
// pre-populated so pressing Enter transitions to stepWorktree.
func newWorktreeStepModel(t *testing.T, dir string) CreateFormModel {
	t.Helper()

	dp := NewDirPickerModel(dir)
	dp.result = dir
	dp.selected = true

	fleet := textinput.New()
	fleet.SetValue("default")
	fleet.Focus()

	return CreateFormModel{
		dirPicker:  dp,
		fleetInput: fleet,
		step:       stepFleet,
	}
}

func TestCreateForm_StepWorktree_ReachedAfterFleet(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	m := newWorktreeStepModel(t, dir)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(CreateFormModel)

	if got.step != stepWorktree {
		t.Fatalf("step = %v, want stepWorktree", got.step)
	}
	if got.worktreeDisabled {
		t.Errorf("worktreeDisabled = true, want false for git repo")
	}
	if got.worktreeEnabled {
		t.Errorf("worktreeEnabled = true, want false (default)")
	}
}

func TestCreateForm_StepWorktree_DisabledWhenNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	m := newWorktreeStepModel(t, dir)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(CreateFormModel)

	if got.step != stepWorktree {
		t.Fatalf("step = %v, want stepWorktree", got.step)
	}
	if !got.worktreeDisabled {
		t.Errorf("worktreeDisabled = false, want true for non-git dir")
	}
	if !strings.Contains(got.worktreeReason, "not a git repository") {
		t.Errorf("worktreeReason = %q, want to contain %q", got.worktreeReason, "not a git repository")
	}

	yKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}
	updated2, _ := got.Update(yKey)
	got2 := updated2.(CreateFormModel)
	if got2.worktreeEnabled {
		t.Errorf("pressing y on disabled step set worktreeEnabled = true; want false")
	}
}

func TestCreateForm_StepWorktree_EnabledInGitRepo_YEnables(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	m := newWorktreeStepModel(t, dir)

	advanced, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := advanced.(CreateFormModel)

	if got.worktreeDisabled {
		t.Fatalf("worktreeDisabled = true, want false for git repo")
	}

	yKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}
	afterY, _ := got.Update(yKey)
	gotY := afterY.(CreateFormModel)
	if !gotY.worktreeEnabled {
		t.Errorf("after y: worktreeEnabled = false, want true")
	}

	nKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}
	afterN, _ := gotY.Update(nKey)
	gotN := afterN.(CreateFormModel)
	if gotN.worktreeEnabled {
		t.Errorf("after n: worktreeEnabled = true, want false")
	}
}

func TestCreateForm_StepWorktree_EscReturnsToFleet(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	m := newWorktreeStepModel(t, dir)

	advanced, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := advanced.(CreateFormModel)
	if got.step != stepWorktree {
		t.Fatalf("precondition: step = %v, want stepWorktree", got.step)
	}

	afterEsc, _ := got.Update(tea.KeyMsg{Type: tea.KeyEsc})
	gotEsc := afterEsc.(CreateFormModel)
	if gotEsc.step != stepFleet {
		t.Errorf("after Esc: step = %v, want stepFleet", gotEsc.step)
	}
}

// --- stepFleet (skip-description flow) ---

// newFleetStepModel drives updateWorkDirStep with a directory already selected
// so the model lands in stepFleet — the manual-description step was removed
// (Layer A + Layer C cover it), so stepWorkDir now jumps straight to fleet.
func newFleetStepModel(t *testing.T, dir string) CreateFormModel {
	t.Helper()

	dp := NewDirPickerModel(dir)
	dp.result = dir
	dp.selected = true

	m := CreateFormModel{
		step:       stepWorkDir,
		dirPicker:  dp,
		fleetInput: textinput.New(),
	}

	updated, _ := m.updateWorkDirStep(tea.WindowSizeMsg{Width: 80, Height: 24})
	return updated.(CreateFormModel)
}

func TestCreateForm_WorkDirTransitionsDirectlyToFleet(t *testing.T) {
	// Confirms the flow skips the removed stepDescription and focuses the
	// fleet input, so the user's first typed character after picking a
	// directory reaches fleetInput, not a dead description field.
	dir := t.TempDir()
	got := newFleetStepModel(t, dir)

	if got.step != stepFleet {
		t.Fatalf("step = %v, want stepFleet", got.step)
	}
	if !got.fleetInput.Focused() {
		t.Error("fleetInput should be focused after entering stepFleet")
	}
}

func TestCreateForm_StepFleet_EscReturnsToWorkDir(t *testing.T) {
	// Fleet used to Esc back into stepDescription; with that step gone, Esc
	// must clear dirPicker.selected so the user can pick a different dir.
	dir := t.TempDir()
	m := newFleetStepModel(t, dir)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(CreateFormModel)

	if got.step != stepWorkDir {
		t.Errorf("after Esc: step = %v, want stepWorkDir", got.step)
	}
	if got.dirPicker.selected {
		t.Error("dirPicker.selected should be reset to false so the user can re-pick a dir")
	}
	if got.fleetInput.Focused() {
		t.Error("fleetInput should be blurred after Esc")
	}
}

// --- convertDirHistoryEntries ---

func TestConvertDirHistoryEntries_MultipleEntries(t *testing.T) {
	now := time.Now()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory")
	}

	entries := []config.DirHistoryEntry{
		{Path: home + "/project1", LastUsedAt: now},
		{Path: home + "/project2", LastUsedAt: now.Add(-time.Hour)},
	}

	result := convertDirHistoryEntries(entries)

	if len(result) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(result))
	}
	if result[0].DisplayPath != "~/project1" {
		t.Errorf("DisplayPath[0] = %q, want %q", result[0].DisplayPath, "~/project1")
	}
	if result[1].DisplayPath != "~/project2" {
		t.Errorf("DisplayPath[1] = %q, want %q", result[1].DisplayPath, "~/project2")
	}
}
