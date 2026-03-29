package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/takaaki-s/claude-code-valet/internal/config"
	"github.com/takaaki-s/claude-code-valet/internal/daemon"
	"github.com/takaaki-s/claude-code-valet/internal/host"
	"github.com/takaaki-s/claude-code-valet/internal/session"
	"github.com/takaaki-s/claude-code-valet/internal/tmux"
)

// maxTUIWidth is the maximum width (columns) for the TUI pane.
// When the terminal is maximized, the TUI pane is resized to this width
// so the display pane gets the extra space.
const maxTUIWidth = 50
const minTUIWidth = 30

// KeyMap defines key bindings
type KeyMap struct {
	Up            key.Binding
	Down          key.Binding
	Enter         key.Binding
	New           key.Binding
	Kill          key.Binding
	Delete        key.Binding
	Refresh       key.Binding
	Quit          key.Binding
	Help          key.Binding
	PrevPage      key.Binding
	NextPage      key.Binding
	Search        key.Binding
	Vscode        key.Binding
	Notifications key.Binding

	// Session creation form
	NextField  key.Binding
	PrevField  key.Binding
	Submit     key.Binding
	CancelForm key.Binding
}

// NewKeyMap creates a KeyMap from config
func NewKeyMap(cfg config.KeybindingsConfig) KeyMap {
	return KeyMap{
		Up: key.NewBinding(
			key.WithKeys(cfg.Up...),
			key.WithHelp(strings.Join(cfg.Up, "/"), "up"),
		),
		Down: key.NewBinding(
			key.WithKeys(cfg.Down...),
			key.WithHelp(strings.Join(cfg.Down, "/"), "down"),
		),
		Enter: key.NewBinding(
			key.WithKeys(cfg.Attach...),
			key.WithHelp(strings.Join(cfg.Attach, "/"), "attach"),
		),
		New: key.NewBinding(
			key.WithKeys(cfg.New...),
			key.WithHelp(strings.Join(cfg.New, "/"), "new session"),
		),
		Kill: key.NewBinding(
			key.WithKeys(cfg.Kill...),
			key.WithHelp(strings.Join(cfg.Kill, "/"), "kill"),
		),
		Delete: key.NewBinding(
			key.WithKeys(cfg.Delete...),
			key.WithHelp(strings.Join(cfg.Delete, "/"), "delete"),
		),
		Refresh: key.NewBinding(
			key.WithKeys(cfg.Refresh...),
			key.WithHelp(strings.Join(cfg.Refresh, "/"), "refresh"),
		),
		Quit: key.NewBinding(
			key.WithKeys(cfg.Quit...),
			key.WithHelp(strings.Join(cfg.Quit, "/"), "quit"),
		),
		Help: key.NewBinding(
			key.WithKeys(cfg.Help...),
			key.WithHelp(strings.Join(cfg.Help, "/"), "help"),
		),
		PrevPage: key.NewBinding(
			key.WithKeys("left", "h"),
			key.WithHelp("←/h", "prev page"),
		),
		NextPage: key.NewBinding(
			key.WithKeys("right", "l"),
			key.WithHelp("→/l", "next page"),
		),
		Search: key.NewBinding(
			key.WithKeys(cfg.Search...),
			key.WithHelp(strings.Join(cfg.Search, "/"), "search"),
		),
		Vscode: key.NewBinding(
			key.WithKeys(cfg.Vscode...),
			key.WithHelp(strings.Join(cfg.Vscode, "/"), "open vscode"),
		),
		Notifications: key.NewBinding(
			key.WithKeys(cfg.Notifications...),
			key.WithHelp(strings.Join(cfg.Notifications, "/"), "notifications"),
		),
		NextField: key.NewBinding(
			key.WithKeys(cfg.NextField...),
			key.WithHelp(strings.Join(cfg.NextField, "/"), "next field"),
		),
		PrevField: key.NewBinding(
			key.WithKeys(cfg.PrevField...),
			key.WithHelp(strings.Join(cfg.PrevField, "/"), "prev field"),
		),
		Submit: key.NewBinding(
			key.WithKeys(cfg.Submit...),
			key.WithHelp(strings.Join(cfg.Submit, "/"), "submit"),
		),
		CancelForm: key.NewBinding(
			key.WithKeys(cfg.CancelForm...),
			key.WithHelp(strings.Join(cfg.CancelForm, "/"), "cancel"),
		),
	}
}

// Model is the TUI model
type Model struct {
	client   *daemon.Client
	sessions []session.Info
	cursor   int
	width    int
	height   int
	err      error
	keys     KeyMap // Keybinding settings

	// Config manager (used for remote session attach)
	configMgr *config.Manager

	// Pagination
	currentPage int // Current page (0-indexed)

	// Delete confirmation
	confirmDelete          bool   // Whether delete confirmation is active
	deleteTargetID         string // Session ID to delete
	deleteTargetName       string // Session name to delete (for display)
	deleteTargetHostID     string // Host ID of session to delete
	deleteTargetIsWorktree bool   // Whether the session is in a git worktree
	confirmWorktreeForce   bool   // Whether force-delete worktree confirmation is active

	// Kill confirmation
	confirmKill      bool   // Whether kill confirmation is active
	killTargetID     string // Session ID to kill
	killTargetName   string // Session name to kill (for display)
	killTargetHostID string // Host ID of session to kill

	// Focus tracking (for visual focus indicator)
	focused bool // true when TUI pane has focus (changes border/title color)

	// tmux integration
	tmuxClient         *tmux.Client // outer tmux client (-L ccvalet-mgr, nil in legacy mode)
	innerTmuxClient    *tmux.Client // inner tmux client (-L ccvalet, for switch-client)
	tuiPaneID          string       // TUI pane unique ID (e.g. "%42") in outer tmux
	displayPaneID      string       // Right pane unique ID (for session display) in outer tmux
	currentSessionID   string       // Session ID currently displayed in right pane
	switchSeq          int          // Sequence number for cursor movement debounce
	displayLocalAttach bool         // true when display pane is running tmux attach to inner tmux

	// Focus after create
	focusSessionID string // Session ID to focus after creation

	// Reswitch after delete/kill
	needsReswitch bool // Reconnect to session at cursor after delete/kill

	// Processing indicator
	processingMsg    string // Processing message (overlay displayed when non-empty)
	waitingForResize bool   // Waiting for WindowSizeMsg (resize completion after ZoomPane)

	// Search/Filter mode
	searching        bool            // true when search mode is active
	searchInput      textinput.Model // text input for search query
	filteredSessions []session.Info  // filtered result (nil when not searching)
}

