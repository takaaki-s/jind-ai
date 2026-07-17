package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/takaaki-s/jind-ai/internal/action"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/paths"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/tmux"
)

// maxTUIWidth is the maximum width (columns) for the TUI pane.
// When the terminal is maximized, the TUI pane is resized to this width
// so the display pane gets the extra space.
const maxTUIWidth = 50
const minTUIWidth = 30

// KeyMap defines key bindings
type KeyMap struct {
	Up       key.Binding
	Down     key.Binding
	Enter    key.Binding
	New      key.Binding
	Kill     key.Binding
	Delete   key.Binding
	Refresh  key.Binding
	Quit     key.Binding
	Help     key.Binding
	PrevPage key.Binding // Scroll one screen up (viewport)
	NextPage key.Binding // Scroll one screen down (viewport)
	Home     key.Binding // Jump to first session
	End      key.Binding // Jump to last session
	Vscode   key.Binding

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
			key.WithKeys("pgup", "left", "h", "ctrl+b"),
			key.WithHelp("←/h/PgUp", "scroll up"),
		),
		NextPage: key.NewBinding(
			key.WithKeys("pgdown", "right", "l", "ctrl+f"),
			key.WithHelp("→/l/PgDn", "scroll down"),
		),
		Home: key.NewBinding(
			key.WithKeys("home", "g"),
			key.WithHelp("g/Home", "first session"),
		),
		End: key.NewBinding(
			key.WithKeys("end", "G"),
			key.WithHelp("G/End", "last session"),
		),
		Vscode: key.NewBinding(
			key.WithKeys(cfg.Vscode...),
			key.WithHelp(strings.Join(cfg.Vscode, "/"), "open vscode"),
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
	warning  string // Non-fatal notice from last create (e.g. hook not allowlisted)
	keys     KeyMap // Keybinding settings

	// Config manager (used for remote session attach)
	configMgr *config.Manager

	// Viewport scrolling. Line offset of the topmost visible row in the
	// scrollable card area (0 = top). Adjusted by cursor movement and by
	// PageUp/PageDown/Home/End; clamped whenever the underlying content
	// changes (session list, window resize).
	scrollOffset int

	// Delete confirmation
	confirmDelete          bool   // Whether delete confirmation is active
	deleteTargetID         string // Session ID to delete
	deleteTargetDesc       string // Session description to delete (for display)
	deleteTargetIsWorktree bool   // Whether the session is in a git worktree
	confirmWorktreeForce   bool   // Whether force-delete worktree confirmation is active

	// Async delete tracking
	deletingIDs map[string]bool // Session IDs currently being deleted

	// Kill confirmation
	confirmKill    bool   // Whether kill confirmation is active
	killTargetID   string // Session ID to kill
	killTargetDesc string // Session description to kill (for display)

	// Focus tracking (for visual focus indicator)
	focused bool // true when TUI pane has focus (changes border/title color)

	// tmux integration
	tmuxClient         *tmux.Client // outer tmux client (-L jin-mgr, nil in legacy mode)
	innerTmuxClient    *tmux.Client // inner tmux client (-L jin, for switch-client)
	tuiPaneID          string       // TUI pane unique ID (e.g. "%42") in outer tmux
	displayPaneID      string       // Right pane unique ID (for session display) in outer tmux
	currentSessionID   string       // Session ID currently displayed in right pane
	displayLocalAttach bool         // true when display pane is running tmux attach to inner tmux

	// Focus after create
	focusSessionID string // Session ID to focus after creation

	// Reswitch after delete/kill
	needsReswitch bool // Reconnect to session at cursor after delete/kill

	// Align cursor to the restored currentSessionID on the first sessionsMsg
	// after TUI restart. Cleared after the first attempt so subsequent user
	// cursor movements are preserved across polls.
	pendingCursorRestore bool

	// Processing indicator
	processingMsg    string // Processing message (overlay displayed when non-empty)
	waitingForResize bool   // Waiting for WindowSizeMsg (resize completion after ZoomPane)

	// Last Description pushed to the display pane's `@session_name` tmux
	// variable. Used to detect Layer C description upgrades between polls so
	// the tmux status bar template picks them up without a manual switch.
	lastDisplayedDesc string
}

// NewModel creates a new TUI model
func NewModel(client *daemon.Client) Model {
	// Initialize config manager
	configMgr, _ := config.NewManager(paths.Config())

	// Initialize keybindings
	var keybindings config.KeybindingsConfig
	if configMgr != nil {
		keybindings = configMgr.GetKeybindings()
	} else {
		keybindings = config.DefaultKeybindings()
	}
	keys := NewKeyMap(keybindings)

	return Model{
		client:      client,
		keys:        keys,
		focused:     true,
		configMgr:   configMgr,
		deletingIDs: make(map[string]bool),
	}
}

// NewModelWithTmux creates a new TUI model with tmux integration.
// The outer tmux (-L jin-mgr) has a fixed 2-pane layout:
// left pane (TUI) + right pane (session display via RespawnPane).
func NewModelWithTmux(client *daemon.Client, tc, innerTC *tmux.Client, tuiPaneID, displayPaneID string) Model {
	m := NewModel(client)
	m.tmuxClient = tc
	m.innerTmuxClient = innerTC
	m.tuiPaneID = tuiPaneID
	m.displayPaneID = displayPaneID
	// Restore which session was displayed (for reattach)
	m.currentSessionID = tc.GetEnvironment(tmux.SessionName, "JIN_CURRENT_SESSION")
	// Point the cursor at that restored session on the first sessionsMsg so
	// relaunching the TUI keeps the left-list selection aligned with the
	// right pane the user was looking at.
	m.pendingCursorRestore = m.currentSessionID != ""
	// Reset JIN_CURSOR_SESSION at startup — sessions have not been fetched
	// yet, so publish an empty value so a stale env from a prior TUI run does
	// not confuse a popup that opens before the first sessionsMsg arrives.
	m.writeCursorEnv()
	return m
}

