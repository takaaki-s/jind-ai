package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/takaaki-s/jindaiko/internal/config"
	"github.com/takaaki-s/jindaiko/internal/daemon"
	"github.com/takaaki-s/jindaiko/internal/git"
	"github.com/takaaki-s/jindaiko/internal/paths"
	"github.com/takaaki-s/jindaiko/internal/session"
	"github.com/takaaki-s/jindaiko/internal/tmux"
	"github.com/takaaki-s/jindaiko/internal/worktreehook"
)

// formStep represents the current form step.
//
// The TUI creation flow intentionally omits a "Description" step: Layer A
// seeds a repo-derived baseline at creation time and Layer C upgrades it
// from the first user prompt via the Stop hook. Users who really want a
// manual label reach for `jin session new --description` or
// `jin session set-description` — both are unaffected by this.
type formStep int

const (
	stepWorkDir    formStep = iota // Work directory selection
	stepFleet                      // Fleet selection
	stepWorktree                   // Worktree yes/no
	stepHookPrompt                 // Post-create hook not-allowed / changed prompt
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

	// Step 1: Work directory selection
	dirPicker DirPickerModel

	// Step 2: Fleet selection
	fleetInput textinput.Model

	// Step 3: Worktree
	worktreeEnabled  bool   // user selection
	worktreeDisabled bool   // true when the current dir cannot support worktree
	worktreeReason   string // shown when disabled

	// Step 4: Hook prompt (only reached when a post-create hook exists but
	// is not on the allowlist, or its SHA256 has changed).
	hookScriptPath string
	hookVerdict    worktreehook.Verdict
}

// createFormCompleteMsg is sent when session creation finishes.
type createFormCompleteMsg struct {
	sessionID string
	warning   string
	err       error
}

// NewCreateFormModel creates a new CreateFormModel with all inputs initialized.
func NewCreateFormModel(socketPath string) CreateFormModel {
	home, _ := os.UserHomeDir()
	configMgr, _ := config.NewManager(paths.Config())

	client := daemon.NewClient(socketPath)

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
		fleetInput: fleetInput,
		dirPicker:  dirPicker,
		step:       stepWorkDir,
	}

	// Fetch existing sessions for duplicate check
	if sessions, err := client.List(); err == nil {
		m.sessions = sessions
	}

	// Fetch directory history from persistent state
	if entries, err := client.DirHistory(5); err == nil {
		m.dirPicker.SetHistory(convertDirHistoryEntries(entries))
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
				return m, tea.Quit
			case stepFleet:
				m.step = stepWorkDir
				m.dirPicker.selected = false
				m.fleetInput.Blur()
				return m, nil
			case stepWorktree:
				m.step = stepFleet
				m.fleetInput.Focus()
				return m, nil
			case stepHookPrompt:
				m.step = stepWorktree
				m.hookScriptPath = ""
				m.hookVerdict = 0
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
		// Success - set env vars for parent TUI to detect
		if tc, err := tmux.NewMgrClient(); err == nil {
			_ = tc.SetEnvironment(tmux.SessionName, "JIN_CREATED_SESSION", msg.sessionID)
			if msg.warning != "" {
				_ = tc.SetEnvironment(tmux.SessionName, "JIN_CREATED_WARNING", msg.warning)
			}
		}
		return m, tea.Quit

	case DirHistoryRemoveMsg:
		// Remove from persistent history since directory does not exist
		_ = m.client.RemoveDirHistory(msg.Path)
		return m, nil
	}

	// Step-specific update
	switch m.step {
	case stepWorkDir:
		return m.updateWorkDirStep(msg)
	case stepFleet:
		return m.updateFleetStep(msg)
	case stepWorktree:
		return m.updateWorktreeStep(msg)
	case stepHookPrompt:
		return m.updateHookPromptStep(msg)
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
	case stepWorkDir:
		b.WriteString(stepStyle.Render("  Step 1: Select Work Directory"))
		b.WriteString("\n")
		b.WriteString(m.dirPicker.View())
	case stepFleet:
		displayDir := m.dirPicker.Result()
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(displayDir, home) {
			displayDir = "~" + displayDir[len(home):]
		}
		b.WriteString(stepStyle.Render(fmt.Sprintf("  Dir: %s", displayDir)))
		b.WriteString("\n")
		b.WriteString(stepStyle.Render("  Step 2: Fleet"))
		b.WriteString("\n\n")
		b.WriteString(m.viewFleetStep())
	case stepWorktree:
		displayDir := m.dirPicker.Result()
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(displayDir, home) {
			displayDir = "~" + displayDir[len(home):]
		}
		b.WriteString(stepStyle.Render(fmt.Sprintf("  Dir: %s", displayDir)))
		b.WriteString("\n")
		fleet := m.fleetInput.Value()
		if fleet == "" {
			fleet = "default"
		}
		b.WriteString(stepStyle.Render(fmt.Sprintf("  Fleet: %s", fleet)))
		b.WriteString("\n")
		b.WriteString(stepStyle.Render("  Step 3: Worktree"))
		b.WriteString("\n\n")
		b.WriteString(m.viewWorktreeStep())
	case stepHookPrompt:
		b.WriteString(m.viewHookPromptStep())
	}

	return b.String()
}