// NewModel creates a new TUI model
func NewModel(client *daemon.Client) Model {
	// Initialize config manager
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".ccvalet")
	configMgr, _ := config.NewManager(configDir)

	// Initialize keybindings
	var keybindings config.KeybindingsConfig
	if configMgr != nil {
		keybindings = configMgr.GetKeybindings()
	} else {
		keybindings = config.DefaultKeybindings()
	}
	keys := NewKeyMap(keybindings)

	si := textinput.New()
	si.Placeholder = "search..."
	si.CharLimit = 100
	si.Width = maxTUIWidth - 10

	return Model{
		client:      client,
		keys:        keys,
		focused:     true,
		configMgr:   configMgr,
		searchInput: si,
	}
}

// NewModelWithTmux creates a new TUI model with tmux integration.
// The outer tmux (-L ccvalet-mgr) has a fixed 2-pane layout:
// left pane (TUI) + right pane (session display via RespawnPane).
func NewModelWithTmux(client *daemon.Client, tc, innerTC *tmux.Client, tuiPaneID, displayPaneID string) Model {
	m := NewModel(client)
	m.tmuxClient = tc
	m.innerTmuxClient = innerTC
	m.tuiPaneID = tuiPaneID
	m.displayPaneID = displayPaneID
	// Restore which session was displayed (for reattach)
	m.currentSessionID = tc.GetEnvironment(tmux.SessionName, "CCVALET_CURRENT_SESSION")
	return m
}

// getItemsPerPage calculates how many items fit on one page
func (m *Model) getItemsPerPage() int {
	// Subtract header lines (title, stats, separator, footer)
	// Header: 3 lines, Footer: 2 lines (page info + help)
	availableLines := m.height - 8
	if m.searching {
		availableLines-- // search bar takes 1 line
	}
	// Reserve space for fleet group headers (always shown when sessions exist).
	if n := m.fleetGroupCount(); n >= 1 {
		availableLines -= n
	}
	availableLines = max(availableLines, 4)
	// Each session takes ~4 lines (name + status + meta + time)
	items := availableLines / 4
	items = max(items, 1)
	return items
}

// fleetGroupCount returns the number of distinct fleet groups in m.sessions.
func (m *Model) fleetGroupCount() int {
	seen := make(map[string]bool)
	for _, s := range m.sessions {
		seen[s.Fleet] = true
	}
	return len(seen)
}

// getTotalPages calculates the total number of pages
func (m *Model) getTotalPages() int {
	sessions := m.getDisplaySessions()
	if len(sessions) == 0 {
		return 1
	}
	itemsPerPage := m.getItemsPerPage()
	totalPages := (len(sessions) + itemsPerPage - 1) / itemsPerPage
	totalPages = max(totalPages, 1)
	return totalPages
}

// getPageSessions returns sessions for the current page
func (m *Model) getPageSessions() []session.Info {
	sessions := m.getDisplaySessions()
	if len(sessions) == 0 {
		return nil
	}
	itemsPerPage := m.getItemsPerPage()
	start := m.currentPage * itemsPerPage
	end := start + itemsPerPage
	if start >= len(sessions) {
		start = 0
		m.currentPage = 0
		end = itemsPerPage
	}
	if end > len(sessions) {
		end = len(sessions)
	}
	return sessions[start:end]
}

// getDisplaySessions returns the sessions to display:
// filteredSessions when searching, sessions otherwise.
func (m *Model) getDisplaySessions() []session.Info {
	if m.searching && m.filteredSessions != nil {
		return m.filteredSessions
	}
	return m.sessions
}

// matchesSearch returns true if the session matches the search query
// across any of the target fields (Name, WorkDir, CurrentWorkDir, CurrentBranch).
func matchesSearch(sess session.Info, query string) bool {
	fields := []string{
		sess.Name,
		sess.WorkDir,
		sess.CurrentWorkDir,
		sess.CurrentBranch,
		sess.Fleet,
	}
	for _, field := range fields {
		if field != "" && strings.Contains(strings.ToLower(field), query) {
			return true
		}
	}
	return false
}

// applySearchFilter filters m.sessions using the current search query
// and stores the result in m.filteredSessions.
func (m *Model) applySearchFilter() {
	query := strings.ToLower(m.searchInput.Value())
	if query == "" {
		m.filteredSessions = m.sessions
		return
	}
	m.filteredSessions = make([]session.Info, 0)
	for _, sess := range m.sessions {
		if matchesSearch(sess, query) {
			m.filteredSessions = append(m.filteredSessions, sess)
		}
	}
}

// Messages
type sessionsMsg []session.Info
type errMsg error
type tickMsg time.Time
type worktreeDirtyMsg struct {
	sessionID string
	hostID    string
	name      string
}

// Commands
func (m *Model) fetchSessions() tea.Msg {
	sessions, err := m.client.List()
	if err != nil {
		return errMsg(err)
	}
	return sessionsMsg(sessions)
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// cursorSettledMsg is sent after a debounce delay when the cursor stops moving.
type cursorSettledMsg struct {
	seq int
}

func cursorSettledCmd(seq int) tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg {
		return cursorSettledMsg{seq: seq}
	})
}

// resizeSettledMsg is sent after a delay to allow WindowSizeMsg to arrive
// after tmux pane operations (ZoomPane).
type resizeSettledMsg struct{}

