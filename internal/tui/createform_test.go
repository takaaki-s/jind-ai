package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/takaaki-s/honjin/internal/config"
	"github.com/takaaki-s/honjin/internal/daemon"
)

// --- hasMultipleHosts ---

func TestCreateFormModel_HasMultipleHosts(t *testing.T) {
	t.Run("empty hosts returns false", func(t *testing.T) {
		m := CreateFormModel{}
		if m.hasMultipleHosts() {
			t.Error("hasMultipleHosts() should return false for empty hosts")
		}
	})

	t.Run("single host returns false", func(t *testing.T) {
		m := CreateFormModel{
			hosts: []daemon.HostInfo{
				{ID: "local", Type: "local", Connected: true},
			},
		}
		if m.hasMultipleHosts() {
			t.Error("hasMultipleHosts() should return false for single host")
		}
	})

	t.Run("multiple hosts returns true", func(t *testing.T) {
		m := CreateFormModel{
			hosts: []daemon.HostInfo{
				{ID: "local", Type: "local", Connected: true},
				{ID: "remote-dev", Type: "ssh", Connected: true},
			},
		}
		if !m.hasMultipleHosts() {
			t.Error("hasMultipleHosts() should return true for multiple hosts")
		}
	})
}

// --- filterHosts ---

func TestCreateFormModel_FilterHosts(t *testing.T) {
	makeModel := func(hosts []daemon.HostInfo, query string) *CreateFormModel {
		hi := textinput.New()
		hi.SetValue(query)
		return &CreateFormModel{
			hosts:     hosts,
			hostInput: hi,
		}
	}

	allHosts := []daemon.HostInfo{
		{ID: "local", Type: "local", Connected: true},
		{ID: "remote-dev", Type: "ssh", Connected: true},
		{ID: "remote-staging", Type: "ssh", Connected: false},
	}

	t.Run("empty query returns all hosts", func(t *testing.T) {
		m := makeModel(allHosts, "")
		m.filterHosts()

		if len(m.filteredHosts) != 3 {
			t.Fatalf("filterHosts('') returned %d hosts, want 3", len(m.filteredHosts))
		}
		if !m.hostDropdownOpen {
			t.Error("hostDropdownOpen should be true for empty query")
		}
		if m.hostSelectedIndex != 0 {
			t.Errorf("hostSelectedIndex should be 0, got %d", m.hostSelectedIndex)
		}
	})

	t.Run("partial match case insensitive", func(t *testing.T) {
		m := makeModel(allHosts, "Remote")
		m.filterHosts()

		if len(m.filteredHosts) != 2 {
			t.Fatalf("filterHosts('Remote') returned %d hosts, want 2: %v", len(m.filteredHosts), m.filteredHosts)
		}
		for _, h := range m.filteredHosts {
			if h.ID != "remote-dev" && h.ID != "remote-staging" {
				t.Errorf("unexpected host %q in filtered results", h.ID)
			}
		}
		if !m.hostDropdownOpen {
			t.Error("hostDropdownOpen should be true when matches exist")
		}
		if m.hostSelectedIndex != 0 {
			t.Errorf("hostSelectedIndex should be 0, got %d", m.hostSelectedIndex)
		}
	})

	t.Run("no matches closes dropdown", func(t *testing.T) {
		m := makeModel(allHosts, "nonexistent")
		m.filterHosts()

		if len(m.filteredHosts) != 0 {
			t.Fatalf("filterHosts('nonexistent') returned %d hosts, want 0", len(m.filteredHosts))
		}
		if m.hostDropdownOpen {
			t.Error("hostDropdownOpen should be false when no matches")
		}
	})

	t.Run("match resets hostSelectedIndex to 0", func(t *testing.T) {
		m := makeModel(allHosts, "")
		m.hostSelectedIndex = 2
		m.filterHosts()

		if m.hostSelectedIndex != 0 {
			t.Errorf("hostSelectedIndex should be reset to 0, got %d", m.hostSelectedIndex)
		}
	})
}

// --- selectHost ---