// --- Step: WorkDir ---

func (m CreateFormModel) updateWorkDirStep(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.dirPicker, cmd = m.dirPicker.Update(msg)

	// Check if directory was selected
	if m.dirPicker.Selected() {
		m.step = stepFleet
		m.fleetInput.Focus()
		return m, textinput.Blink
	}

	return m, cmd
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
// enabled for the currently-selected dir and resets the user's choice.
// Called on each entry to stepWorktree so a WorkDir change made via
// Esc-back is picked up.
func (m *CreateFormModel) computeWorktreeAvailability() {
	m.worktreeEnabled = false
	m.worktreeDisabled = false
	m.worktreeReason = ""

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
			b.WriteString("  " + mutedStyle.Render("  Branch:   (auto — jin/<8hex>)"))
			b.WriteString("\n")
			b.WriteString("  " + mutedStyle.Render("  Base:     (origin/HEAD)"))
			b.WriteString("\n\n")
		}
	}

	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	b.WriteString("  " + helpStyle.Render("Enter:create  y/n:toggle  Esc:back"))

	return b.String()
}

// --- Step: HookPrompt ---

func (m CreateFormModel) updateHookPromptStep(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "a", "A":
		// Trust the hook: add to allowlist with current SHA256, then submit
		// with hooks enabled. Any allow error surfaces as an inline form
		// error and leaves the user on the prompt so they can retry.
		absRepo, err := filepath.Abs(m.dirPicker.Result())
		if err != nil {
			m.err = fmt.Errorf("resolve repo path: %w", err)
			return m, nil
		}
		sha, err := worktreehook.ComputeSHA256(m.hookScriptPath)
		if err != nil {
			m.err = fmt.Errorf("read hook script: %w", err)
			return m, nil
		}
		allowlist, err := worktreehook.LoadAllowlist(paths.State())
		if err != nil {
			m.err = fmt.Errorf("load allowlist: %w", err)
			return m, nil
		}
		if err := allowlist.Allow(absRepo, sha); err != nil {
			m.err = fmt.Errorf("allow hook: %w", err)
			return m, nil
		}
		m.processingMsg = "Creating session..."
		m.err = nil
		return m, m.submitWith(false)
	case "s", "S":
		m.processingMsg = "Creating session..."
		m.err = nil
		return m, m.submitWith(true)
	case "c", "C":
		m.step = stepWorktree
		m.hookScriptPath = ""
		m.hookVerdict = 0
		return m, nil
	}
	return m, nil
}

func (m CreateFormModel) viewHookPromptStep() string {
	var b strings.Builder

	warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#e0af68")).Bold(true)
	pathStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#7aa2f7"))
	keyStyle := lipgloss.NewStyle().Bold(true)
	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	var headline string
	switch m.hookVerdict {
	case worktreehook.VerdictNotAllowed:
		headline = "⚠ Post-create hook is not trusted"
	case worktreehook.VerdictChanged:
		headline = "⚠ Post-create hook has changed since last trust"
	default:
		headline = "⚠ Post-create hook needs attention"
	}

	b.WriteString("  " + warnStyle.Render(headline))
	b.WriteString("\n")
	b.WriteString("  " + pathStyle.Render(m.hookScriptPath))
	b.WriteString("\n\n")
	b.WriteString("  What would you like to do?\n\n")
	b.WriteString("    " + keyStyle.Render("a") + ": Allow (trust this script and run it)\n")
	b.WriteString("    " + keyStyle.Render("s") + ": Skip (create session without running the hook)\n")
	b.WriteString("    " + keyStyle.Render("c") + ": Cancel (back to Worktree step)\n\n")
	b.WriteString("  " + helpStyle.Render("a/s/c or Esc"))

	return b.String()
}