// switchToSession displays the given session in the right pane via RespawnPane.
// For local sessions, attaches to the inner tmux session (-L ccvalet).
// For remote sessions, runs SSH attach command.
// For stopped/error sessions, shows a placeholder with session info.
func (m *Model) switchToSession(sessionID string) {
	if m.tmuxClient == nil || m.displayPaneID == "" || sessionID == "" {
		return
	}

	// Already displaying this session
	if m.currentSessionID == sessionID {
		return
	}

	// Find session info
	var sess *session.Info
	for i := range m.sessions {
		if m.sessions[i].ID == sessionID {
			sess = &m.sessions[i]
			break
		}
	}
	if sess == nil {
		return
	}

	// Determine if the target is a local alive session
	isLocalAlive := isSessionAlive(sess.Status) && sess.TmuxWindowName != "" &&
		(sess.HostID == "" || sess.HostID == "local")

	// When switching away from a local attach, detach the inner tmux client first
	// so that "tmux attach" exits cleanly and avoids "pane is dead".
	if !isLocalAlive && m.displayLocalAttach {
		m.detachInnerClient()
		m.displayLocalAttach = false
	}

	// Stopped/error sessions: show placeholder in right pane (no TmuxWindowName needed)
	if !isSessionAlive(sess.Status) {
		var placeholderCmd string
		if sess.ErrorMessage != "" {
			placeholderCmd = fmt.Sprintf(
				"printf '\\n  Session: %s\\n  Status:  %s\\n\\n  Error:\\n%s\\n'; tail -f /dev/null",
				sess.Name, sess.Status, sess.ErrorMessage,
			)
		} else {
			placeholderCmd = fmt.Sprintf(
				"printf '\\n  Session: %s\\n  Status:  %s\\n\\n  Press Enter to restart\\n'; tail -f /dev/null",
				sess.Name, sess.Status,
			)
		}
		_ = m.tmuxClient.RespawnPane(m.displayPaneID, placeholderCmd)
		m.currentSessionID = sessionID
		_ = m.tmuxClient.SetEnvironment(tmux.SessionName, "CCVALET_CURRENT_SESSION", sessionID)
		_ = m.tmuxClient.SetPaneOption(m.displayPaneID, "@session_name", sess.Name)
		return
	}

	// Running sessions require TmuxWindowName for inner tmux attach
	if sess.TmuxWindowName == "" {
		return
	}

	// Remote session: use SSH attach command
	if sess.HostID != "" && sess.HostID != "local" {
		m.switchToRemoteSession(sess)
		_ = m.tmuxClient.SetPaneOption(m.displayPaneID, "@session_name", sess.Name)
		return
	}

	// Local alive session: prefer switch-client over respawn-pane to avoid "pane is dead"
	if m.displayLocalAttach && m.innerTmuxClient != nil {
		paneTTY, err := m.tmuxClient.GetPaneTTY(m.displayPaneID)
		if err == nil && paneTTY != "" {
			if m.innerTmuxClient.SwitchClient(paneTTY, sess.TmuxWindowName) == nil {
				m.currentSessionID = sessionID
				_ = m.tmuxClient.SetEnvironment(tmux.SessionName, "CCVALET_CURRENT_SESSION", sessionID)
				_ = m.tmuxClient.SetPaneOption(m.displayPaneID, "@session_name", sess.Name)
				return
			}
		}
		// switch-client failed — fall through to respawn
	}

	// Local: respawn right pane with inner tmux attach.
	// Unset $TMUX so tmux does not refuse with "sessions should be nested with care":
	// the display pane runs inside the outer tmux (ccvalet-mgr), so $TMUX points to
	// the outer session. Without env -u TMUX, attaching to the inner tmux (ccvalet)
	// on the same host is rejected as nesting. This mirrors the env -u TMUX pattern
	// used in session/manager.go when launching CC processes.
	attachCmd := fmt.Sprintf("env -u TMUX tmux -L %s attach -t %s", tmux.SocketName, sess.TmuxWindowName)
	_ = m.tmuxClient.RespawnPane(m.displayPaneID, attachCmd)
	m.displayLocalAttach = true

	m.currentSessionID = sessionID
	_ = m.tmuxClient.SetEnvironment(tmux.SessionName, "CCVALET_CURRENT_SESSION", sessionID)
	_ = m.tmuxClient.SetPaneOption(m.displayPaneID, "@session_name", sess.Name)
}

// detachInnerClient detaches the inner tmux client running in the display pane.
// This makes the "tmux attach" process exit cleanly, preventing "pane is dead".
func (m *Model) detachInnerClient() {
	if m.innerTmuxClient == nil {
		return
	}
	paneTTY, err := m.tmuxClient.GetPaneTTY(m.displayPaneID)
	if err != nil || paneTTY == "" {
		return
	}
	_ = m.innerTmuxClient.DetachClientByTTY(paneTTY)
}

// respawnPlaceholder replaces the display pane with a placeholder command.
// Detaches any active inner tmux client first to avoid "pane is dead".
func (m *Model) respawnPlaceholder() {
	if m.tmuxClient == nil || m.displayPaneID == "" {
		return
	}
	if m.displayLocalAttach {
		m.detachInnerClient()
		m.displayLocalAttach = false
	}
	_ = m.tmuxClient.RespawnPane(m.displayPaneID, tmux.PlaceholderCmd)
}

// isSessionAlive returns true if the session status indicates an active process.
func isSessionAlive(status session.Status) bool {
	switch status {
	case session.StatusRunning, session.StatusThinking, session.StatusIdle,
		session.StatusPermission, session.StatusCreating:
		return true
	}
	return false
}

// switchToRemoteSession displays a remote session in the right pane via RespawnPane.
func (m *Model) switchToRemoteSession(sess *session.Info) {
	if m.configMgr == nil {
		return
	}

	hostConfig := m.configMgr.GetHost(sess.HostID)
	if hostConfig == nil {
		return
	}

	// Ensure a background ControlMaster SSH connection exists for this host.
	// This is separate from the tmux pane process, so RespawnPane won't kill it.
	// Subsequent SSH connections (slaves) reuse the master for near-instant connection.
	_ = host.EnsureSSHMaster(*hostConfig)

	// Generate SSH attach command string (slave connection via ControlMaster)
	attachCmd := host.AttachCommandString(*hostConfig, sess.TmuxWindowName)
	_ = m.tmuxClient.RespawnPane(m.displayPaneID, attachCmd)

	m.currentSessionID = sess.ID
	_ = m.tmuxClient.SetEnvironment(tmux.SessionName, "CCVALET_CURRENT_SESSION", sess.ID)
}

// openVSCode opens VS Code for the given session's working directory.
// For local sessions: code <path>
// For SSH remote sessions: code --remote ssh-remote+<host> <path>
func (m *Model) openVSCode(sess *session.Info) {
	workDir := sess.CurrentWorkDir
	if workDir == "" {
		workDir = sess.WorkDir
	}
	if workDir == "" {
		return
	}

	// Remote session (SSH)
	if sess.HostID != "" && sess.HostID != "local" {
		if m.configMgr == nil {
			return
		}
		hostConfig := m.configMgr.GetHost(sess.HostID)
		if hostConfig == nil || hostConfig.Type != "ssh" {
			return
		}
		_ = exec.Command("code", "--remote", "ssh-remote+"+hostConfig.Host, workDir).Start()
		return
	}

	// Local session
	_ = exec.Command("code", workDir).Start()
}