// contentAreaLines returns the number of lines available for the scrollable
// card area — the pane height minus the fixed header rows (STATS + blank,
// plus error / warning when active).
func (m *Model) contentAreaLines() int {
	// Pane holds (m.height - 1) rows (the extra row is the outer help line).
	avail := m.height - 1
	// STATS row (always shown unless there are literally no sessions).
	if len(m.sessions) > 0 {
		avail--
	}
	// Blank separator row between header and cards.
	avail--
	if m.err != nil {
		avail -= 2 // "Error: ..." + blank
	}
	if m.warning != "" {
		avail -= 2 // "⚠ ..." + blank
	}
	return max(avail, 3)
}

// pageScrollLines returns how many lines PageUp / PageDown scrolls the
// viewport — one visible page worth of cards, minus one line of overlap so
// the user keeps a reference row across the jump.
func (m *Model) pageScrollLines() int {
	return max(m.contentAreaLines()-1, 1)
}

// cardHeight returns the number of terminal rows a single session card
// occupies in the current renderSession layout, including the trailing
// blank-line spacer. Kept in sync with renderSession by construction — if
// the layout changes there, update the counts here in the same commit.
func (m *Model) cardHeight(sess session.Info) int {
	if m.deletingIDs[sess.ID] {
		// Deleting: name + "⟳ deleting..." + trailing blank
		return 3
	}
	// Base: name + status/meta + trailing blank
	h := 3
	if sess.LastUserMessage != "" {
		h++
	}
	if sess.LastAssistantMessage != "" {
		h++
	}
	return h
}

// sessionCardTop returns the line offset (within the scrollable card area,
// 0 = first row of the first fleet header or card) where the session at
// display-index `idx` starts, and its height. Returns (-1, 0) if idx is
// out of range.
func (m *Model) sessionCardTop(idx int) (top, height int) {
	sessions := m.getDisplaySessions()
	if idx < 0 || idx >= len(sessions) {
		return -1, 0
	}
	targetID := sessions[idx].ID
	groups := groupSessionsByFleet(sessions)
	showHeaders := len(groups) >= 1
	line := 0
	for _, g := range groups {
		if showHeaders {
			line++ // fleet header row
		}
		for _, sess := range g.Sessions {
			h := m.cardHeight(sess)
			if sess.ID == targetID {
				return line, h
			}
			line += h
		}
	}
	return -1, 0
}

// totalCardLines returns the total number of lines the scrollable card
// area currently spans (all cards + fleet headers), used to clamp
// scrollOffset so we cannot scroll past the last card.
func (m *Model) totalCardLines() int {
	sessions := m.getDisplaySessions()
	groups := groupSessionsByFleet(sessions)
	showHeaders := len(groups) >= 1
	total := 0
	for _, g := range groups {
		if showHeaders {
			total++
		}
		for _, sess := range g.Sessions {
			total += m.cardHeight(sess)
		}
	}
	return total
}

// adjustScrollForCursor moves scrollOffset so the current cursor's card is
// fully visible in the content area. Called after any cursor movement.
func (m *Model) adjustScrollForCursor() {
	top, height := m.sessionCardTop(m.cursor)
	if top < 0 {
		m.scrollOffset = 0
		return
	}
	avail := m.contentAreaLines()
	bottom := top + height
	if top < m.scrollOffset {
		m.scrollOffset = top
	} else if bottom > m.scrollOffset+avail {
		m.scrollOffset = bottom - avail
	}
	m.clampScroll()
}

// clampScroll bounds scrollOffset into [0, max(0, totalCardLines-avail)].
// Call after any change that shrinks or grows the content (session list
// change, filter toggle, window resize).
func (m *Model) clampScroll() {
	max := m.totalCardLines() - m.contentAreaLines()
	if max < 0 {
		max = 0
	}
	if m.scrollOffset > max {
		m.scrollOffset = max
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
}

// getDisplaySessions returns the sessions to display.
func (m *Model) getDisplaySessions() []session.Info {
	return m.sessions
}

// Messages
type sessionsMsg []session.Info
type errMsg error

// envTickMsg fires on envTickInterval to poll tmux env pushed by popup children.
type envTickMsg time.Time

// sessionTickMsg fires on sessionTickInterval to refetch the session list.
type sessionTickMsg time.Time

type deleteErrMsg struct {
	sessionID string
	err       error
}
type worktreeDirtyMsg struct {
	sessionID string
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

const (
	// envTickInterval controls how often the TUI polls tmux env vars pushed
	// by popup children (JIN_CREATED_SESSION / JIN_CREATED_WARNING /
	// JIN_NOTIFY_SESSION / JIN_ACTION_ID). Kept short so popup selections
	// reflect in the parent TUI without user-visible lag.
	envTickInterval = 250 * time.Millisecond

	// sessionTickInterval controls how often the TUI refetches the session
	// list from the daemon. Longer than envTickInterval because refetches
	// touch the daemon socket and re-render the full list.
	sessionTickInterval = 2 * time.Second
)

func envTickCmd() tea.Cmd {
	return tea.Tick(envTickInterval, func(t time.Time) tea.Msg {
		return envTickMsg(t)
	})
}

func sessionTickCmd() tea.Cmd {
	return tea.Tick(sessionTickInterval, func(t time.Time) tea.Msg {
		return sessionTickMsg(t)
	})
}

// resizeSettledMsg is sent after a delay to allow WindowSizeMsg to arrive
// after tmux pane operations (ZoomPane).
type resizeSettledMsg struct{}

// resolveFocusSession completes a pending focus switch. Returns true if
// nothing was pending or the target was found and switched (clearing
// focusSessionID + refreshing JIN_CURSOR_SESSION). Returns false with
// focusSessionID retained if the target is not yet in m.sessions; callers
// decide whether to keep it armed for retry (envTick fast path) or clear
// and give up (sessionsMsg slow path, already ran against a fresh List).
func (m *Model) resolveFocusSession() bool {
	if m.focusSessionID == "" {
		return true
	}
	displaySessions := m.getDisplaySessions()
	i := slices.IndexFunc(displaySessions, func(s session.Info) bool {
		return s.ID == m.focusSessionID
	})
	if i < 0 {
		return false
	}
	m.cursor = i
	m.adjustScrollForCursor()
	m.currentSessionID = "" // Force reset so switchToSession runs even when the cursor was already on this session.
	m.switchToSession(displaySessions[i].ID)
	m.focusSessionID = ""
	m.writeCursorEnv()
	return true
}

// switchToSession displays the given session in the right pane via RespawnPane.
// For local sessions, attaches to the inner tmux session (-L jin).
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
	isLocalAlive := isSessionAlive(sess.Status) && sess.TmuxWindowName != ""

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
				sess.Description, sess.Status, sess.ErrorMessage,
			)
		} else {
			placeholderCmd = fmt.Sprintf(
				"printf '\\n  Session: %s\\n  Status:  %s\\n\\n  Press Enter to restart\\n'; tail -f /dev/null",
				sess.Description, sess.Status,
			)
		}
		_ = m.tmuxClient.RespawnPane(m.displayPaneID, placeholderCmd)
		m.currentSessionID = sessionID
		_ = m.tmuxClient.SetEnvironment(tmux.SessionName, "JIN_CURRENT_SESSION", sessionID)
		m.pushDisplayedDescription(sess.Description)
		return
	}

	// Running sessions require TmuxWindowName for inner tmux attach
	if sess.TmuxWindowName == "" {
		return
	}

	// Local alive session: prefer switch-client over respawn-pane to avoid "pane is dead"
	if m.displayLocalAttach && m.innerTmuxClient != nil {
		paneTTY, err := m.tmuxClient.GetPaneTTY(m.displayPaneID)
		if err == nil && paneTTY != "" {
			if m.innerTmuxClient.SwitchClient(paneTTY, sess.TmuxWindowName) == nil {
				m.currentSessionID = sessionID
				_ = m.tmuxClient.SetEnvironment(tmux.SessionName, "JIN_CURRENT_SESSION", sessionID)
				m.pushDisplayedDescription(sess.Description)
				return
			}
		}
		// switch-client failed — fall through to respawn
	}

	// Local: respawn right pane with inner tmux attach.
	// Unset $TMUX so tmux does not refuse with "sessions should be nested with care":
	// the display pane runs inside the outer tmux (jin-mgr), so $TMUX points to
	// the outer session. Without env -u TMUX, attaching to the inner tmux (jin)
	// on the same host is rejected as nesting. This mirrors the env -u TMUX pattern
	// used in session/manager.go when launching CC processes.
	attachCmd := fmt.Sprintf("env -u TMUX tmux -L %s attach -t %s", tmux.SocketName, sess.TmuxWindowName)
	_ = m.tmuxClient.RespawnPane(m.displayPaneID, attachCmd)
	m.displayLocalAttach = true

	m.currentSessionID = sessionID
	_ = m.tmuxClient.SetEnvironment(tmux.SessionName, "JIN_CURRENT_SESSION", sessionID)
	m.pushDisplayedDescription(sess.Description)
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