func TestCreateFormModel_SelectHost(t *testing.T) {
	t.Run("valid index sets selectedHostID and input value", func(t *testing.T) {
		hi := textinput.New()
		m := CreateFormModel{
			hostInput: hi,
			filteredHosts: []daemon.HostInfo{
				{ID: "local", Type: "local", Connected: true},
				{ID: "remote-dev", Type: "ssh", Connected: true},
			},
			hostSelectedIndex: 1,
			hostDropdownOpen:  true,
		}

		m.selectHost()

		if m.selectedHostID != "remote-dev" {
			t.Errorf("selectedHostID = %q, want %q", m.selectedHostID, "remote-dev")
		}
		if m.hostInput.Value() != "remote-dev" {
			t.Errorf("hostInput.Value() = %q, want %q", m.hostInput.Value(), "remote-dev")
		}
		if m.hostDropdownOpen {
			t.Error("hostDropdownOpen should be false after selectHost()")
		}
	})

	t.Run("out of bounds index makes no change", func(t *testing.T) {
		hi := textinput.New()
		m := CreateFormModel{
			hostInput:      hi,
			selectedHostID: "",
			filteredHosts: []daemon.HostInfo{
				{ID: "local", Type: "local", Connected: true},
			},
			hostSelectedIndex: 5, // out of bounds
			hostDropdownOpen:  true,
		}

		m.selectHost()

		if m.selectedHostID != "" {
			t.Errorf("selectedHostID should remain empty, got %q", m.selectedHostID)
		}
		if m.hostInput.Value() != "" {
			t.Errorf("hostInput.Value() should remain empty, got %q", m.hostInput.Value())
		}
		if !m.hostDropdownOpen {
			t.Error("hostDropdownOpen should remain true when index is out of bounds")
		}
	})
}

// --- stepWorktree ---

// newWorktreeStepModel builds a minimal CreateFormModel already advanced to
// stepFleet with a dirPicker whose Result() reports dir. Fleet input is
// pre-populated so pressing Enter transitions to stepWorktree.
func newWorktreeStepModel(t *testing.T, dir, hostID string) CreateFormModel {
	t.Helper()

	dp := NewDirPickerModel(dir)
	dp.result = dir
	dp.selected = true

	fleet := textinput.New()
	fleet.SetValue("default")
	fleet.Focus()

	name := textinput.New()
	name.SetValue(filepath.Base(dir))

	return CreateFormModel{
		selectedHostID: hostID,
		dirPicker:      dp,
		nameInput:      name,
		fleetInput:     fleet,
		step:           stepFleet,
	}
}

func TestCreateForm_StepWorktree_ReachedAfterFleet(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	m := newWorktreeStepModel(t, dir, "")

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
	m := newWorktreeStepModel(t, dir, "")

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

func TestCreateForm_StepWorktree_DisabledWhenRemoteHost(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	m := newWorktreeStepModel(t, dir, "ec2")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(CreateFormModel)

	if got.step != stepWorktree {
		t.Fatalf("step = %v, want stepWorktree", got.step)
	}
	if !got.worktreeDisabled {
		t.Errorf("worktreeDisabled = false, want true for remote host")
	}
	if !strings.Contains(got.worktreeReason, "remote") {
		t.Errorf("worktreeReason = %q, want to mention 'remote'", got.worktreeReason)
	}
}

func TestCreateForm_StepWorktree_EnabledInGitRepo_YEnables(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	m := newWorktreeStepModel(t, dir, "")

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
	m := newWorktreeStepModel(t, dir, "")

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

// --- convertDirHistoryEntries (additional cases) ---

func TestConvertDirHistoryEntries_MultipleEntries(t *testing.T) {
	now := time.Now()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory")
	}

	entries := []config.DirHistoryEntry{
		{Path: home + "/project1", HostID: "local", LastUsedAt: now},
		{Path: home + "/project2", HostID: "local", LastUsedAt: now.Add(-time.Hour)},
	}

	result := convertDirHistoryEntries(entries, "local")

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