// handleAttach attaches to the currently selected session.
func (m Model) handleAttach() (tea.Model, tea.Cmd) {
	pageSessions := m.getPageSessions()
	if len(pageSessions) == 0 || m.cursor >= len(pageSessions) {
		return m, nil
	}
	sess := pageSessions[m.cursor]

	if sess.Status == session.StatusCreating {
		m.err = fmt.Errorf("cannot attach to creating session")
		return m, nil
	}

	if m.tmuxClient != nil {
		needsStart := sess.Status == session.StatusStopped
		if needsStart {
			if err := m.client.Start(sess.ID, sess.HostID); err != nil {
				m.err = err
				return m, nil
			}
			for i := range m.sessions {
				if m.sessions[i].ID == sess.ID {
					if m.sessions[i].TmuxWindowName == "" {
						m.sessions[i].TmuxWindowName = tmux.InnerSessionName(sess.ID)
					}
					m.sessions[i].Status = session.StatusRunning
					break
				}
			}
			m.currentSessionID = ""
		}
		m.switchToSession(sess.ID)
		if m.displayPaneID != "" {
			_ = m.tmuxClient.SelectPane(m.displayPaneID)
		}
		return m, m.fetchSessions
	}
	return m, nil
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.fetchSessions,
		tickCmd(),
	)
}

// Update handles messages
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Handle window size for all modes
	if msg, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = msg.Width
		m.height = msg.Height
		// TUI pane width control: cap at max, enforce minimum.
		if m.tmuxClient != nil && m.tuiPaneID != "" && m.displayPaneID != "" {
			if m.width > maxTUIWidth {
				_ = m.tmuxClient.ResizePaneWidth(m.tuiPaneID, maxTUIWidth)
			} else if m.width < minTUIWidth {
				_ = m.tmuxClient.ResizePaneWidth(m.tuiPaneID, minTUIWidth)
			}
		}
		// Detect resize completion after ZoomPane
		// WindowSizeMsg arrived = pane size is finalized → clear processingMsg and redraw
		if m.waitingForResize {
			m.waitingForResize = false
			m.processingMsg = ""
			return m, tea.ClearScreen
		}
	}

	// Handle focus events (from tmux focus-events + tea.WithReportFocus)
	if _, ok := msg.(tea.FocusMsg); ok {
		m.focused = true
		return m, nil
	}
	if _, ok := msg.(tea.BlurMsg); ok {
		m.focused = false
		return m, nil
	}

	// Ignore key input while processing, only handle completion messages
	if m.processingMsg != "" {
		switch msg.(type) {
		case tea.KeyMsg:
			return m, nil
		}
	}

	return m.updateListMode(msg)
}