// pushDisplayedDescription sets the display pane's `@session_name` tmux
// variable and records the value locally so refreshDisplayedDescription can
// detect drift without re-issuing set-option every poll.
func (m *Model) pushDisplayedDescription(desc string) {
	if m.tmuxClient == nil || m.displayPaneID == "" {
		return
	}
	_ = m.tmuxClient.SetPaneOption(m.displayPaneID, "@session_name", desc)
	m.lastDisplayedDesc = desc
}

// refreshDisplayedDescription re-pushes `@session_name` when the currently
// displayed session's Description has changed since the last poll (e.g.
// Layer C promoted the baseline to a transcript-derived label). Cheap: it
// walks m.sessions once and calls set-option at most once per drift.
func (m *Model) refreshDisplayedDescription() {
	if m.tmuxClient == nil || m.displayPaneID == "" || m.currentSessionID == "" {
		return
	}
	// Skip synthetic placeholders (e.g. "_empty" from respawnPlaceholder).
	if strings.HasPrefix(m.currentSessionID, "_") {
		return
	}
	for i := range m.sessions {
		if m.sessions[i].ID != m.currentSessionID {
			continue
		}
		desc := m.sessions[i].Description
		if desc == m.lastDisplayedDesc {
			return
		}
		m.pushDisplayedDescription(desc)
		return
	}
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

// openVSCode opens VS Code for the given session's working directory.
func (m *Model) openVSCode(sess *session.Info) {
	workDir := sess.CurrentWorkDir
	if workDir == "" {
		workDir = sess.WorkDir
	}
	if workDir == "" {
		return
	}
	_ = exec.Command("code", workDir).Start()
}

// handleSelectSession switches the right pane to display the currently selected session.
func (m Model) handleSelectSession() (tea.Model, tea.Cmd) {
	pageSessions := m.getDisplaySessions()
	if len(pageSessions) == 0 || m.cursor >= len(pageSessions) {
		return m, nil
	}
	sess := pageSessions[m.cursor]

	if sess.Status == session.StatusCreating {
		m.err = fmt.Errorf("cannot select creating session")
		return m, nil
	}
	if m.deletingIDs[sess.ID] {
		return m, nil
	}

	if m.tmuxClient != nil {
		needsStart := sess.Status == session.StatusStopped
		if needsStart {
			if err := m.client.Start(sess.ID); err != nil {
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

// currentCursorSessionID returns the session ID under the cursor, or "" when
// the list is empty, the cursor is out of range, or the target is in a
// deleting state. Kept in sync with the palette / plugin dispatch so callers
// never target a session that is transitioning away.
func (m Model) currentCursorSessionID() string {
	ps := m.getDisplaySessions()
	if len(ps) == 0 || m.cursor < 0 || m.cursor >= len(ps) {
		return ""
	}
	if m.deletingIDs[ps[m.cursor].ID] {
		return ""
	}
	return ps[m.cursor].ID
}

// writeCursorEnv publishes the current cursor session ID to the outer tmux
// server env (JIN_CURSOR_SESSION). The action-popup reads this to decorate
// NeedsSession labels with the target's description. No-op without an outer
// tmux client (legacy mode / tests).
func (m Model) writeCursorEnv() {
	if m.tmuxClient == nil {
		return
	}
	_ = m.tmuxClient.SetEnvironment(tmux.SessionName, "JIN_CURSOR_SESSION", m.currentCursorSessionID())
}

// openPopup runs one of the hidden `jin <name>-popup` UIs inside a tmux
// popup, sized via configMgr.GetPopupSize(name). No-op when tmuxClient or
// configMgr is unwired (tests, legacy mode); popup errors are swallowed
// since there is no useful recovery from a failed popup spawn mid-Bubble
// Tea update loop.
func (m Model) openPopup(name, title string) {
	if m.tmuxClient == nil || m.configMgr == nil {
		return
	}
	_ = m.tmuxClient.DisplayPopup(m.popupDisplayOptions(name, title))
}

// popupDisplayOptions resolves the tmux display-popup arguments for a
// canonical popup name. Split out from openPopup so the size/subcmd
// resolution is unit-testable without a live tmux client. Both the size
// and the subcommand are looked up from config's popup catalog, so config
// keys and cobra subcommand names cannot silently drift.
func (m Model) popupDisplayOptions(name, title string) tmux.DisplayPopupOptions {
	width, height := m.configMgr.GetPopupSize(name)
	selfBin, _ := os.Executable()
	return tmux.DisplayPopupOptions{
		Width:  width,
		Height: height,
		Cmd:    fmt.Sprintf("'%s' %s", selfBin, config.PopupSubcmd(name)),
		Title:  title,
	}
}

// handleNew opens the session-creation popup in outer tmux. Matches the
// former inline keys.New case verbatim.
func (m Model) handleNew() (tea.Model, tea.Cmd) {
	m.openPopup(config.PopupCreate, " New Session ")
	return m, nil
}

// handleKill enters kill-confirmation mode for the session under the cursor.
// No-op when the list is empty or the target is already being deleted.
func (m Model) handleKill() (tea.Model, tea.Cmd) {
	pageSessions := m.getDisplaySessions()
	if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
		sess := pageSessions[m.cursor]
		if m.deletingIDs[sess.ID] {
			return m, nil
		}
		m.confirmKill = true
		m.killTargetID = sess.ID
		m.killTargetDesc = sess.Description
		return m, nil
	}
	return m, nil
}

// handleDelete enters delete-confirmation mode for the session under the
// cursor. Worktree sessions get a follow-up sub-confirmation later in the
// delete flow.
func (m Model) handleDelete() (tea.Model, tea.Cmd) {
	pageSessions := m.getDisplaySessions()
	if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
		sess := pageSessions[m.cursor]
		if m.deletingIDs[sess.ID] {
			return m, nil
		}
		m.confirmDelete = true
		m.deleteTargetID = sess.ID
		m.deleteTargetDesc = sess.Description
		m.deleteTargetIsWorktree = sess.IsWorktree
		return m, nil
	}
	return m, nil
}

// handleRefresh triggers a session-list refetch.
func (m Model) handleRefresh() (tea.Model, tea.Cmd) {
	return m, m.fetchSessions
}

// handleVscode launches VS Code for the cursor session's working directory.
func (m Model) handleVscode() (tea.Model, tea.Cmd) {
	pageSessions := m.getDisplaySessions()
	if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
		sess := pageSessions[m.cursor]
		if m.deletingIDs[sess.ID] {
			return m, nil
		}
		go m.openVSCode(&sess)
	}
	return m, nil
}

// handleSessionFilter opens the session filter popup — the same popup
// bound at the outer-tmux root key table via keybindings.search. Wired
// here so the action palette can launch it without depending on the
// tmux root binding being set (or on the user's default key).
func (m Model) handleSessionFilter() (tea.Model, tea.Cmd) {
	m.openPopup(config.PopupSessionFilter, " Session Filter ")
	return m, nil
}

// handleHelp opens the shortcut help popup.
func (m Model) handleHelp() (tea.Model, tea.Cmd) {
	m.openPopup(config.PopupHelp, " Shortcuts ")
	return m, nil
}

// handleTogglePane zooms/unzooms the display pane (sidebar toggle). Mirrors
// the outer-tmux root binding so palette invocation matches the direct key.
func (m Model) handleTogglePane() (tea.Model, tea.Cmd) {
	if m.tmuxClient == nil || m.displayPaneID == "" {
		return m, nil
	}
	_ = m.tmuxClient.ZoomPane(m.displayPaneID)
	return m, nil
}

// dispatchAction routes an action ID (from the action palette or any other
// caller) to the same helper the direct-key path uses. Unknown IDs are
// silently ignored so a stale env value cannot wedge the TUI.
func (m Model) dispatchAction(id string) (tea.Model, tea.Cmd) {
	switch id {
	case action.IDNew:
		return m.handleNew()
	case action.IDKill:
		return m.handleKill()
	case action.IDDelete:
		return m.handleDelete()
	case action.IDRefresh:
		return m.handleRefresh()
	case action.IDVscode:
		return m.handleVscode()
	case action.IDHelp:
		return m.handleHelp()
	case action.IDTogglePane:
		return m.handleTogglePane()
	case action.IDSessionFilter:
		return m.handleSessionFilter()
	}
	// Plugin palette IDs are three-segment ("plugin:<name>:<action>");
	// anything else — a core ID that missed the switch above, or a stale
	// two-segment ID left in the tmux env by an older binary — falls through
	// to the no-op return.
	if name, _, ok := action.ParsePluginActionID(id); ok {
		// TODO(plugin-multi-action): forward the parsed action ID once
		// daemon.PluginRunRequest grows an Action field (dispatcher phase);
		// until then the daemon always runs the plugin's default action.
		return m.handlePluginRun(name)
	}
	return m, nil
}

// handlePluginRun issues a plugin-run request to the daemon for the given
// plugin name, targeting the current cursor session (empty ID => global
// action). Failures surface on m.err.
func (m Model) handlePluginRun(name string) (tea.Model, tea.Cmd) {
	if m.client == nil {
		return m, nil
	}
	req := daemon.PluginRunRequest{
		Plugin:           name,
		SessionID:        m.currentCursorSessionID(),
		Depth:            0,
		CallerTmuxSocket: tmux.SocketPathFromEnv(os.Getenv("TMUX")),
		CallerTmuxPane:   m.tuiPaneID,
	}
	if err := m.client.PluginRun(req); err != nil {
		m.err = fmt.Errorf("plugin %s: %w", name, err)
	}
	return m, nil
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.fetchSessions,
		envTickCmd(),
		sessionTickCmd(),
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
		// Content area height depends on m.height, so re-clamp scroll and
		// re-follow the cursor.
		m.adjustScrollForCursor()
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
		// Dismiss any transient warning on the first key press.
		m.warning = ""

		// Handle delete confirmation mode
		if m.confirmDelete {
			// Sub-confirmation: force delete dirty worktree
			if m.confirmWorktreeForce {
				switch msg.String() {
				case "y", "Y":
					deleteID := m.deleteTargetID
					m.deletingIDs[deleteID] = true
					m.resetDeleteState()
					m.skipDeletingSessions(1)
					client := m.client
					return m, func() tea.Msg {
						if err := client.Delete(deleteID, true, true); err != nil {
							return deleteErrMsg{sessionID: deleteID, err: fmt.Errorf("delete failed: %w", err)}
						}
						sessions, err := client.List()
						if err != nil {
							return errMsg(err)
						}
						return sessionsMsg(sessions)
					}
				case "n", "N", "esc":
					// Fall back: delete session only
					deleteID := m.deleteTargetID
					m.deletingIDs[deleteID] = true
					m.resetDeleteState()
					m.skipDeletingSessions(1)
					client := m.client
					return m, func() tea.Msg {
						if err := client.Delete(deleteID, false, false); err != nil {
							return deleteErrMsg{sessionID: deleteID, err: fmt.Errorf("delete failed: %w", err)}
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
				deleteID := m.deleteTargetID
				m.deletingIDs[deleteID] = true
				m.resetDeleteState()
				m.skipDeletingSessions(1)
				client := m.client
				return m, func() tea.Msg {
					if err := client.Delete(deleteID, false, false); err != nil {
						return deleteErrMsg{sessionID: deleteID, err: fmt.Errorf("delete failed: %w", err)}
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
				deleteID := m.deleteTargetID
				deleteName := m.deleteTargetDesc
				m.deletingIDs[deleteID] = true
				m.resetDeleteState()
				m.skipDeletingSessions(1)
				client := m.client
				return m, func() tea.Msg {
					err := client.Delete(deleteID, true, false)
					if err != nil {
						if errors.Is(err, session.ErrWorktreeDirty) {
							return worktreeDirtyMsg{sessionID: deleteID, name: deleteName}
						}
						if errors.Is(err, session.ErrNotWorktree) {
							return deleteErrMsg{
								sessionID: deleteID,
								err:       fmt.Errorf("worktree not found for session %q (already removed, or session is not in a worktree)", deleteName),
							}
						}
						return deleteErrMsg{sessionID: deleteID, err: fmt.Errorf("delete failed: %w", err)}
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
				m.killTargetID = ""
				m.killTargetDesc = ""

				client := m.client

				return m, func() tea.Msg {
					if err := client.Kill(killID); err != nil {
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
				m.killTargetDesc = ""
				return m, nil
			}
			return m, nil
		}

		// Left pane key handling
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit

		case key.Matches(msg, m.keys.Up):
			if m.cursor > 0 {
				m.cursor--
				m.skipDeletingSessions(-1)
			}
			m.adjustScrollForCursor()
			m.writeCursorEnv()
			return m, nil

		case key.Matches(msg, m.keys.Down):
			pageSessions := m.getDisplaySessions()
			if m.cursor < len(pageSessions)-1 {
				m.cursor++
				m.skipDeletingSessions(1)
			}
			m.adjustScrollForCursor()
			m.writeCursorEnv()
			return m, nil

		case key.Matches(msg, m.keys.Enter):
			return m.handleSelectSession()

		case key.Matches(msg, m.keys.New):
			return m.handleNew()

		case key.Matches(msg, m.keys.Kill):
			return m.handleKill()

		case key.Matches(msg, m.keys.Delete):
			return m.handleDelete()

		case key.Matches(msg, m.keys.Refresh):
			return m.handleRefresh()

		case key.Matches(msg, m.keys.Help):
			return m.handleHelp()

		case key.Matches(msg, m.keys.PrevPage):
			m.scrollOffset -= m.pageScrollLines()
			m.clampScroll()
			m.writeCursorEnv()
			return m, nil

		case key.Matches(msg, m.keys.NextPage):
			m.scrollOffset += m.pageScrollLines()
			m.clampScroll()
			m.writeCursorEnv()
			return m, nil

		case key.Matches(msg, m.keys.Home):
			m.cursor = 0
			m.skipDeletingSessions(1)
			m.adjustScrollForCursor()
			m.writeCursorEnv()
			return m, nil

		case key.Matches(msg, m.keys.End):
			pageSessions := m.getDisplaySessions()
			if len(pageSessions) > 0 {
				m.cursor = len(pageSessions) - 1
				m.skipDeletingSessions(-1)
			}
			m.adjustScrollForCursor()
			m.writeCursorEnv()
			return m, nil

		case key.Matches(msg, m.keys.Vscode):
			return m.handleVscode()
		}

	case sessionsMsg:
		m.sessions = msg
		m.err = nil

		// Check if any deleting sessions have been removed (deletion completed)
		deleteCompleted := false
		if len(m.deletingIDs) > 0 {
			sessionIDs := make(map[string]bool, len(m.sessions))
			for _, s := range m.sessions {
				sessionIDs[s.ID] = true
			}
			for id := range m.deletingIDs {
				if !sessionIDs[id] {
					delete(m.deletingIDs, id)
					deleteCompleted = true
				}
			}
		}

		// Align cursor to the session restored from JIN_CURRENT_SESSION so a
		// relaunched TUI selects whatever the right pane is showing. Runs once
		// per TUI startup (armed in NewModelWithTmux only when currentSessionID
		// is non-empty); if the target no longer exists between runs, IndexFunc
		// returns -1 and the cursor keeps its default.
		if m.pendingCursorRestore {
			m.pendingCursorRestore = false
			if i := slices.IndexFunc(m.getDisplaySessions(), func(s session.Info) bool {
				return s.ID == m.currentSessionID
			}); i >= 0 {
				m.cursor = i
			}
		}

		// Focus on newly created session + switch right pane. Slow path:
		// even after a fresh List we may still miss (target killed between
		// popup selection and this frame); in that case clear the pending
		// target so subsequent ticks don't spin on a ghost ID.
		if m.focusSessionID != "" {
			if !m.resolveFocusSession() {
				m.focusSessionID = ""
				m.writeCursorEnv()
			}
			return m, nil
		}
		displaySessions := m.getDisplaySessions()
		if m.cursor >= len(displaySessions) && m.cursor > 0 {
			m.cursor = len(displaySessions) - 1
		}
		// Session list changed: clamp scroll so we cannot land past the last
		// card, and ensure the cursor's card stays in view.
		m.adjustScrollForCursor()
		// Reconnect to session at cursor after delete/kill
		if m.needsReswitch || deleteCompleted {
			m.needsReswitch = false
			m.currentSessionID = "" // Force reset
			if m.displayLocalAttach {
				m.detachInnerClient()
				m.displayLocalAttach = false
			}
			pageSessions := m.getDisplaySessions()
			if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
				sess := pageSessions[m.cursor]
				if !m.deletingIDs[sess.ID] {
					m.switchToSession(sess.ID)
				}
			} else {
				m.respawnPlaceholder()
			}
			m.processingMsg = ""
			m.writeCursorEnv()
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
			m.writeCursorEnv()
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
				pageSessions := m.getDisplaySessions()
				if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
					m.switchToSession(pageSessions[m.cursor].ID)
				} else {
					m.respawnPlaceholder()
				}
			}
		}
		// Auto-display first session on initial load
		if m.currentSessionID == "" {
			pageSessions := m.getDisplaySessions()
			if len(pageSessions) > 0 && m.cursor < len(pageSessions) {
				m.switchToSession(pageSessions[m.cursor].ID)
			}
		}
		// Refresh @session_name for the currently displayed session so the
		// tmux status bar picks up Layer C description upgrades without a
		// manual switch. Idempotent: only pushes when the Description changed
		// since the last poll.
		m.refreshDisplayedDescription()
		m.processingMsg = ""
		// Keep the outer-tmux env in sync so a popup opened right after the
		// list refresh sees the current cursor target.
		m.writeCursorEnv()
		return m, nil

	case resizeSettledMsg:
		// Fallback: WindowSizeMsg did not arrive (no pane size change)
		if m.waitingForResize {
			m.waitingForResize = false
			m.processingMsg = ""
			return m, tea.ClearScreen
		}
		return m, nil

	case worktreeDirtyMsg:
		delete(m.deletingIDs, msg.sessionID)
		m.processingMsg = ""
		m.confirmDelete = true
		m.confirmWorktreeForce = true
		m.deleteTargetID = msg.sessionID
		m.deleteTargetDesc = msg.name
		m.deleteTargetIsWorktree = true

	case deleteErrMsg:
		delete(m.deletingIDs, msg.sessionID)
		m.err = msg.err

	case errMsg:
		m.processingMsg = ""
		m.err = msg

	case envTickMsg:
		if m.tmuxClient != nil {
			env := m.tmuxClient.ListEnvironment(tmux.SessionName)
			// consume reads a JIN_* key and, if set, unsets it on tmux so
			// the same value isn't picked up again on the next tick.
			consume := func(key string) string {
				v := env[key]
				if v != "" {
					_ = m.tmuxClient.UnsetEnvironment(tmux.SessionName, key)
				}
				return v
			}

			// Any popup that wants the parent TUI to focus a session pushes
			// the ID here. JIN_CREATED_SESSION (create popup), JIN_NOTIFY_SESSION
			// (external notifier plugin via jin session focus), JIN_FOCUS_SESSION
			// (session-filter-popup) all share the same downstream (switchToSession)
			// via focusSessionID.
			for _, k := range []string{"JIN_CREATED_SESSION", "JIN_NOTIFY_SESSION", "JIN_FOCUS_SESSION"} {
				if id := consume(k); id != "" {
					m.focusSessionID = id
				}
			}
			// Fast path: resolve now, or kick a fetch so the sessionsMsg slow
			// path resolves on the next round-trip instead of after the next
			// sessionTick (~2s). JIN_CREATED_WARNING / JIN_ACTION_ID stay in
			// tmux env and surface on the next envTick.
			if !m.resolveFocusSession() {
				return m, tea.Batch(envTickCmd(), m.fetchSessions)
			}
			// Non-fatal warning from the create popup (e.g. hook not
			// allowlisted). Read alongside JIN_CREATED_SESSION so it
			// surfaces on the same tick.
			if w := consume("JIN_CREATED_WARNING"); w != "" {
				m.warning = w
			}
			// Poll for an action ID pushed by the action-popup, then route
			// through dispatchAction so palette and direct-key paths share
			// the same helpers. If the helper returns a Cmd (e.g. Refresh),
			// merge it into the tick's Batch so tea sees a single frame.
			if id := consume("JIN_ACTION_ID"); id != "" {
				next, cmd := m.dispatchAction(id)
				if nm, ok := next.(Model); ok {
					m = nm
				}
				if cmd != nil {
					return m, tea.Batch(envTickCmd(), cmd)
				}
			}
		}
		return m, envTickCmd()

	case sessionTickMsg:
		return m, tea.Batch(m.fetchSessions, sessionTickCmd())
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

	paneWidth := m.width
	paneWidth = max(paneWidth, 20)
	paneHeight := m.height - 1
	paneHeight = max(paneHeight, 5)
	// Content sits inside 1-column horizontal padding on each side.
	contentWidth := paneWidth - 2
	contentWidth = max(contentWidth, 16)
	paneStyle := createPaneStyle(paneWidth, paneHeight, m.focused)
	pane := paneStyle.Render(m.renderListContent(contentWidth))
	helpLine := m.renderHelpLine()
	return pane + "\n" + helpLine
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
	m.deleteTargetDesc = ""
	m.deleteTargetIsWorktree = false
}

// skipDeletingSessions adjusts cursor to skip over sessions being deleted.
// dir: -1 for up, +1 for down.
func (m *Model) skipDeletingSessions(dir int) {
	if len(m.deletingIDs) == 0 {
		return
	}
	pageSessions := m.getDisplaySessions()
	for m.cursor >= 0 && m.cursor < len(pageSessions) && m.deletingIDs[pageSessions[m.cursor].ID] {
		m.cursor += dir
	}
	// Clamp
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(pageSessions) {
		m.cursor = len(pageSessions) - 1
	}
	// Fallback: if still on a deleting session, scan the opposite direction
	if m.cursor >= 0 && m.cursor < len(pageSessions) && m.deletingIDs[pageSessions[m.cursor].ID] {
		for i := m.cursor - dir; i >= 0 && i < len(pageSessions); i -= dir {
			if !m.deletingIDs[pageSessions[i].ID] {
				m.cursor = i
				return
			}
		}
	}
}

// renderDeleteConfirm renders a delete confirmation dialog as a pane overlay.
func (m Model) renderDeleteConfirm() string {
	paneWidth := m.width
	paneWidth = max(paneWidth, 20)
	paneHeight := m.height - 1
	paneHeight = max(paneHeight, 5)

	contentWidth := paneWidth - 2 // horizontal padding inside pane
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
		name := truncateString(m.deleteTargetDesc, contentWidth-10)
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
		name := truncateString(m.deleteTargetDesc, contentWidth-10)
		lines = append(lines,
			warnStyle.Render(fmt.Sprintf("Delete '%s'?", name)),
			"",
			keyStyle.Render("y")+dimStyle.Render(": delete"),
			keyStyle.Render("n")+dimStyle.Render(": cancel"),
		)
	}

	content := strings.Join(lines, "\n")
	placed := lipgloss.Place(contentWidth, paneHeight, lipgloss.Center, lipgloss.Center, content)

	paneStyle := createPaneStyle(paneWidth, paneHeight, m.focused)
	return paneStyle.Render(placed)
}

// renderListContent renders the session list content.
//
// Layout:
//
//	[STATS row]        <- fixed header (top of pane, not scrolled)
//	[blank spacer]     <- fixed header
//	[err / warn ...]
//	[scrollable card area]  <- windowed by m.scrollOffset
//
// The "sessions" title is rendered on the tmux pane-border above via the
// pane's @session_name option, so the content area starts directly with
// the STATS row.
func (m Model) renderListContent(contentWidth int) string {
	var content strings.Builder

	// --- Fixed header (never scrolled) ---
	statusSummary := buildStatusSummary(m.sessions)
	if statusSummary != "" {
		content.WriteString(statusSummary)
		content.WriteString("\n")
	}
	content.WriteString("\n")

	if m.err != nil {
		content.WriteString(lipgloss.NewStyle().Foreground(errorColor).Render(fmt.Sprintf("Error: %v", m.err)))
		content.WriteString("\n\n")
	}
	if m.warning != "" {
		content.WriteString(lipgloss.NewStyle().Foreground(warningColor).Render(fmt.Sprintf("⚠ %s", m.warning)))
		content.WriteString("\n\n")
	}

	// --- Scrollable card area ---
	displaySessions := m.getDisplaySessions()
	if len(displaySessions) == 0 {
		content.WriteString("\n")
		content.WriteString(helpStyle.Render("No sessions. Press 'n' to create one."))
		content.WriteString("\n")
		return content.String()
	}

	// Build the full card content into a separate buffer so we can slice it
	// by lines and expose only the visible window (no per-page arithmetic).
	var cards strings.Builder
	groups := groupSessionsByFleet(displaySessions)
	showHeaders := len(groups) >= 1
	idToIdx := make(map[string]int, len(displaySessions))
	for i, sess := range displaySessions {
		idToIdx[sess.ID] = i
	}
	for _, group := range groups {
		if showHeaders {
			cards.WriteString(renderFleetHeader(group.Name, contentWidth))
		}
		for _, sess := range group.Sessions {
			idx := idToIdx[sess.ID]
			viewed := sess.ID == m.currentSessionID
			cards.WriteString(m.renderSession(sess, idx == m.cursor, viewed, contentWidth))
		}
	}

	// Slice by lines and take a window starting at scrollOffset. The
	// content area size is computed the same way adjustScrollForCursor
	// does, so the two stay in agreement.
	lines := strings.Split(cards.String(), "\n")
	avail := m.contentAreaLines()
	start := m.scrollOffset
	if start < 0 {
		start = 0
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := start + avail
	if end > len(lines) {
		end = len(lines)
	}
	content.WriteString(strings.Join(lines[start:end], "\n"))
	return content.String()
}

// renderHelpLine renders the help line at the bottom
func (m Model) renderHelpLine() string {
	if m.confirmKill {
		name := truncateString(m.killTargetDesc, 20)
		confirmMsg := fmt.Sprintf(" Kill '%s'? y:yes n:no", name)
		return lipgloss.NewStyle().Foreground(warningColor).Bold(true).Render(confirmMsg)
	}
	return helpStyle.Render(" ? help")
}

// renderSession renders a single session as a card.
//
// Two orthogonal indicators:
//
//   - selected → blue cursor bar '▎' running down every card line
//   - viewed   → subdued row background across every card line
//
// The two indicators compose freely: a card can be selected, viewed, both,
// or neither. Roles are visually separate (bar = pointer, background =
// current location) so users never have to disambiguate a single glyph.
//
// A blank line follows every card (no background) so cards read as
// visually separate blocks even when a run of them is highlighted.
func (m Model) renderSession(sess session.Info, selected bool, viewed bool, width int) string {
	// Deleting sessions: dim rendering, not selectable.
	if m.deletingIDs[sess.ID] {
		var b strings.Builder
		name := truncateString(sess.Description, width-2)
		if sess.DescriptionLocked && sess.Description != "" {
			name += "*"
		}
		b.WriteString("  ")
		b.WriteString(deletingStyle.Render(name))
		b.WriteString("\n")
		b.WriteString("    ")
		b.WriteString(deletingStyle.Render("⟳ deleting..."))
		b.WriteString("\n\n")
		return b.String()
	}

	_, statusLabel, statusStyle := getStatusDisplay(sess.Status)

	// withBg composes any inline style with the viewed row background when
	// the card is being displayed on the right. Applying the background per
	// styled segment (rather than wrapping the whole line) sidesteps ANSI
	// reset artifacts between segments — every visible cell carries the bg
	// SGR codes so the background stays continuous.
	withBg := func(s lipgloss.Style) lipgloss.Style {
		if viewed {
			return s.Background(viewedRowBg)
		}
		return s
	}
	bgOnly := withBg(lipgloss.NewStyle())
	padBg := func(n int) string {
		if n <= 0 {
			return ""
		}
		if viewed {
			return bgOnly.Render(strings.Repeat(" ", n))
		}
		return strings.Repeat(" ", n)
	}

	// Cursor bar column (2 cols): blue '▎' on selected cards, blank otherwise.
	// The bar repeats on every subsequent line so the eye can trace it down
	// the whole card.
	var cursorBar string
	if selected {
		cursorBar = withBg(selectedItemStyle).Render("▎ ")
	} else {
		cursorBar = padBg(2)
	}

	// The inner lead prefix carried by every non-header line: cursor bar (2)
	// + 2-column nesting indent = 4 columns total.
	innerLead := cursorBar + padBg(2)

	var b strings.Builder

	// --- Line 1: cursor + name ---
	nameAvail := width - 2 // cursor col
	nameAvail = max(nameAvail, 8)

	name := truncateString(sess.Description, nameAvail)
	if sess.DescriptionLocked && sess.Description != "" {
		name += "*"
	}
	var nameStyled string
	if selected {
		nameStyled = withBg(selectedItemStyle).Render(name)
	} else {
		nameStyled = withBg(sessionNameStyle).Render(name)
	}
	nameW := lipgloss.Width(nameStyled)

	b.WriteString(cursorBar)
	b.WriteString(nameStyled)
	b.WriteString(padBg(width - 2 - nameW))
	b.WriteString("\n")

	// --- Line 2: colored dot + STATUS + branch/workdir ---
	metaStr := sess.CurrentBranch
	if metaStr == "" {
		displayDir := sess.CurrentWorkDir
		if displayDir == "" {
			displayDir = sess.WorkDir
		}
		if displayDir != "" {
			if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(displayDir, home) {
				displayDir = "~" + displayDir[len(home):]
			}
			metaStr = displayDir
		}
	}

	statusCluster := withBg(statusStyle).Render("● " + statusLabel)
	statusClusterW := lipgloss.Width(statusCluster)

	b.WriteString(innerLead)
	b.WriteString(statusCluster)
	usedW := 4 + statusClusterW
	if metaStr != "" {
		const gapW = 3
		metaAvail := width - usedW - gapW
		if metaAvail > 0 {
			if runewidth.StringWidth(metaStr) > metaAvail {
				metaStr = truncateString(metaStr, metaAvail)
			}
			metaStyled := withBg(helpStyle).Render(metaStr)
			b.WriteString(padBg(gapW))
			b.WriteString(metaStyled)
			usedW += gapW + lipgloss.Width(metaStyled)
		}
	}
	b.WriteString(padBg(width - usedW))
	b.WriteString("\n")

	// --- Line 3: last user message ---
	if sess.LastUserMessage != "" {
		iconPrefix := "👤 "
		msgWidth := width - 4 - lipgloss.Width(iconPrefix)
		msgWidth = max(msgWidth, 10)
		msgStr := truncateString(sess.LastUserMessage, msgWidth)
		msgStyled := withBg(helpStyle).Render(iconPrefix + msgStr)
		b.WriteString(innerLead)
		b.WriteString(msgStyled)
		b.WriteString(padBg(width - 4 - lipgloss.Width(msgStyled)))
		b.WriteString("\n")
	}

	// --- Line 4: last assistant message ---
	if sess.LastAssistantMessage != "" {
		iconPrefix := "🤖 "
		msgWidth := width - 4 - lipgloss.Width(iconPrefix)
		msgWidth = max(msgWidth, 10)
		msgStr := truncateStringFromEnd(sess.LastAssistantMessage, msgWidth)
		msgStyled := withBg(helpStyle).Render(iconPrefix + msgStr)
		b.WriteString(innerLead)
		b.WriteString(msgStyled)
		b.WriteString(padBg(width - 4 - lipgloss.Width(msgStyled)))
		b.WriteString("\n")
	}

	// Trailing blank spacer between cards — no background, so cards read
	// as visually distinct blocks even when consecutive rows are highlighted.
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

	// Each cluster: "● N Label" colored by its status style; separated by a
	// dim middle dot. The colored dot replaces the earlier `*` / `>` / `o`
	// ASCII glyphs for a more modern, uniform look.
	var parts []string
	if counts.thinking > 0 {
		parts = append(parts, thinkingStyle.Render(fmt.Sprintf("● %d Thinking", counts.thinking)))
	}
	if counts.permission > 0 {
		parts = append(parts, permissionStyle.Render(fmt.Sprintf("● %d Permission", counts.permission)))
	}
	if counts.running > 0 {
		parts = append(parts, runningStyle.Render(fmt.Sprintf("● %d Running", counts.running)))
	}
	if counts.creating > 0 {
		parts = append(parts, creatingStyle.Render(fmt.Sprintf("● %d Creating", counts.creating)))
	}
	if counts.idle > 0 {
		parts = append(parts, idleStyle.Render(fmt.Sprintf("● %d Idle", counts.idle)))
	}

	if len(parts) == 0 {
		return ""
	}
	sep := lipgloss.NewStyle().Foreground(dimColor).Render("  ·  ")
	return strings.Join(parts, sep)
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
// Uppercased, muted, letter-spaced name — no dashes; whitespace groups items.
func renderFleetHeader(name string, width int) string {
	_ = width // width kept for API parity; layout no longer depends on it
	label := strings.ToUpper(name)
	headerStyle := lipgloss.NewStyle().
		Foreground(secondaryColor).
		Bold(true)
	return headerStyle.Render(label) + "\n"
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
