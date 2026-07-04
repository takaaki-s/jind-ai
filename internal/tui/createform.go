package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/takaaki-s/honjin/internal/config"
	"github.com/takaaki-s/honjin/internal/daemon"
	"github.com/takaaki-s/honjin/internal/git"
	"github.com/takaaki-s/honjin/internal/paths"
	"github.com/takaaki-s/honjin/internal/session"
	"github.com/takaaki-s/honjin/internal/tmux"
)

// formStep represents the current form step
type formStep int

const (
	stepHost     formStep = iota // Host selection (only with multiple hosts)
	stepWorkDir                  // Work directory selection
	stepName                     // Session name
	stepFleet                    // Fleet selection
	stepWorktree                 // Worktree yes/no
)

// CreateFormModel is a standalone Bubble Tea model for the session creation form.
// It is designed to run inside a tmux popup and communicates the result
// back to the parent TUI via the JIN_CREATED_SESSION environment variable.
type CreateFormModel struct {
	client   *daemon.Client
	sessions []session.Info
	width    int
	height   int
	err      error

	// Current step
	step formStep

	// Processing state
	processingMsg string

	// Config managers
	configMgr *config.Manager

	// Step 1: Host selection
	selectedHostID    string
	hosts             []daemon.HostInfo
	hostInput         textinput.Model
	filteredHosts     []daemon.HostInfo
	hostSelectedIndex int
	hostDropdownOpen  bool

	// Step 2: Work directory selection
	dirPicker DirPickerModel

	// Step 3: Session name
	nameInput textinput.Model

	// Step 4: Fleet selection
	fleetInput textinput.Model

	// Step 5: Worktree
	worktreeEnabled  bool   // user selection
	worktreeDisabled bool   // true when the current host/dir cannot support worktree
	worktreeReason   string // shown when disabled
}

// createFormCompleteMsg is sent when session creation finishes.
type createFormCompleteMsg struct {
	sessionID string
	err       error
}

// NewCreateFormModel creates a new CreateFormModel with all inputs initialized.
func NewCreateFormModel(socketPath string) CreateFormModel {
	home, _ := os.UserHomeDir()
	configMgr, _ := config.NewManager(paths.Config())

	client := daemon.NewClient(socketPath)

	// Host input
	hostInput := textinput.New()
	hostInput.Placeholder = "local"
	hostInput.CharLimit = 50
	hostInput.Width = 40

	// Name input
	nameInput := textinput.New()
	nameInput.Placeholder = "(auto: directory name)"
	nameInput.CharLimit = 100
	nameInput.Width = 40

	// Fleet input
	fleetInput := textinput.New()
	fleetInput.Placeholder = "default"
	fleetInput.CharLimit = 50
	fleetInput.Width = 40

	// Dir picker - start at home directory
	dirPicker := NewDirPickerModel(home)

	m := CreateFormModel{
		client:     client,
		configMgr:  configMgr,
		hostInput:  hostInput,
		nameInput:  nameInput,
		fleetInput: fleetInput,
		dirPicker:  dirPicker,
	}

	// Fetch hosts
	if hosts, err := client.ListHosts(); err == nil {
		m.hosts = hosts
		m.filteredHosts = hosts
	}

	// Determine starting step
	if m.hasMultipleHosts() {
		m.step = stepHost
		m.hostInput.Focus()
		m.hostDropdownOpen = true
	} else {
		m.step = stepWorkDir
	}

	// Fetch existing sessions for duplicate check
	if sessions, err := client.List(); err == nil {
		m.sessions = sessions
	}

	// Fetch directory history from persistent state
	hostID := m.selectedHostID
	if hostID == "" {
		hostID = "local"
	}
	if entries, err := client.DirHistory(hostID, 5); err == nil {
		m.dirPicker.SetHistory(convertDirHistoryEntries(entries, hostID))
	}

	return m
}

// Init initializes the model.
func (m CreateFormModel) Init() tea.Cmd {
	return textinput.Blink
}