// --- Submit ---

func (m CreateFormModel) handleSubmit() (tea.Model, tea.Cmd) {
	workDir := m.dirPicker.Result()
	if workDir == "" {
		m.err = fmt.Errorf("work directory is required")
		return m, nil
	}

	if info, err := os.Stat(workDir); err != nil {
		m.err = fmt.Errorf("directory does not exist: %s", workDir)
		return m, nil
	} else if !info.IsDir() {
		m.err = fmt.Errorf("not a directory: %s", workDir)
		return m, nil
	}

	// When creating a worktree, check hook allowlist synchronously
	// (fast local file I/O) before dispatching the async create. If the
	// hook script exists but isn't allowlisted, pause and let the user
	// choose: allow, skip, or cancel.
	if m.worktreeEnabled {
		if script, verdict, ok := m.evaluateHook(workDir); ok {
			switch verdict {
			case worktreehook.VerdictNotAllowed, worktreehook.VerdictChanged:
				m.hookScriptPath = script
				m.hookVerdict = verdict
				m.step = stepHookPrompt
				return m, nil
			}
		}
	}

	m.processingMsg = "Creating session..."
	m.err = nil
	return m, m.submitWith(false)
}

// evaluateHook checks whether workDir has a post-create hook that would
// require user attention. Returns (scriptPath, verdict, true) when a script
// exists; (_, _, false) when there is no script or hooks are globally
// disabled via config.
func (m CreateFormModel) evaluateHook(workDir string) (string, worktreehook.Verdict, bool) {
	if m.configMgr != nil {
		cfg := m.configMgr.GetWorktreeConfig()
		if cfg.HookEnabled != nil && !*cfg.HookEnabled {
			return "", 0, false
		}
	}
	scriptPath := filepath.Join(workDir, ".jin", "worktree-post-create.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		return "", 0, false
	}
	allowlist, err := worktreehook.LoadAllowlist(paths.State())
	if err != nil {
		return "", 0, false
	}
	absRepo, err := filepath.Abs(workDir)
	if err != nil {
		return "", 0, false
	}
	sha, err := worktreehook.ComputeSHA256(scriptPath)
	if err != nil {
		return "", 0, false
	}
	entry, ok := allowlist.Get(absRepo)
	switch {
	case !ok:
		return scriptPath, worktreehook.VerdictNotAllowed, true
	case entry.SHA256 != sha:
		return scriptPath, worktreehook.VerdictChanged, true
	default:
		return scriptPath, worktreehook.VerdictOK, true
	}
}

// submitWith returns the tea.Cmd that dispatches the actual daemon call.
// noHook is passed through to skip the post-create hook. Caller is
// responsible for setting m.processingMsg before dispatching.
//
// Description is intentionally sent empty: the daemon populates it via
// Layer A at creation, and Layer C promotes it from the first user prompt
// once the transcript is flushed.
func (m CreateFormModel) submitWith(noHook bool) tea.Cmd {
	workDir := m.dirPicker.Result()
	fleet := strings.TrimSpace(m.fleetInput.Value())
	client := m.client
	worktree := m.worktreeEnabled
	return func() tea.Msg {
		s, warning, err := client.NewWithOptions(daemon.NewOptions{
			WorkDir:  workDir,
			Start:    true,
			Fleet:    fleet,
			Worktree: worktree,
			NoHook:   noHook,
		})
		if err != nil {
			return createFormCompleteMsg{err: err}
		}
		return createFormCompleteMsg{sessionID: s.ID, warning: warning}
	}
}

// --- Internal helpers ---

// convertDirHistoryEntries converts config.DirHistoryEntry to tui.HistoryEntry,
// applying display path formatting (~ for home directory).
func convertDirHistoryEntries(entries []config.DirHistoryEntry) []HistoryEntry {
	home, _ := os.UserHomeDir()
	result := make([]HistoryEntry, 0, len(entries))
	for _, e := range entries {
		displayPath := e.Path
		if home != "" && strings.HasPrefix(displayPath, home) {
			displayPath = "~" + displayPath[len(home):]
		}
		result = append(result, HistoryEntry{
			Path:        e.Path,
			DisplayPath: displayPath,
			LastUsedAt:  e.LastUsedAt,
		})
	}
	return result
}