func (m Model) updateListMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Handle delete confirmation mode
		if m.confirmDelete {
			// Sub-confirmation: force delete dirty worktree
			if m.confirmWorktreeForce {
				switch msg.String() {
				case "y", "Y":
					m.processingMsg = "Deleting with worktree..."
					m.needsReswitch = true
					deleteID := m.deleteTargetID
					deleteHostID := m.deleteTargetHostID
					m.resetDeleteState()
					client := m.client
					return m, func() tea.Msg {
						if err := client.Delete(deleteID, deleteHostID, true, true); err != nil {
							return errMsg(fmt.Errorf("delete failed: %w", err))
						}
						sessions, err := client.List()
						if err != nil {
							return errMsg(err)
						}
						return sessionsMsg(sessions)
					}
				case "n", "N", "esc":
					// Fall back: delete session only
					m.processingMsg = "Deleting..."
					m.needsReswitch = true
					deleteID := m.deleteTargetID
					deleteHostID := m.deleteTargetHostID
					m.resetDeleteState()
					client := m.client
					return m, func() tea.Msg {
						if err := client.Delete(deleteID, deleteHostID, false, false); err != nil {
							return errMsg(fmt.Errorf("delete failed: %w", err))
						}
						sessions, err := client.List()
						if err != nil {
							return errMsg(err)
						}
						return sessionsMsg(sessions)
					}
				}
				return m, nil
			}

			// Primary delete confirmation
			switch msg.String() {
			case "y", "Y", "enter":
				m.processingMsg = "Deleting..."
				m.needsReswitch = true
				deleteID := m.deleteTargetID
				deleteHostID := m.deleteTargetHostID
				m.resetDeleteState()
				client := m.client
				return m, func() tea.Msg {
					if err := client.Delete(deleteID, deleteHostID, false, false); err != nil {
						return errMsg(fmt.Errorf("delete failed: %w", err))
					}
					sessions, err := client.List()
					if err != nil {
						return errMsg(err)
					}
					return sessionsMsg(sessions)
				}
			case "w", "W":
				if !m.deleteTargetIsWorktree {
					return m, nil // ignore if not a worktree
				}
				m.processingMsg = "Deleting with worktree..."
				m.needsReswitch = true
				deleteID := m.deleteTargetID
				deleteHostID := m.deleteTargetHostID
				deleteName := m.deleteTargetName
				m.resetDeleteState()
				client := m.client
				return m, func() tea.Msg {
					err := client.Delete(deleteID, deleteHostID, true, false)
					if err != nil {
						if errors.Is(err, session.ErrWorktreeDirty) {
							return worktreeDirtyMsg{sessionID: deleteID, hostID: deleteHostID, name: deleteName}
						}
						return errMsg(fmt.Errorf("delete failed: %w", err))
					}
					sessions, err := client.List()
					if err != nil {
						return errMsg(err)
					}
					return sessionsMsg(sessions)
				}
			case "n", "N", "esc":
				m.resetDeleteState()
				return m, nil
			}
			return m, nil
		}

		// Handle kill confirmation mode
		if m.confirmKill {
			switch msg.String() {
			case "y", "Y", "enter":
				m.processingMsg = "Stopping..."
				m.confirmKill = false
				m.needsReswitch = true

				killID := m.killTargetID
				killHostID := m.killTargetHostID
				m.killTargetID = ""
				m.killTargetName = ""
				m.killTargetHostID = ""

				client := m.client

				return m, func() tea.Msg {
					if err := client.Kill(killID, killHostID); err != nil {
						return errMsg(fmt.Errorf("kill failed: %w", err))
					}
					sessions, err := client.List()
					if err != nil {
						return errMsg(err)
					}
					return sessionsMsg(sessions)
				}
			case "n", "N", "esc":
				m.confirmKill = false
				m.killTargetID = ""
				m.killTargetName = ""
				m.killTargetHostID = ""
				return m, nil
			}
			return m, nil
		}

		// Search mode key handling
		if m.searching {
			switch msg.String() {
			case "esc":
				m.searching = false
				m.searchInput.Reset()
				m.filteredSessions = nil
				m.currentPage = 0
				m.cursor = 0
				return m, nil

			case "up":
				if m.cursor > 0 {
					m.cursor--
				}
				m.switchSeq++
				return m, cursorSettledCmd(m.switchSeq)

			case "down":
				pageSessions := m.getPageSessions()
				if m.cursor < len(pageSessions)-1 {
					m.cursor++
				}
				m.switchSeq++
				return m, cursorSettledCmd(m.switchSeq)

			case "enter":
				m.switchSeq++
				return m.handleAttach()

			case "left":
				if m.currentPage > 0 {
					m.currentPage--
					m.cursor = 0
				}
				return m, nil

			case "right":
				totalPages := m.getTotalPages()
				if m.currentPage < totalPages-1 {
					m.currentPage++
					m.cursor = 0
				}
				return m, nil
			}

			// All other keys go to textinput
			var cmd tea.Cmd
			oldQuery := m.searchInput.Value()
			m.searchInput, cmd = m.searchInput.Update(msg)
			if m.searchInput.Value() != oldQuery {
				m.applySearchFilter()
				m.currentPage = 0
				m.cursor = 0
			}
			return m, cmd
		}

		// Left pane key handling
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit

		case key.Matches(msg, m.keys.Up):
			if m.cursor > 0 {
				m.cursor--
			}
			m.switchSeq++
			return m, cursorSettledCmd(m.switchSeq)

		case key.Matches(msg, m.keys.Down):
			pageSessions := m.getPageSessions()
			if m.cursor < len(pageSessions)-1 {
				m.cursor++
			}
			m.switchSeq++
			return m, cursorSettledCmd(m.switchSeq)

		case key.Matches(msg, m.keys.Enter):
			m.switchSeq++
			return m.handleAttach()

		case key.Matches(msg, m.keys.Search):
			m.searching = true
			m.searchInput.Reset()
			m.searchInput.Focus()
			m.filteredSessions = m.sessions
			m.currentPage = 0
			m.cursor = 0
			return m, textinput.Blink

		case key.Matches(msg, m.keys.New):
			// Open session creation form in a tmux popup
			if m.tmuxClient != nil {
				selfBin, _ := os.Executable()
				_ = m.tmuxClient.DisplayPopup(tmux.DisplayPopupOptions{
					Width:  "80%",
					Height: "80%",
					Cmd:    fmt.Sprintf("'%s' create-popup", selfBin),
					Title:  " New Session ",
				})
			}
			return m, nil

		case key.Matches(msg, m.keys.Kill):
			pageSessions := m.getPageSessions()
			if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
				sess := pageSessions[m.cursor]
				// Enter confirmation mode
				m.confirmKill = true
				m.killTargetID = sess.ID
				m.killTargetName = sess.Name
				m.killTargetHostID = sess.HostID
				return m, nil
			}

		case key.Matches(msg, m.keys.Delete):
			pageSessions := m.getPageSessions()
			if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
				sess := pageSessions[m.cursor]
				// Enter confirmation mode
				m.confirmDelete = true
				m.deleteTargetID = sess.ID
				m.deleteTargetName = sess.Name
				m.deleteTargetHostID = sess.HostID
				m.deleteTargetIsWorktree = sess.IsWorktree
				return m, nil
			}

		case key.Matches(msg, m.keys.Refresh):
			return m, m.fetchSessions

		case key.Matches(msg, m.keys.Help):
			if m.tmuxClient != nil {
				selfBin, _ := os.Executable()
				_ = m.tmuxClient.DisplayPopup(tmux.DisplayPopupOptions{
					Width:  "60%",
					Height: "60%",
					Cmd:    fmt.Sprintf("'%s' help-popup", selfBin),
					Title:  " Shortcuts ",
				})
			}
			return m, nil

		case key.Matches(msg, m.keys.PrevPage):
			if m.currentPage > 0 {
				m.currentPage--
				m.cursor = 0
			}
			return m, nil

		case key.Matches(msg, m.keys.NextPage):
			totalPages := m.getTotalPages()
			if m.currentPage < totalPages-1 {
				m.currentPage++
				m.cursor = 0
			}
			return m, nil

		case key.Matches(msg, m.keys.Vscode):
			pageSessions := m.getPageSessions()
			if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
				sess := pageSessions[m.cursor]
				go m.openVSCode(&sess)
			}
			return m, nil

		case key.Matches(msg, m.keys.Notifications):
			if m.tmuxClient != nil {
				selfBin, _ := os.Executable()
				_ = m.tmuxClient.DisplayPopup(tmux.DisplayPopupOptions{
					Width:  "70%",
					Height: "60%",
					Cmd:    fmt.Sprintf("'%s' notify-popup", selfBin),
					Title:  " Notifications ",
				})
			}
			return m, nil
		}

	case sessionsMsg:
		m.sessions = msg
		m.err = nil

		// Re-apply search filter if active
		if m.searching {
			m.applySearchFilter()
		}

		// Focus on newly created session + switch right pane
		if m.focusSessionID != "" {
			// Clear search to show the newly created session
			m.searching = false
			m.searchInput.Reset()
			m.filteredSessions = nil

			itemsPerPage := m.getItemsPerPage()
			for i, s := range m.sessions {
				if s.ID == m.focusSessionID {
					m.currentPage = i / itemsPerPage
					m.cursor = i % itemsPerPage
					m.currentSessionID = "" // Force reset to execute switchToSession
					m.switchToSession(s.ID)
					break
				}
			}
			m.focusSessionID = ""
			return m, nil
		}
		displaySessions := m.getDisplaySessions()
		if m.cursor >= len(displaySessions) && m.cursor > 0 {
			m.cursor = len(displaySessions) - 1
		}
		// Reconnect to session at cursor after delete/kill
		if m.needsReswitch {
			m.needsReswitch = false
			m.currentSessionID = "" // Force reset
			if m.displayLocalAttach {
				m.detachInnerClient()
				m.displayLocalAttach = false
			}
			pageSessions := m.getPageSessions()
			if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
				m.switchToSession(pageSessions[m.cursor].ID)
			} else {
				m.respawnPlaceholder()
			}
			m.processingMsg = ""
			return m, nil
		}
		// Reset right pane to placeholder when sessions become empty
		// Even with empty currentSessionID, stale content may remain in right pane,
		// so run RespawnPane only once and set "_empty" to skip subsequent calls
		if len(m.sessions) == 0 {
			if m.currentSessionID != "_empty" {
				m.currentSessionID = "_empty"
				m.respawnPlaceholder()
			}
			m.processingMsg = ""
			return m, nil
		}
		// Reset right pane when the currently displayed session disappears during polling
		if m.currentSessionID != "" {
			found := false
			for _, s := range m.sessions {
				if s.ID == m.currentSessionID {
					found = true
					break
				}
			}
			if !found {
				m.currentSessionID = ""
				pageSessions := m.getPageSessions()
				if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
					m.switchToSession(pageSessions[m.cursor].ID)
				} else {
					m.respawnPlaceholder()
				}
			}
		}
		m.processingMsg = ""
		return m, nil

	case resizeSettledMsg:
		// Fallback: WindowSizeMsg did not arrive (no pane size change)
		if m.waitingForResize {
			m.waitingForResize = false
			m.processingMsg = ""
			return m, tea.ClearScreen
		}
		return m, nil

	case cursorSettledMsg:
		if msg.seq != m.switchSeq {
			return m, nil
		}
		pageSessions := m.getPageSessions()
		if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
			sess := pageSessions[m.cursor]
			m.switchToSession(sess.ID)
		}
		return m, nil

	case worktreeDirtyMsg:
		m.processingMsg = ""
		m.confirmDelete = true
		m.confirmWorktreeForce = true
		m.deleteTargetID = msg.sessionID
		m.deleteTargetHostID = msg.hostID
		m.deleteTargetName = msg.name
		m.deleteTargetIsWorktree = true

	case errMsg:
		m.processingMsg = ""
		m.err = msg

	case tickMsg:
		// Poll for session created via tmux popup
		if m.tmuxClient != nil {
			if id := m.tmuxClient.GetEnvironment(tmux.SessionName, "CCVALET_CREATED_SESSION"); id != "" {
				_ = m.tmuxClient.UnsetEnvironment(tmux.SessionName, "CCVALET_CREATED_SESSION")
				m.focusSessionID = id
			}
			// Poll for session selected from notification history popup
			if id := m.tmuxClient.GetEnvironment(tmux.SessionName, "CCVALET_NOTIFY_SESSION"); id != "" {
				_ = m.tmuxClient.UnsetEnvironment(tmux.SessionName, "CCVALET_NOTIFY_SESSION")
				m.focusSessionID = id
			}
		}
		return m, tea.Batch(m.fetchSessions, tickCmd())
	}

	return m, nil
}