// Update handles messages.
func (m CreateFormModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.dirPicker.width = msg.Width
		m.dirPicker.height = msg.Height - 6 // Reserve space for header/footer
		return m, nil

	case tea.KeyMsg:
		// Ignore key input while processing
		if m.processingMsg != "" {
			return m, nil
		}

		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			// Change Esc behavior based on current step
			switch m.step {
			case stepWorkDir:
				if m.hasMultipleHosts() {
					m.step = stepHost
					m.hostInput.Focus()
					m.hostDropdownOpen = true
					return m, nil
				}
				return m, tea.Quit
			case stepName:
				m.step = stepWorkDir
				m.dirPicker.selected = false
				m.nameInput.Blur()
				return m, nil
			case stepFleet:
				m.step = stepName
				m.fleetInput.Blur()
				m.nameInput.Focus()
				return m, nil
			case stepWorktree:
				m.step = stepFleet
				m.fleetInput.Focus()
				return m, nil
			default:
				return m, tea.Quit
			}
		}

	case createFormCompleteMsg:
		if msg.err != nil {
			m.processingMsg = ""
			m.err = msg.err
			m.step = stepWorktree
			return m, nil
		}
		// Success - set env var for parent TUI to detect
		if tc, err := tmux.NewMgrClient(); err == nil {
			_ = tc.SetEnvironment(tmux.SessionName, "JIN_CREATED_SESSION", msg.sessionID)
		}
		return m, tea.Quit

	case DirHistoryRemoveMsg:
		// Remove from persistent history since directory does not exist
		_ = m.client.RemoveDirHistory(msg.HostID, msg.Path)
		return m, nil
	}

	// Step-specific update
	switch m.step {
	case stepHost:
		return m.updateHostStep(msg)
	case stepWorkDir:
		return m.updateWorkDirStep(msg)
	case stepName:
		return m.updateNameStep(msg)
	case stepFleet:
		return m.updateFleetStep(msg)
	case stepWorktree:
		return m.updateWorktreeStep(msg)
	}

	return m, nil
}

// View renders the form.
func (m CreateFormModel) View() string {
	if m.processingMsg != "" {
		return "\n  ⟳ " + m.processingMsg
	}

	var b strings.Builder

	// Title
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7aa2f7"))
	b.WriteString(titleStyle.Render("  New Session"))
	b.WriteString("\n")

	// Error
	if m.err != nil {
		errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#f7768e"))
		b.WriteString("  " + errStyle.Render(fmt.Sprintf("Error: %v", m.err)))
		b.WriteString("\n")
	}

	// Step indicator
	stepStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#565f89"))
	switch m.step {
	case stepHost:
		b.WriteString(stepStyle.Render("  Step 1: Select Host"))
		b.WriteString("\n\n")
		b.WriteString(m.viewHostStep())
	case stepWorkDir:
		if m.selectedHostID != "" && m.selectedHostID != "local" {
			b.WriteString(stepStyle.Render(fmt.Sprintf("  Host: %s", m.selectedHostID)))
			b.WriteString("\n")
		}
		b.WriteString(stepStyle.Render("  Step 2: Select Work Directory"))
		b.WriteString("\n")
		b.WriteString(m.dirPicker.View())
	case stepName:
		if m.selectedHostID != "" && m.selectedHostID != "local" {
			b.WriteString(stepStyle.Render(fmt.Sprintf("  Host: %s", m.selectedHostID)))
			b.WriteString("\n")
		}
		displayDir := m.dirPicker.Result()
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(displayDir, home) {
			displayDir = "~" + displayDir[len(home):]
		}
		b.WriteString(stepStyle.Render(fmt.Sprintf("  Dir: %s", displayDir)))
		b.WriteString("\n")
		b.WriteString(stepStyle.Render("  Step 3: Session Name"))
		b.WriteString("\n\n")
		b.WriteString(m.viewNameStep())
	case stepFleet:
		if m.selectedHostID != "" && m.selectedHostID != "local" {
			b.WriteString(stepStyle.Render(fmt.Sprintf("  Host: %s", m.selectedHostID)))
			b.WriteString("\n")
		}
		displayDir := m.dirPicker.Result()
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(displayDir, home) {
			displayDir = "~" + displayDir[len(home):]
		}
		b.WriteString(stepStyle.Render(fmt.Sprintf("  Dir: %s", displayDir)))
		b.WriteString("\n")
		name := m.nameInput.Value()
		if name == "" {
			name = filepath.Base(m.dirPicker.Result())
		}
		b.WriteString(stepStyle.Render(fmt.Sprintf("  Name: %s", name)))
		b.WriteString("\n")
		b.WriteString(stepStyle.Render("  Step 4: Fleet"))
		b.WriteString("\n\n")
		b.WriteString(m.viewFleetStep())
	case stepWorktree:
		if m.selectedHostID != "" && m.selectedHostID != "local" {
			b.WriteString(stepStyle.Render(fmt.Sprintf("  Host: %s", m.selectedHostID)))
			b.WriteString("\n")
		}
		displayDir := m.dirPicker.Result()
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(displayDir, home) {
			displayDir = "~" + displayDir[len(home):]
		}
		b.WriteString(stepStyle.Render(fmt.Sprintf("  Dir: %s", displayDir)))
		b.WriteString("\n")
		name := m.nameInput.Value()
		if name == "" {
			name = filepath.Base(m.dirPicker.Result())
		}
		b.WriteString(stepStyle.Render(fmt.Sprintf("  Name: %s", name)))
		b.WriteString("\n")
		fleet := m.fleetInput.Value()
		if fleet == "" {
			fleet = "default"
		}
		b.WriteString(stepStyle.Render(fmt.Sprintf("  Fleet: %s", fleet)))
		b.WriteString("\n")
		b.WriteString(stepStyle.Render("  Step 5: Worktree"))
		b.WriteString("\n\n")
		b.WriteString(m.viewWorktreeStep())
	}

	return b.String()
}

