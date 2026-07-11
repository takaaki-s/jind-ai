package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/daemon"
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

// --- stepAgent ---

// newConfigMgrWithDefaultAgent builds a config.Manager from a config.yaml
// containing "default_agent: <kind>". Empty kind writes no config file so
// GetDefaultAgent falls back to its "claude" default.
func newConfigMgrWithDefaultAgent(t *testing.T, kind string) *config.Manager {
	t.Helper()
	dir := t.TempDir()
	if kind != "" {
		yaml := "default_agent: " + kind + "\n"
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	m, err := config.NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestResolveInitialAgentKind_TransientWins(t *testing.T) {
	// transient default is a valid kind → wins over config default.
	cfg := newConfigMgrWithDefaultAgent(t, "codex")
	got := resolveInitialAgentKind("claude", cfg, []string{"claude", "codex"})
	if got != "claude" {
		t.Errorf("got %q, want %q", got, "claude")
	}
}

func TestResolveInitialAgentKind_ConfigWinsWhenTransientEmpty(t *testing.T) {
	cfg := newConfigMgrWithDefaultAgent(t, "codex")
	got := resolveInitialAgentKind("", cfg, []string{"claude", "codex"})
	if got != "codex" {
		t.Errorf("got %q, want %q", got, "codex")
	}
}

func TestResolveInitialAgentKind_FallsBackToClaude(t *testing.T) {
	// Transient outside kinds + no config default → GetDefaultAgent returns
	// its "claude" fallback, which is present in kinds.
	cfg := newConfigMgrWithDefaultAgent(t, "")
	got := resolveInitialAgentKind("nonsense", cfg, []string{"claude", "codex"})
	if got != "claude" {
		t.Errorf("got %q, want %q", got, "claude")
	}
}

func TestResolveInitialAgentKind_FallsBackToFirstKind(t *testing.T) {
	// kinds has no "claude" and no config-default match → first element wins.
	// nil configMgr also exercises the nil-guard.
	got := resolveInitialAgentKind("", nil, []string{"aider", "codex"})
	if got != "aider" {
		t.Errorf("got %q, want %q", got, "aider")
	}
}

func TestStepIndex(t *testing.T) {
	// Guards §N1 in 01_spec.md: skipping stepAgent (single adapter) must not
	// leave a visible gap in the "Step N" numbering shown to the user.
	tests := []struct {
		name         string
		step         formStep
		agentSkipped bool
		want         int
	}{
		{"workDir stays 1 when picker shown", stepWorkDir, false, 1},
		{"workDir stays 1 when picker skipped", stepWorkDir, true, 1},
		{"agent is 2 (only reachable when not skipped)", stepAgent, false, 2},
		{"fleet is 3 when picker shown", stepFleet, false, 3},
		{"fleet shifts to 2 when picker skipped", stepFleet, true, 2},
		{"worktree is 4 when picker shown", stepWorktree, false, 4},
		{"worktree shifts to 3 when picker skipped", stepWorktree, true, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stepIndex(tt.step, tt.agentSkipped)
			if got != tt.want {
				t.Errorf("stepIndex(%v, %v) = %d, want %d", tt.step, tt.agentSkipped, got, tt.want)
			}
		})
	}
}

// newAgentStepModel builds a CreateFormModel already at stepAgent with the
// given kinds and cursor position. Constructed by hand to avoid dragging in
// NewCreateFormModel's daemon.Client / config paths.
func newAgentStepModel(kinds []string, cursor int) CreateFormModel {
	fleet := textinput.New()
	return CreateFormModel{
		step:             stepAgent,
		agentKindOptions: kinds,
		agentCursor:      cursor,
		fleetInput:       fleet,
	}
}

func TestAdvanceFromWorkDir_SkipsWhenSingleAdapter(t *testing.T) {
	fleet := textinput.New()
	m := CreateFormModel{
		step:             stepWorkDir,
		agentKindOptions: []string{"claude"},
		fleetInput:       fleet,
	}
	advanced, _ := m.advanceFromWorkDir()
	if advanced.step != stepFleet {
		t.Errorf("single adapter: step = %v, want stepFleet", advanced.step)
	}
	if !advanced.fleetInput.Focused() {
		t.Error("single adapter: fleet input should be focused after skip")
	}
}

func TestAdvanceFromWorkDir_ShowsPickerWhenMultipleAdapters(t *testing.T) {
	fleet := textinput.New()
	m := CreateFormModel{
		step:             stepWorkDir,
		agentKindOptions: []string{"claude", "codex"},
		fleetInput:       fleet,
	}
	advanced, _ := m.advanceFromWorkDir()
	if advanced.step != stepAgent {
		t.Errorf("multi adapter: step = %v, want stepAgent", advanced.step)
	}
	if advanced.fleetInput.Focused() {
		t.Error("multi adapter: fleet input must not be focused before stepAgent enters")
	}
}

func TestUpdateAgentStep_UpDownEnter(t *testing.T) {
	m := newAgentStepModel([]string{"claude", "codex"}, 0)

	// up at top → cursor stays at 0
	afterUp, _ := m.updateAgentStep(tea.KeyMsg{Type: tea.KeyUp})
	if afterUp.(CreateFormModel).agentCursor != 0 {
		t.Errorf("up at top: cursor = %d, want 0", afterUp.(CreateFormModel).agentCursor)
	}

	// down moves cursor forward
	afterDown, _ := afterUp.(CreateFormModel).updateAgentStep(tea.KeyMsg{Type: tea.KeyDown})
	if afterDown.(CreateFormModel).agentCursor != 1 {
		t.Errorf("down: cursor = %d, want 1", afterDown.(CreateFormModel).agentCursor)
	}

	// down at bottom → cursor stays at last index
	afterDown2, _ := afterDown.(CreateFormModel).updateAgentStep(tea.KeyMsg{Type: tea.KeyDown})
	if afterDown2.(CreateFormModel).agentCursor != 1 {
		t.Errorf("down at bottom: cursor = %d, want 1", afterDown2.(CreateFormModel).agentCursor)
	}

	// j/k mirror down/up
	afterK, _ := afterDown2.(CreateFormModel).updateAgentStep(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if afterK.(CreateFormModel).agentCursor != 0 {
		t.Errorf("k: cursor = %d, want 0", afterK.(CreateFormModel).agentCursor)
	}
	afterJ, _ := afterK.(CreateFormModel).updateAgentStep(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if afterJ.(CreateFormModel).agentCursor != 1 {
		t.Errorf("j: cursor = %d, want 1", afterJ.(CreateFormModel).agentCursor)
	}

	// enter commits selectedAgentKind and transitions to stepFleet
	afterEnter, _ := afterJ.(CreateFormModel).updateAgentStep(tea.KeyMsg{Type: tea.KeyEnter})
	got := afterEnter.(CreateFormModel)
	if got.step != stepFleet {
		t.Errorf("enter: step = %v, want stepFleet", got.step)
	}
	if got.selectedAgentKind != "codex" {
		t.Errorf("enter: selectedAgentKind = %q, want %q", got.selectedAgentKind, "codex")
	}
	if !got.fleetInput.Focused() {
		t.Error("enter: fleet input should be focused after transition")
	}
}

// TestSubmitWith_IncludesAgentKind verifies that submitWith reads
// m.selectedAgentKind and produces a non-nil tea.Cmd. The daemon.Client is a
// concrete type, so intercepting the actual NewOptions.AgentKind field on the
// wire would require refactoring submitWith. This test settles for confirming
// that (a) the field is set correctly by the stepAgent → Enter path and
// (b) submitWith closes over it (returned cmd is non-nil).
func TestSubmitWith_IncludesAgentKind(t *testing.T) {
	dir := t.TempDir()

	dp := NewDirPickerModel(dir)
	dp.result = dir
	dp.selected = true

	fleet := textinput.New()
	fleet.SetValue("default")

	m := CreateFormModel{
		client:           daemon.NewClient("/nonexistent/jin-test.sock"),
		step:             stepAgent,
		dirPicker:        dp,
		fleetInput:       fleet,
		agentKindOptions: []string{"claude", "codex"},
		agentCursor:      1,
	}

	// Simulate Enter on stepAgent → selectedAgentKind should be committed to
	// "codex" (kinds[1]), and stepFleet becomes active.
	afterEnter, _ := m.updateAgentStep(tea.KeyMsg{Type: tea.KeyEnter})
	after := afterEnter.(CreateFormModel)
	if after.selectedAgentKind != "codex" {
		t.Fatalf("selectedAgentKind = %q, want %q", after.selectedAgentKind, "codex")
	}

	cmd := after.submitWith(false)
	if cmd == nil {
		t.Fatal("submitWith returned nil cmd")
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