// View renders the UI
func (m Model) View() string {
	// Processing indicator
	if m.processingMsg != "" {
		return m.renderProcessingView()
	}

	// Delete confirmation overlay
	if m.confirmDelete {
		return m.renderDeleteConfirm()
	}

	boxWidth := m.width - 2
	boxWidth = max(boxWidth, 20)
	boxHeight := m.height - 3
	boxHeight = max(boxHeight, 5)
	boxStyle := createBoxStyle(boxWidth, boxHeight, m.focused)
	box := boxStyle.Render(m.renderListContent(boxWidth - 4))
	helpLine := m.renderHelpLine()
	return box + "\n" + helpLine
}

// renderProcessingView renders a processing indicator.
// Size-independent: renders correctly even before WindowSizeMsg arrives after ZoomPane/JoinPane
func (m Model) renderProcessingView() string {
	return "\n  ⟳ " + m.processingMsg
}

func (m *Model) resetDeleteState() {
	m.confirmDelete = false
	m.confirmWorktreeForce = false
	m.deleteTargetID = ""
	m.deleteTargetName = ""
	m.deleteTargetHostID = ""
	m.deleteTargetIsWorktree = false
}

// renderDeleteConfirm renders a delete confirmation dialog as a box overlay.
func (m Model) renderDeleteConfirm() string {
	boxWidth := m.width - 2
	boxWidth = max(boxWidth, 20)
	boxHeight := m.height - 3
	boxHeight = max(boxHeight, 5)

	contentWidth := boxWidth - 4 // padding inside box
	var lines []string

	warnStyle := lipgloss.NewStyle().Foreground(warningColor).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(secondaryColor)
	keyStyle := lipgloss.NewStyle().Foreground(primaryColor).Bold(true)

	if m.confirmWorktreeForce {
		// Force confirmation for dirty worktree
		lines = append(lines,
			warnStyle.Render("⚠ Worktree has uncommitted"),
			warnStyle.Render("  changes"),
			"",
			keyStyle.Render("y")+dimStyle.Render(": force delete worktree"),
			keyStyle.Render("n")+dimStyle.Render(": delete session only"),
		)
	} else if m.deleteTargetIsWorktree {
		// Worktree session
		name := truncateString(m.deleteTargetName, contentWidth-10)
		lines = append(lines,
			warnStyle.Render(fmt.Sprintf("Delete '%s'?", name)),
			dimStyle.Render("Session is in a git worktree"),
			"",
			keyStyle.Render("y")+dimStyle.Render(": delete session only"),
			keyStyle.Render("w")+dimStyle.Render(": delete session + worktree"),
			keyStyle.Render("n")+dimStyle.Render(": cancel"),
		)
	} else {
		// Normal session
		name := truncateString(m.deleteTargetName, contentWidth-10)
		lines = append(lines,
			warnStyle.Render(fmt.Sprintf("Delete '%s'?", name)),
			"",
			keyStyle.Render("y")+dimStyle.Render(": delete"),
			keyStyle.Render("n")+dimStyle.Render(": cancel"),
		)
	}

	content := strings.Join(lines, "\n")
	placed := lipgloss.Place(contentWidth, boxHeight, lipgloss.Center, lipgloss.Center, content)

	boxStyle := createBoxStyle(boxWidth, boxHeight, m.focused)
	return boxStyle.Render(placed)
}