// --- Step: Host ---

func (m CreateFormModel) updateHostStep(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "enter":
			if m.hostDropdownOpen && len(m.filteredHosts) > 0 {
				m.selectHost()
			}
			// Move to next step
			m.step = stepWorkDir
			m.hostInput.Blur()
			m.hostDropdownOpen = false

			// Re-fetch history when host changes
			hostID := m.selectedHostID
			if hostID == "" {
				hostID = "local"
			}
			if entries, err := m.client.DirHistory(hostID, 5); err == nil {
				m.dirPicker.SetHistory(convertDirHistoryEntries(entries, hostID))
			}

			// Switch directory picker to remote mode when remote host is selected
			if m.selectedHostID != "" && m.selectedHostID != "local" && m.configMgr != nil {
				if hc := m.configMgr.GetHost(m.selectedHostID); hc != nil {
					cmd := m.dirPicker.SetRemoteHost(hc)
					return m, cmd
				}
			} else {
				m.dirPicker.ClearRemoteHost()
			}
			return m, nil

		case "up":
			if m.hostDropdownOpen && m.hostSelectedIndex > 0 {
				m.hostSelectedIndex--
			}
			return m, nil

		case "down":
			if m.hostDropdownOpen && m.hostSelectedIndex < len(m.filteredHosts)-1 {
				m.hostSelectedIndex++
			}
			return m, nil
		}
	}

	oldQuery := m.hostInput.Value()
	var cmd tea.Cmd
	m.hostInput, cmd = m.hostInput.Update(msg)
	if m.hostInput.Value() != oldQuery {
		m.filterHosts()
	}

	return m, cmd
}

func (m CreateFormModel) viewHostStep() string {
	var b strings.Builder

	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7aa2f7"))
	b.WriteString("  " + labelStyle.Render("▸ Host:"))
	b.WriteString("\n")
	b.WriteString("    " + m.hostInput.View())
	b.WriteString("\n")

	if m.hostDropdownOpen && len(m.filteredHosts) > 0 {
		selectedStyle := lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("255")).
			Background(lipgloss.Color("#7aa2f7"))
		dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#414868"))

		for i, h := range m.filteredHosts {
			connStatus := ""
			if !h.Connected {
				connStatus = " (disconnected)"
			}
			entry := h.ID + connStatus
			if i == m.hostSelectedIndex {
				padded := "▸ " + entry
				b.WriteString("    " + selectedStyle.Render(padded))
			} else {
				b.WriteString("      " + dimStyle.Render(entry))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	b.WriteString("  " + helpStyle.Render("Enter:confirm  ↑↓:navigate  Esc:cancel"))

	return b.String()
}

// --- Step: WorkDir ---

func (m CreateFormModel) updateWorkDirStep(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.dirPicker, cmd = m.dirPicker.Update(msg)

	// Check if directory was selected
	if m.dirPicker.Selected() {
		m.step = stepName
		m.nameInput.Focus()
		// Set default name to directory basename
		basename := filepath.Base(m.dirPicker.Result())
		m.nameInput.SetValue(basename)
		return m, textinput.Blink
	}

	return m, cmd
}

// --- Step: Name ---

func (m CreateFormModel) updateNameStep(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "enter":
			m.step = stepFleet
			m.nameInput.Blur()
			m.fleetInput.Focus()
			return m, textinput.Blink
		}
	}

	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

func (m CreateFormModel) viewNameStep() string {
	var b strings.Builder

	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7aa2f7"))
	b.WriteString("  " + labelStyle.Render("▸ Name:"))
	b.WriteString("\n")
	b.WriteString("    " + m.nameInput.View())
	b.WriteString("\n\n")

	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	b.WriteString("  " + helpStyle.Render("Enter:next  Esc:back"))

	return b.String()
}

// --- Step: Fleet ---

func (m CreateFormModel) updateFleetStep(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "enter":
			m.step = stepWorktree
			m.fleetInput.Blur()
			m.computeWorktreeAvailability()
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.fleetInput, cmd = m.fleetInput.Update(msg)
	return m, cmd
}

func (m CreateFormModel) viewFleetStep() string {
	var b strings.Builder

	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7aa2f7"))
	b.WriteString("  " + labelStyle.Render("▸ Fleet:"))
	b.WriteString("\n")
	b.WriteString("    " + m.fleetInput.View())
	b.WriteString("\n\n")

	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	b.WriteString("  " + helpStyle.Render("Enter:next  Esc:back"))

	return b.String()
}

// --- Step: Worktree ---

// computeWorktreeAvailability decides whether the worktree option can be
// enabled for the currently-selected host/dir and resets the user's choice.
// Called on each entry to stepWorktree so a WorkDir/host change made via
// Esc-back is picked up.
func (m *CreateFormModel) computeWorktreeAvailability() {
	m.worktreeEnabled = false
	m.worktreeDisabled = false
	m.worktreeReason = ""

	if m.selectedHostID != "" && m.selectedHostID != "local" {
		m.worktreeDisabled = true
		m.worktreeReason = "remote hosts not supported yet"
		return
	}
	if !git.IsGitRoot(m.dirPicker.Result()) {
		m.worktreeDisabled = true
		m.worktreeReason = "not a git repository"
		return
	}
}

func (m CreateFormModel) updateWorktreeStep(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "enter":
		return m.handleSubmit()
	case "y", "Y":
		if !m.worktreeDisabled {
			m.worktreeEnabled = true
		}
		return m, nil
	case "n", "N":
		m.worktreeEnabled = false
		return m, nil
	case " ", "space":
		if !m.worktreeDisabled {
			m.worktreeEnabled = !m.worktreeEnabled
		}
		return m, nil
	}
	return m, nil
}