// renderListContent renders the session list content
func (m Model) renderListContent(contentWidth int) string {
	var content strings.Builder

	// Header line: title + current time
	ts := titleStyle
	if !m.focused {
		ts = ts.Foreground(secondaryColor)
	}
	title := ts.Render("ccvalet")
	currentTime := time.Now().Format("15:04:05")
	timeDisplay := fmt.Sprintf("[ %s ]", currentTime)

	titleLen := lipgloss.Width(title)
	timeLen := len(timeDisplay)
	headerSpacing := contentWidth - titleLen - timeLen
	headerSpacing = max(headerSpacing, 2)

	content.WriteString(title)
	content.WriteString(strings.Repeat(" ", headerSpacing))
	content.WriteString(timeStyle.Render(timeDisplay))
	content.WriteString("\n")

	// STATS line
	statusSummary := buildStatusSummary(m.sessions)
	if statusSummary != "" {
		content.WriteString("STATS: ")
		content.WriteString(statusSummary)
		content.WriteString("\n")
	}

	// Separator
	content.WriteString(strings.Repeat("-", contentWidth))
	content.WriteString("\n")

	// Search bar
	if m.searching {
		matchCount := len(m.getDisplaySessions())
		content.WriteString("/")
		content.WriteString(m.searchInput.View())
		content.WriteString(helpStyle.Render(fmt.Sprintf(" (%d)", matchCount)))
		content.WriteString("\n")
	}

	// Error message
	if m.err != nil {
		content.WriteString(lipgloss.NewStyle().Foreground(errorColor).Render(fmt.Sprintf("Error: %v", m.err)))
		content.WriteString("\n\n")
	}

	// Sessions list
	displaySessions := m.getDisplaySessions()
	if len(displaySessions) == 0 {
		content.WriteString("\n")
		if m.searching {
			content.WriteString(helpStyle.Render("No matching sessions."))
		} else {
			content.WriteString(helpStyle.Render("No sessions. Press 'n' to create one."))
		}
		content.WriteString("\n")
	} else {
		pageSessions := m.getPageSessions()
		groups := groupSessionsByFleet(pageSessions)
		showHeaders := len(groups) >= 1

		// Build ID-to-cursor-index mapping for correct selection highlighting
		idToIdx := make(map[string]int, len(pageSessions))
		for i, sess := range pageSessions {
			idToIdx[sess.ID] = i
		}

		for _, group := range groups {
			if showHeaders {
				content.WriteString(renderFleetHeader(group.Name, contentWidth))
			}
			for _, sess := range group.Sessions {
				idx := idToIdx[sess.ID]
				content.WriteString(m.renderSession(sess, idx == m.cursor, contentWidth))
			}
		}
	}

	// Page info
	totalPages := m.getTotalPages()
	if totalPages > 1 {
		content.WriteString("\n")
		pageInfo := fmt.Sprintf("Page %d/%d", m.currentPage+1, totalPages)
		pageInfoStyled := helpStyle.Render(pageInfo)
		pageInfoLen := lipgloss.Width(pageInfoStyled)
		leftPad := (contentWidth - pageInfoLen) / 2
		if leftPad > 0 {
			content.WriteString(strings.Repeat(" ", leftPad))
		}
		content.WriteString(pageInfoStyled)
	}

	return content.String()
}

// renderHelpLine renders the help line at the bottom
func (m Model) renderHelpLine() string {
	if m.confirmKill {
		name := truncateString(m.killTargetName, 20)
		confirmMsg := fmt.Sprintf(" Kill '%s'? y:yes n:no", name)
		return lipgloss.NewStyle().Foreground(warningColor).Bold(true).Render(confirmMsg)
	}
	if m.searching {
		return helpStyle.Render(" Esc:clear  Enter:attach  ↑↓:navigate")
	}
	return helpStyle.Render(" ? help  / search")
}

// renderSession renders a single session in 1-line format with optional output preview
// Format: >name (branch)                    STATUS    Last Active
//
//	details...
func (m Model) renderSession(sess session.Info, selected bool, width int) string {
	var b strings.Builder

	statusIcon, statusLabel, statusStyle := getStatusDisplay(sess.Status)

	// Use LastActiveAt if available, otherwise CreatedAt
	var lastActiveTime time.Time
	if !sess.LastActiveAt.IsZero() {
		lastActiveTime = sess.LastActiveAt
	} else {
		lastActiveTime = sess.CreatedAt
	}
	timeStr := timeAgo(lastActiveTime)

	// --- Line 1: cursor + session name ---
	availableForName := width - 2 // cursor(2)
	name := truncateString(sess.Name, availableForName)

	if selected {
		b.WriteString(selectedItemStyle.Render(padLine("> "+name, width)))
	} else {
		b.WriteString("  ")
		b.WriteString(sessionNameStyle.Render(name))
	}
	b.WriteString("\n")

	// Build metadata: [host] workdir (branch)
	var metaParts []string
	if sess.HostID != "" && sess.HostID != "local" {
		metaParts = append(metaParts, "["+sess.HostID+"]")
	}
	// Use CurrentWorkDir if available, fall back to WorkDir
	displayDir := sess.CurrentWorkDir
	if displayDir == "" {
		displayDir = sess.WorkDir
	}
	if displayDir != "" {
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(displayDir, home) {
			displayDir = "~" + displayDir[len(home):]
		}
		metaParts = append(metaParts, displayDir)
	}
	if sess.CurrentBranch != "" {
		metaParts = append(metaParts, "("+sess.CurrentBranch+")")
	}

	statusStr := statusIcon + " " + statusLabel
	metaStr := strings.Join(metaParts, " ")
	indent := "  ├─ "
	indentWidth := 5

	// Truncate metadata if needed
	availableForMeta := width - indentWidth
	if availableForMeta > 0 && runewidth.StringWidth(metaStr) > availableForMeta {
		metaStr = truncateString(metaStr, availableForMeta)
	}

	// --- Line 2: status (icon + label) ---
	if selected {
		b.WriteString(selectedItemStyle.Render(padLine(indent+statusStr, width)))
	} else {
		b.WriteString(indent)
		b.WriteString(statusStyle.Render(statusStr))
	}
	b.WriteString("\n")

	// --- Line 3: metadata ([host] repo (branch)) ---
	if metaStr != "" {
		if selected {
			b.WriteString(selectedItemStyle.Render(padLine(indent+metaStr, width)))
		} else {
			b.WriteString(indent)
			b.WriteString(helpStyle.Render(metaStr))
		}
		b.WriteString("\n")
	}

	// --- Line 3: last user message ---
	if sess.LastUserMessage != "" {
		prefix := "  ├─ 👤 "
		pWidth := lipgloss.Width(prefix)
		msgWidth := width - pWidth
		msgWidth = max(msgWidth, 10)
		msgStr := truncateString(sess.LastUserMessage, msgWidth)

		if selected {
			b.WriteString(selectedItemStyle.Render(padLine(prefix+msgStr, width)))
		} else {
			b.WriteString("  ├─ " + helpStyle.Render("👤 "+msgStr))
		}
		b.WriteString("\n")
	}

	// --- Line 4: last assistant message ---
	if sess.LastAssistantMessage != "" {
		prefix := "  ├─ 🤖 "
		pWidth := lipgloss.Width(prefix)
		msgWidth := width - pWidth
		msgWidth = max(msgWidth, 10)
		msgStr := truncateStringFromEnd(sess.LastAssistantMessage, msgWidth)

		if selected {
			b.WriteString(selectedItemStyle.Render(padLine(prefix+msgStr, width)))
		} else {
			b.WriteString("  ├─ " + helpStyle.Render("🤖 "+msgStr))
		}
		b.WriteString("\n")
	}

	// --- Last line: time ---
	if selected {
		b.WriteString(selectedItemStyle.Render(padLine("  └─ "+timeStr, width)))
	} else {
		b.WriteString("  └─ " + timeStyle.Render(timeStr))
	}
	b.WriteString("\n")

	return b.String()
}

// padLine pads a string to the specified width with spaces.
func padLine(s string, width int) string {
	w := lipgloss.Width(s)
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// truncateString truncates a string to fit within maxWidth display width from the beginning
func truncateString(s string, maxWidth int) string {
	if runewidth.StringWidth(s) <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return truncateToWidth(s, maxWidth)
	}
	return truncateToWidth(s, maxWidth-3) + "..."
}

// truncateStringFromEnd truncates a string, keeping the last maxWidth display width
func truncateStringFromEnd(s string, maxWidth int) string {
	if runewidth.StringWidth(s) <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return truncateFromEndToWidth(s, maxWidth)
	}
	return "..." + truncateFromEndToWidth(s, maxWidth-3)
}

// truncateToWidth truncates a string from the beginning to fit within maxWidth
func truncateToWidth(s string, maxWidth int) string {
	var result []rune
	width := 0
	for _, r := range s {
		w := runewidth.RuneWidth(r)
		if width+w > maxWidth {
			break
		}
		result = append(result, r)
		width += w
	}
	return string(result)
}

// truncateFromEndToWidth truncates a string from the end, keeping the last maxWidth
func truncateFromEndToWidth(s string, maxWidth int) string {
	runes := []rune(s)
	width := 0
	startIdx := len(runes)
	for i := len(runes) - 1; i >= 0; i-- {
		w := runewidth.RuneWidth(runes[i])
		if width+w > maxWidth {
			break
		}
		startIdx = i
		width += w
	}
	return string(runes[startIdx:])
}

// timeAgo returns a human-readable relative time string
func timeAgo(t time.Time) string {
	now := time.Now()
	diff := now.Sub(t)

	switch {
	case diff < time.Minute:
		return "just now"
	case diff < time.Hour:
		mins := int(diff.Minutes())
		if mins == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", mins)
	case diff < 24*time.Hour:
		hours := int(diff.Hours())
		if hours == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", hours)
	default:
		days := int(diff.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

// countStatuses counts sessions by status category for summary
type statusCounts struct {
	thinking   int
	permission int
	running    int
	creating   int
	idle       int
	stopped    int
}

func countStatuses(sessions []session.Info) statusCounts {
	var counts statusCounts
	for _, s := range sessions {
		switch s.Status {
		case session.StatusThinking:
			counts.thinking++
		case session.StatusPermission:
			counts.permission++
		case session.StatusRunning:
			counts.running++
		case session.StatusCreating:
			counts.creating++
		case session.StatusIdle:
			counts.idle++
		case session.StatusStopped:
			counts.stopped++
		}
	}
	return counts
}

// buildStatusSummary builds the status summary string for header
func buildStatusSummary(sessions []session.Info) string {
	counts := countStatuses(sessions)

	var parts []string
	if counts.thinking > 0 {
		parts = append(parts, thinkingStyle.Render(fmt.Sprintf("*%d Thinking", counts.thinking)))
	}
	if counts.permission > 0 {
		parts = append(parts, permissionStyle.Render(fmt.Sprintf("?%d Permission", counts.permission)))
	}
	if counts.running > 0 {
		parts = append(parts, runningStyle.Render(fmt.Sprintf(">%d Running", counts.running)))
	}
	if counts.creating > 0 {
		parts = append(parts, creatingStyle.Render(fmt.Sprintf("+%d Creating", counts.creating)))
	}
	if counts.idle > 0 {
		parts = append(parts, idleStyle.Render(fmt.Sprintf("o%d Idle", counts.idle)))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// getStatusDisplay returns icon, label, and style for a given status
func getStatusDisplay(status session.Status) (icon, label string, style lipgloss.Style) {
	switch status {
	case session.StatusThinking:
		return "⚡", "THINKING", thinkingStyle
	case session.StatusPermission:
		return "?", "PERMISSION", permissionStyle
	case session.StatusRunning:
		return "▶", "RUNNING", runningStyle
	case session.StatusCreating:
		return "+", "CREATING", creatingStyle
	case session.StatusIdle:
		return "○", "IDLE", idleStyle
	case session.StatusStopped:
		return "■", "STOPPED", stoppedStyle
	default:
		return "?", "UNKNOWN", stoppedStyle
	}
}

// fleetGroup represents a group of sessions belonging to the same fleet.
type fleetGroup struct {
	Name     string
	Sessions []session.Info
}

// groupSessionsByFleet groups sessions by fleet name.
// Groups are sorted alphabetically, with session.DefaultFleet always last.
// Sessions within each group maintain their original order.
func groupSessionsByFleet(sessions []session.Info) []fleetGroup {
	// Collect sessions by fleet
	groupMap := make(map[string][]session.Info)
	var fleetNames []string
	seen := make(map[string]bool)

	for _, sess := range sessions {
		name := sess.Fleet
		if !seen[name] {
			seen[name] = true
			fleetNames = append(fleetNames, name)
		}
		groupMap[name] = append(groupMap[name], sess)
	}

	// Sort fleet names alphabetically, DefaultFleet always last
	sort.SliceStable(fleetNames, func(i, j int) bool {
		if fleetNames[i] == session.DefaultFleet {
			return false
		}
		if fleetNames[j] == session.DefaultFleet {
			return true
		}
		return fleetNames[i] < fleetNames[j]
	})

	groups := make([]fleetGroup, 0, len(fleetNames))
	for _, name := range fleetNames {
		groups = append(groups, fleetGroup{
			Name:     name,
			Sessions: groupMap[name],
		})
	}
	return groups
}

// renderFleetHeader renders a fleet group header line.
func renderFleetHeader(name string, width int) string {
	label := " " + name + " "
	labelWidth := lipgloss.Width(label)

	// ── fleet-name ──────
	leftDash := "── "
	rightDashCount := max(width-lipgloss.Width(leftDash)-labelWidth, 1)
	rightDash := strings.Repeat("─", rightDashCount)

	headerStyle := lipgloss.NewStyle().Foreground(secondaryColor)
	return headerStyle.Render(leftDash+label+rightDash) + "\n"
}

// wrapText wraps text to fit within the specified width
func wrapText(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}

	var lines []string
	// First split by existing newlines
	for rawLine := range strings.SplitSeq(text, "\n") {
		if runewidth.StringWidth(rawLine) <= width {
			lines = append(lines, rawLine)
			continue
		}
		// Wrap long lines
		runes := []rune(rawLine)
		current := 0
		for current < len(runes) {
			end := current
			lineWidth := 0
			for end < len(runes) && lineWidth < width {
				w := runewidth.RuneWidth(runes[end])
				if lineWidth+w > width {
					break
				}
				lineWidth += w
				end++
			}
			if end == current {
				end++ // Avoid infinite loop for very wide characters
			}
			lines = append(lines, string(runes[current:end]))
			current = end
		}
	}
	return lines
}