func (m CreateFormModel) viewWorktreeStep() string {
	var b strings.Builder

	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7aa2f7"))
	mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#565f89"))
	selectedStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("255")).
		Background(lipgloss.Color("#7aa2f7"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#414868"))

	b.WriteString("  " + labelStyle.Render("▸ Create worktree?"))
	b.WriteString("\n")

	if m.worktreeDisabled {
		b.WriteString("    " + mutedStyle.Render(m.worktreeReason))
		b.WriteString("\n")
		b.WriteString("    " + mutedStyle.Render("Selection: No"))
		b.WriteString("\n\n")
	} else {
		yesLabel := "  [y] Yes  "
		noLabel := "  [n] No  "
		if m.worktreeEnabled {
			b.WriteString("    " + selectedStyle.Render(yesLabel) + dimStyle.Render(noLabel))
		} else {
			b.WriteString("    " + dimStyle.Render(yesLabel) + selectedStyle.Render(noLabel))
		}
		b.WriteString("\n\n")

		if m.worktreeEnabled {
			b.WriteString("  " + mutedStyle.Render("Preview:"))
			b.WriteString("\n")
			b.WriteString("  " + mutedStyle.Render("  Worktree: (auto — jin-<8hex>)"))
			b.WriteString("\n")
			b.WriteString("  " + mutedStyle.Render("  Branch:   (auto — wip/jin-<8hex>)"))
			b.WriteString("\n")
			b.WriteString("  " + mutedStyle.Render("  Base:     (origin/HEAD)"))
			b.WriteString("\n\n")
		}
	}

	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	b.WriteString("  " + helpStyle.Render("Enter:create  y/n:toggle  Esc:back"))

	return b.String()
}

// --- Submit ---

func (m CreateFormModel) handleSubmit() (tea.Model, tea.Cmd) {
	workDir := m.dirPicker.Result()
	if workDir == "" {
		m.err = fmt.Errorf("work directory is required")
		return m, nil
	}

	// Validate directory exists (local only; remote is validated by the remote dir picker)
	if m.selectedHostID == "" || m.selectedHostID == "local" {
		if info, err := os.Stat(workDir); err != nil {
			m.err = fmt.Errorf("directory does not exist: %s", workDir)
			return m, nil
		} else if !info.IsDir() {
			m.err = fmt.Errorf("not a directory: %s", workDir)
			return m, nil
		}
	}

	name := strings.TrimSpace(m.nameInput.Value())
	fleet := strings.TrimSpace(m.fleetInput.Value())
	hostID := m.selectedHostID

	m.processingMsg = "Creating session..."
	m.err = nil

	client := m.client
	worktree := m.worktreeEnabled
	return m, func() tea.Msg {
		s, err := client.NewWithOptions(daemon.NewOptions{
			Name:     name,
			WorkDir:  workDir,
			Start:    true,
			HostID:   hostID,
			Fleet:    fleet,
			Worktree: worktree,
		})
		if err != nil {
			return createFormCompleteMsg{err: err}
		}
		return createFormCompleteMsg{sessionID: s.ID}
	}
}

// --- Internal helpers ---

func (m *CreateFormModel) hasMultipleHosts() bool {
	return len(m.hosts) > 1
}

func (m *CreateFormModel) filterHosts() {
	query := strings.ToLower(m.hostInput.Value())
	if query == "" {
		m.filteredHosts = m.hosts
		m.hostDropdownOpen = true
		m.hostSelectedIndex = 0
		return
	}
	m.filteredHosts = nil
	for _, h := range m.hosts {
		if strings.Contains(strings.ToLower(h.ID), query) {
			m.filteredHosts = append(m.filteredHosts, h)
		}
	}
	m.hostDropdownOpen = len(m.filteredHosts) > 0
	m.hostSelectedIndex = 0
}

func (m *CreateFormModel) selectHost() {
	if m.hostSelectedIndex < len(m.filteredHosts) {
		selected := m.filteredHosts[m.hostSelectedIndex]
		m.selectedHostID = selected.ID
		m.hostInput.SetValue(selected.ID)
		m.hostDropdownOpen = false
	}
}

// convertDirHistoryEntries converts config.DirHistoryEntry to tui.HistoryEntry,
// applying display path formatting (~ for home directory).
func convertDirHistoryEntries(entries []config.DirHistoryEntry, hostID string) []HistoryEntry {
	home, _ := os.UserHomeDir()
	result := make([]HistoryEntry, 0, len(entries))
	for _, e := range entries {
		displayPath := e.Path
		// Local: convert home prefix to ~
		if hostID == "local" && home != "" && strings.HasPrefix(displayPath, home) {
			displayPath = "~" + displayPath[len(home):]
		}
		// Remote: Path is already stored in ~/... format, use as-is
		result = append(result, HistoryEntry{
			Path:        e.Path,
			DisplayPath: displayPath,
			LastUsedAt:  e.LastUsedAt,
			HostID:      hostID,
		})
	}
	return result
}
