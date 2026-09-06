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
	"github.com/charmbracelet/x/ansi"
	"github.com/takaaki-s/jind-ai/internal/action"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/jinenv"
	"github.com/takaaki-s/jind-ai/internal/paths"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/tmux"
)

// maxTUIWidth is the maximum width (columns) for the TUI pane.
// When the terminal is maximized, the TUI pane is resized to this width
// so the display pane gets the extra space.
const maxTUIWidth = 50
const minTUIWidth = 30

// placeholderSessionID is the sentinel currentSessionID carries while the
// display pane shows the placeholder rather than a session. The leading "_"
// keeps it out of the session-ID namespace (UUIDs).
const placeholderSessionID = "_empty"

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

	// Async delete tracking
	deletingIDs map[string]bool // Session IDs currently being deleted

	// Focus tracking (for visual focus indicator)
	focused bool // true when TUI pane has focus (changes border/title color)

	// tmux integration
	tmuxClient         *tmux.Client // outer tmux client (-L jin-mgr, nil in legacy mode)
	popups             popupOpener  // outer tmux popup spawner; same client, narrowed — see popupOpener
	innerTmuxClient    *tmux.Client // inner tmux client (-L jin, for switch-client)
	tuiPaneID          string       // TUI pane unique ID (e.g. "%42") in outer tmux
	displayPaneID      string       // Right pane unique ID (for session display) in outer tmux
	currentSessionID   string       // Session ID currently displayed in right pane
	displayLocalAttach bool         // true when display pane is running tmux attach to inner tmux

	// Focus after create
	focusSessionID string // Session ID to focus after creation

	// Reswitch after kill. Holds the session whose Kill was issued, not a bare
	// "something needs reswitching" bit: only a sessionsMsg that actually
	// reflects the kill may consume it (see updateListMode's sessionsMsg
	// branch, and docs/gotchas.md for why a bool cannot work here).
	pendingKillID string

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

	// identity is the jin every popup this Model opens must reach: the daemon
	// `jin ui` validated, rather than whichever one the environment a popup
	// happens to start from names. Handed in at construction so a popup and the
	// list behind it cannot answer "which jin" differently.
	identity jinenv.Identity
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
//
// identity travels as a parameter and is not defaulted: a Model that resolved
// one for itself would answer "which jin" from the outer tmux server's
// environment, which named a different daemon than `jin ui` had just validated
// in 3 of 3 trials.
func NewModelWithTmux(client *daemon.Client, tc, innerTC *tmux.Client, tuiPaneID, displayPaneID string, identity jinenv.Identity) Model {
	m := NewModel(client)
	m.tmuxClient = tc
	m.identity = identity
	// The same client, narrowed to the one call openPopup makes. No nil guard:
	// tc is dereferenced a few lines down, so this constructor already requires a
	// live client. Models with no tmux come from NewModel, which leaves it nil.
	m.popups = tc
	m.innerTmuxClient = innerTC
	m.tuiPaneID = tuiPaneID
	m.displayPaneID = displayPaneID
	// One tmux call covers both startup reads below.
	env := tc.ListEnvironment(tmux.SessionName)
	// Restore which session was displayed (for reattach)
	m.currentSessionID = env["JIN_CURRENT_SESSION"]
	// Point the cursor at that restored session on the first sessionsMsg so
	// relaunching the TUI keeps the left-list selection aligned with the
	// right pane the user was looking at.
	m.pendingCursorRestore = m.currentSessionID != ""
	// Reset JIN_CURSOR_SESSION at startup — sessions have not been fetched
	// yet, so publish an empty value so a stale env from a prior TUI run does
	// not confuse a popup that opens before the first sessionsMsg arrives.
	m.writeCursorEnv()
	// Same reason, higher stakes: the outer tmux server outlives the TUI, so a
	// confirm answered in the last 250ms of a previous run is still sitting in its
	// env, and our first envTick would replay it as an approval to kill or delete
	// a session this run never asked about. Nothing useful to do with a failure
	// here: a key that survives is inert until paired with an answer this run
	// would have to write itself.
	_ = clearConfirmEnv(tc, staleConfirmKeys(env))
	return m
}

// noticeLines returns how many rows the error / warning notices occupy above
// the list header, and is the sole owner of that arithmetic: everything below
// them — contentAreaLines, and the row the mouse hit-test counts from —
// derives from it. Kept in sync with renderListContent's prologue by hand.
func (m *Model) noticeLines() int {
	rows := 0
	if m.err != nil {
		rows += 2 // "Error: ..." + blank
	}
	if m.warning != "" {
		rows += 2 // "⚠ ..." + blank
	}
	return rows
}

// helpChromeLines is what View() spends below the pane: the rule that cuts the
// detail pane off from the chrome, plus the help line itself. Both View() and
// contentAreaLines subtract it, which is the whole reason it is a constant.
const helpChromeLines = 2

// contentAreaLines returns the number of lines available below the notices —
// the pane height minus error / warning rows when active. It is the parent of
// the three regions the pane is divided into (list header, scrollable list,
// detail pane), not the size of any one of them.
func (m *Model) contentAreaLines() int {
	// Pane holds (m.height - helpChromeLines) rows; the rest is the rule and
	// the help line View() draws underneath it.
	return max(m.height-helpChromeLines-m.noticeLines(), 3)
}

// Row budget of the three regions inside contentAreaLines.
const (
	// sessionRowHeight is the number of rows one session occupies in the list.
	// Constant by construction: renderSession emits exactly this many lines
	// whatever the session carries, so the scroll and hit-test arithmetic below
	// cannot desync from the renderer.
	//
	// Two rows rather than one buys a pointer target the list is tapped with over
	// SSH from a phone, where one cell is thinner than a fingertip, and somewhere
	// to put the repo / branch so the list reads as a table instead of a column of
	// names. It costs half the list's density.
	sessionRowHeight = 2
	// listHeaderLines covers the count line plus one blank spacer. Without the
	// spacer the header collides visually with the first fleet header.
	listHeaderLines = 2
	// detailNameLines is how many rows the detail pane reserves for the session
	// name. On one row the pane showed exactly what a list row did — the same name
	// cut at the same column — so two is the smallest budget that beats it.
	//
	// The name gets both rows whether or not it fills them. Sizing the block to
	// the name instead would put the session under the cursor back into the list
	// height adjustScrollForCursor derives the viewport from: move the cursor, the
	// list resizes, the viewport scrolls, repeat.
	detailNameLines = 2
	// detailMsgLines is how many rows the pane gives each of the last user and
	// assistant messages. One row held about 16 Japanese characters at the widths
	// this pane runs at — enough to say a message exists, not enough to say which
	// one. Two rows is not "readable" either and is not meant to be: it is where
	// a message becomes identifiable, and reading one is what attaching is for.
	detailMsgLines = 2
	// detailPaneLines covers rule + name (detailNameLines rows) + status + the
	// last user and assistant messages (detailMsgLines rows each). Fixed even when
	// fields are empty: every scroll and hit-test calculation is built on the
	// height, so it must not depend on the session under the cursor.
	//
	// The repo/branch row moved onto the second row of every list row, and the row
	// it vacated here is what paid for the second message row.
	detailPaneLines = 2 + detailNameLines + 2*detailMsgLines
	// minListLines is the smallest list we shrink to before the detail pane is
	// dropped whole. Degrading the pane gradually instead would make its height a
	// third variable in every geometry test.
	//
	// A multiple of sessionRowHeight, because the floor is the one list height we
	// choose outright: an odd floor would spend its last row drawing half of a
	// session nobody can read. Only the floor is even — any other height may leave
	// the list odd, and the bottom session then shows one of its two rows. That is
	// accepted: adjustScrollForCursor pulls the session under the cursor fully
	// into view, so a half-drawn row is never the one the next action hits.
	//
	// With sessionRowHeight and detailPaneLines where they now are, the pane
	// appears only from m.height >= 18 (20 with one notice, 22 with two). Below
	// the threshold it goes whole, so the user sees it vanish rather than shrink.
	minListLines = 6
)

// headerLines returns the rows the list header occupies. Zero when there are
// no sessions: the empty state replaces the whole list, header included.
func (m *Model) headerLines() int {
	if len(m.getDisplaySessions()) == 0 {
		return 0
	}
	return listHeaderLines
}

// detailVisible reports whether the detail pane is drawn for the session under
// the cursor. It derives from contentAreaLines and the constants alone and
// never calls listAreaLines — listAreaLines subtracts detailLines, so consulting
// it here would be a cycle.
//
// With no notices and a valid cursor the threshold works out to m.height >= 18.
//
// The cursor-range check is not merely defensive: renderListContent indexes the
// session slice with m.cursor to pick the pane's subject, so an out-of-range
// cursor is a panic rather than a blank pane.
//
// A scrolled-away cursor still gets its pane. PageUp / PageDown move the
// viewport and deliberately leave the cursor put, so the pane keeps describing
// the session the next action will actually hit.
func (m *Model) detailVisible() bool {
	sessions := m.getDisplaySessions()
	if m.cursor < 0 || m.cursor >= len(sessions) {
		return false
	}
	return m.contentAreaLines()-m.headerLines()-detailPaneLines >= minListLines
}

// detailLines returns the rows the detail pane occupies: all of them or none
// (see minListLines).
func (m *Model) detailLines() int {
	if m.detailVisible() {
		return detailPaneLines
	}
	return 0
}

// listAreaLines returns the height of the scrollable session list — what is
// left of contentAreaLines once the header and the detail pane have taken
// their fixed shares. Every scroll and mouse calculation measures against this,
// not against contentAreaLines.
func (m *Model) listAreaLines() int {
	return max(m.contentAreaLines()-m.headerLines()-m.detailLines(), 1)
}

// pageScrollLines returns how many lines PageUp / PageDown scrolls the
// viewport — one visible page worth of rows, minus one line of overlap so
// the user keeps a reference row across the jump.
func (m *Model) pageScrollLines() int {
	return max(m.listAreaLines()-1, 1)
}

// sessionCardTop returns the line offset (within the scrollable list area,
// 0 = first row of the first fleet header or session row) where the session at
// display-index `idx` starts, and its height. Returns (-1, 0) if idx is out of
// range. The scan survives the move to a constant row height because fleet
// headers still insert rows that belong to no session.
func (m *Model) sessionCardTop(idx int) (top, height int) {
	sessions := m.getDisplaySessions()
	if idx < 0 || idx >= len(sessions) {
		return -1, 0
	}
	targetID := sessions[idx].ID
	line := 0
	for _, g := range groupSessionsByFleet(sessions) {
		line++ // fleet header row; every group draws one
		for _, sess := range g.Sessions {
			if sess.ID == targetID {
				return line, sessionRowHeight
			}
			line += sessionRowHeight
		}
	}
	return -1, 0
}

// totalCardLines returns the total number of lines the scrollable list area
// currently spans, used to clamp scrollOffset so we cannot scroll past the
// last session. Plain arithmetic now that every row is sessionRowHeight tall:
// one row per session plus the fleet header rows sessionCardTop counts.
func (m *Model) totalCardLines() int {
	sessions := m.getDisplaySessions()
	return len(sessions)*sessionRowHeight + len(groupSessionsByFleet(sessions))
}

// adjustScrollForCursor moves scrollOffset so the current cursor's row is
// visible in the list area. Called after any cursor movement.
func (m *Model) adjustScrollForCursor() {
	top, height := m.sessionCardTop(m.cursor)
	if top < 0 {
		m.scrollOffset = 0
		return
	}
	avail := m.listAreaLines()
	bottom := top + height
	if top < m.scrollOffset {
		m.scrollOffset = top
	} else if bottom > m.scrollOffset+avail {
		m.scrollOffset = bottom - avail
	}
	m.clampScroll()
}

// scrollBy moves the viewport by lines (negative = towards the top), clamped to
// the content bounds and then pulled back to the top of whatever session row it
// landed inside. The cursor deliberately stays put — every scroll-only input
// shares that contract, so looking around never changes what the next action
// targets.
//
// The pull-back cannot be replaced by a friendlier step size. A session is
// sessionRowHeight rows while a fleet header is one, so no step stays in phase
// with the grid: over one fleet header, wheelScrollLines lands mid-session on
// half its notches and a step of sessionRowHeight lands there on every one of
// them. A viewport that opens mid-session puts a row's repo/branch line directly
// under the list header with its name scrolled away, which reads as a session
// that has no name.
//
// Alignment belongs here and NOT in adjustScrollForCursor, which owes the
// cursor's row whole and anchors on that row's bottom for exactly that reason.
//
// Two landings are deliberately left where they fall:
//
//   - The last page, where the list has already run out of rows to align to.
//     Pulling back there would put the final row permanently out of reach.
//   - A step the pull-back would swallow whole (top == from). PageDown moves
//     listAreaLines-1 rows, which is a single row on a two-row list, and taking
//     it back on every press would pin the viewport where it stands.
func (m *Model) scrollBy(lines int) {
	from := m.scrollOffset
	m.scrollOffset += lines
	m.clampScroll()

	if m.scrollOffset >= m.maxScrollOffset() {
		return
	}
	// A fleet header row, or past the last row: neither is half of anything.
	idx, ok := m.sessionIndexAtLine(m.scrollOffset)
	if !ok {
		return
	}
	if top, _ := m.sessionCardTop(idx); top != from {
		m.scrollOffset = top
	}
}

// maxScrollOffset is the last page: the largest scrollOffset the content
// allows, or 0 while the list is shorter than its viewport. clampScroll bounds
// against it and scrollBy tests for it — if the two disagreed about where the
// last page starts, an alignment could pull the final row out of reach.
func (m *Model) maxScrollOffset() int {
	return max(m.totalCardLines()-m.listAreaLines(), 0)
}

// clampScroll bounds scrollOffset into [0, maxScrollOffset()]. Call after any
// change that shrinks or grows the content (session list change, filter toggle,
// window resize).
func (m *Model) clampScroll() {
	m.scrollOffset = min(max(m.scrollOffset, 0), m.maxScrollOffset())
}

// sessionIndexAtLine is the inverse of sessionCardTop: it maps a line offset
// inside the scrollable list area to the display-index of the session drawn on
// that line. Fleet header rows belong to no session and report false, as does
// any line past the last row.
//
// Implemented by scanning sessionCardTop rather than re-walking the groups, so
// the layout is described in exactly one place.
//
// The scan runs to the end rather than stopping at the first top past `line`.
// Stopping early would assume row tops ascend with the display index, which only
// holds while session.SortInfos and groupSessionsByFleet keep agreeing — an
// agreement between two packages that nothing checks, and a reordering on either
// side would turn every click below the first out-of-order session into a hit on
// the wrong one.
func (m *Model) sessionIndexAtLine(line int) (int, bool) {
	for i := range m.getDisplaySessions() {
		top, height := m.sessionCardTop(i)
		if top < 0 {
			continue
		}
		if line >= top && line < top+height {
			return i, true
		}
	}
	return 0, false
}

// sessionIndexAtRow maps a mouse event's pane-relative row to the display-index
// of the session drawn there, accounting for the notice and list header rows
// above the list area and the current scroll offset. Only the list area is
// live: neither the header band nor the detail pane below it draws a session.
func (m *Model) sessionIndexAtRow(y int) (int, bool) {
	top := m.noticeLines() + m.headerLines()
	if y < top || y >= top+m.listAreaLines() {
		return 0, false
	}
	return m.sessionIndexAtLine(y - top + m.scrollOffset)
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

// attachedSessionMsg carries the inner tmux session name the display-pane
// client is currently attached to ("" when unknown / no client).
type attachedSessionMsg string

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

	// sessionTickInterval controls how often the TUI refetches the session list
	// from the daemon. Longer than envTickInterval because refetches touch the
	// daemon socket and re-render the full list. The display-pane attach poll
	// rides on this tick, so it also bounds how long a tmux-side session switch
	// takes to reflect in the TUI.
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

// pollAttachedSessionCmd returns a Cmd that reads which inner tmux session the
// display-pane client is attached to, so adoptAttachedSession can follow a
// switch the user made from inside the pane (choose-tree etc.). Returns nil
// unless the display pane is locally attached and both tmux clients are wired.
// The two tmux calls run in the Cmd closure to keep them off the Update loop.
func (m *Model) pollAttachedSessionCmd() tea.Cmd {
	if !m.displayLocalAttach || m.tmuxClient == nil || m.innerTmuxClient == nil || m.displayPaneID == "" {
		return nil
	}
	tmuxClient := m.tmuxClient
	innerTmuxClient := m.innerTmuxClient
	displayPaneID := m.displayPaneID
	return func() tea.Msg {
		tty, err := tmuxClient.GetPaneTTY(displayPaneID)
		if err != nil || tty == "" {
			return attachedSessionMsg("")
		}
		attached, err := innerTmuxClient.ClientSessionForTTY(tty)
		if err != nil {
			return attachedSessionMsg("")
		}
		return attachedSessionMsg(attached)
	}
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
	if !m.moveCursorToSession(m.focusSessionID) {
		return false
	}
	m.currentSessionID = "" // Force reset so switchToSession runs even when the cursor was already on this session.
	m.switchToSession(m.focusSessionID)
	m.focusSessionID = ""
	return true
}

// buildInnerAttachCmd assembles the shell command the display pane runs to
// attach to an inner tmux session. socketName must be the *resolved* inner
// socket (tmux.DefaultSocketName), never the tmux.SocketName constant: this
// string is handed to a fresh `tmux` process, so it is the one place the display
// pane's socket is chosen independently of the Client objects the Model holds.
// Passing the constant sends the pane to the real "jin" server even when
// JIN_TMUX_SOCKET points everything else somewhere else.
func buildInnerAttachCmd(socketName, innerSession string) string {
	// Unset $TMUX so tmux does not refuse with "sessions should be nested
	// with care": the display pane runs inside the outer tmux, so $TMUX points
	// to the outer session and attaching to the inner one on the same host is
	// rejected as nesting.
	//
	// Chain `tail -f /dev/null` after attach so a quick attach failure — or a
	// later user-initiated detach — leaves the pane with a still-running
	// process. Without it the shell exits, and remain-on-exit=on surfaces
	// tmux's "Pane is dead" overlay until the next respawn.
	return fmt.Sprintf("env -u TMUX tmux -L %s attach -t %s; tail -f /dev/null", socketName, innerSession)
}

// switchToSession displays the given session in the right pane via RespawnPane:
// an inner tmux attach for local sessions (-L jin by default, or whatever
// JIN_TMUX_SOCKET resolves to), an SSH attach for remote ones, and a
// placeholder carrying session info for stopped or errored ones.
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
	if !isLocalAlive {
		m.detachInnerClient()
	}

	// Stopped/error sessions: show placeholder in right pane (no TmuxWindowName needed).
	// CreationWarning (non-fatal notice from async provisioning, e.g. hook
	// not allowed) is appended when set — the session still worked, but the
	// note is worth surfacing anywhere the placeholder is visible.
	if !isSessionAlive(sess.Status) {
		var placeholderCmd string
		switch {
		case sess.ErrorMessage != "" && sess.CreationWarning != "":
			placeholderCmd = fmt.Sprintf(
				"printf '\\n  Session: %s\\n  Status:  %s\\n\\n  Error:\\n%s\\n\\n  Warning:\\n%s\\n'; tail -f /dev/null",
				sess.Description, sess.Status, sess.ErrorMessage, sess.CreationWarning,
			)
		case sess.ErrorMessage != "":
			placeholderCmd = fmt.Sprintf(
				"printf '\\n  Session: %s\\n  Status:  %s\\n\\n  Error:\\n%s\\n'; tail -f /dev/null",
				sess.Description, sess.Status, sess.ErrorMessage,
			)
		case sess.CreationWarning != "":
			placeholderCmd = fmt.Sprintf(
				"printf '\\n  Session: %s\\n  Status:  %s\\n\\n  Warning:\\n%s\\n\\n  Press Enter to restart\\n'; tail -f /dev/null",
				sess.Description, sess.Status, sess.CreationWarning,
			)
		default:
			placeholderCmd = fmt.Sprintf(
				"printf '\\n  Session: %s\\n  Status:  %s\\n\\n  Press Enter to restart\\n'; tail -f /dev/null",
				sess.Description, sess.Status,
			)
		}
		_ = m.tmuxClient.RespawnPane(m.displayPaneID, placeholderCmd, nil)
		_ = m.tmuxClient.ClearHistory(m.displayPaneID)
		m.recordDisplayedSession(sess)
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
				m.recordDisplayedSession(sess)
				return
			}
		}
		// switch-client failed — fall through to respawn
	}

	// Local: respawn right pane with inner tmux attach. The socket is resolved
	// the same way m.innerTmuxClient was built (tmux.NewClient →
	// DefaultSocketName), so the pane and the Model agree on which inner
	// server they are talking about.
	attachCmd := buildInnerAttachCmd(tmux.DefaultSocketName(), sess.TmuxWindowName)
	_ = m.tmuxClient.RespawnPane(m.displayPaneID, attachCmd, nil)
	_ = m.tmuxClient.ClearHistory(m.displayPaneID)
	m.displayLocalAttach = true

	m.recordDisplayedSession(sess)
}

// adoptAttachedSession aligns TUI state (currentSessionID, cursor,
// @session_name label, JIN_CURRENT_SESSION env) to the inner session the display
// pane is actually attached to, after the user switched it from inside the pane
// (choose-tree etc.). State adoption only: it never issues switch-client back,
// so a tmux-side switch and a TUI-side switch cannot ping-pong.
func (m *Model) adoptAttachedSession(attached string) {
	if attached == "" || !m.displayLocalAttach {
		return
	}
	// TmuxWindowName is the inner tmux *session* name (one per jin session) —
	// the same namespace as #{client_session}.
	i := slices.IndexFunc(m.sessions, func(s session.Info) bool {
		return s.TmuxWindowName == attached
	})
	if i < 0 {
		return // jin-unmanaged session: leave the TUI untouched.
	}
	sess := &m.sessions[i]
	// Already in sync (steady state). Same-session window switches also land
	// here since client_session is unchanged.
	if sess.ID == m.currentSessionID {
		return
	}

	m.recordDisplayedSession(sess)
	// Follow the cursor only when the adopted session is visible; a filter may
	// exclude it, in which case we still adopt the ID/label/env.
	m.moveCursorToSession(sess.ID)
}

// detachInnerClient detaches the inner tmux client running in the display pane,
// so the "tmux attach" process exits cleanly and the pane does not go dead.
// No-op when the pane is not running an attach. It owns displayLocalAttach, so
// the flag cannot drift from the detach.
func (m *Model) detachInnerClient() {
	if !m.displayLocalAttach {
		return
	}
	m.displayLocalAttach = false
	if m.innerTmuxClient == nil {
		return
	}
	paneTTY, err := m.tmuxClient.GetPaneTTY(m.displayPaneID)
	if err != nil || paneTTY == "" {
		return
	}
	_ = m.innerTmuxClient.DetachClientByTTY(paneTTY)
}

// recordDisplayedSession records which session the display pane now shows:
// currentSessionID, the JIN_CURRENT_SESSION outer-tmux env var, and the pane
// border label. Shared tail of every path that changes the displayed session
// (switchToSession's exits and adoptAttachedSession).
func (m *Model) recordDisplayedSession(sess *session.Info) {
	m.currentSessionID = sess.ID
	if m.tmuxClient != nil {
		_ = m.tmuxClient.SetEnvironment(tmux.SessionName, "JIN_CURRENT_SESSION", sess.ID)
	}
	m.pushDisplayedDescription(sess.Description)
}

// clearDisplayedSession is recordDisplayedSession's inverse, for when the pane
// stops showing a session at all: it drops all four of the things its
// counterpart writes. See docs/gotchas.md — pane options outlive the process
// in the pane, so the label has to be reset by hand.
func (m *Model) clearDisplayedSession() {
	m.currentSessionID = placeholderSessionID
	if m.tmuxClient != nil {
		_ = m.tmuxClient.UnsetEnvironment(tmux.SessionName, "JIN_CURRENT_SESSION")
	}
	m.pushDisplayedDescription("")
}

// moveCursorToSession points the list cursor at the given session and
// republishes the cursor env. Returns false without moving anything when the
// session is not in the display list (e.g. hidden by a filter).
func (m *Model) moveCursorToSession(id string) bool {
	i := slices.IndexFunc(m.getDisplaySessions(), func(s session.Info) bool {
		return s.ID == id
	})
	if i < 0 {
		return false
	}
	m.cursor = i
	m.adjustScrollForCursor()
	m.writeCursorEnv()
	return true
}

// pushDisplayedDescription sets the display pane's `@session_name` tmux
// variable and records the value locally so refreshDisplayedDescription can
// detect drift without re-issuing set-option every poll.
func (m *Model) pushDisplayedDescription(desc string) {
	if m.tmuxClient == nil || m.displayPaneID == "" {
		return
	}
	_ = m.tmuxClient.SetPaneOption(m.displayPaneID, tmux.PaneLabelOption, desc)
	m.lastDisplayedDesc = desc
}

// refreshDisplayedDescription re-pushes the pane label when the currently
// displayed session's Description has changed since the last poll (e.g.
// Layer C promoted the baseline to a transcript-derived label). Cheap: it
// walks m.sessions once and calls set-option at most once per drift.
func (m *Model) refreshDisplayedDescription() {
	if m.tmuxClient == nil || m.displayPaneID == "" || !m.displaysLiveSession() {
		return
	}
	i := slices.IndexFunc(m.sessions, func(s session.Info) bool {
		return s.ID == m.currentSessionID
	})
	if desc := m.sessions[i].Description; desc != m.lastDisplayedDesc {
		m.pushDisplayedDescription(desc)
	}
}

// respawnPlaceholder replaces the display pane with a placeholder command and
// strips every trace of the session it used to show, detaching any active inner
// tmux client first. Idempotent via the placeholder sentinel: callers may reach
// it on every poll — an empty list, or a cursor parked on a session still being
// deleted — and only the first one respawns.
func (m *Model) respawnPlaceholder() {
	if m.currentSessionID == placeholderSessionID {
		return
	}
	if m.tmuxClient != nil && m.displayPaneID != "" {
		m.detachInnerClient()
		_ = m.tmuxClient.RespawnPane(m.displayPaneID, tmux.PlaceholderCmd, nil)
		_ = m.tmuxClient.ClearHistory(m.displayPaneID)
	}
	// Runs even without a display pane (legacy mode, tests), where it just
	// records "no session is on screen" — which is what the callers mean.
	m.clearDisplayedSession()
}

// showCursorSession points the display pane at the session under the cursor,
// falling back to the placeholder when there is nothing attachable there — an
// empty list, or a cursor sitting on a session that is itself being deleted.
//
// force re-points the pane even when it already claims the cursor session. Only
// the kill path needs it: a killed session stays in the list, so the pane looks
// settled while actually holding an attach to a tmux session that is gone.
// Everywhere else skipping saves a detach / respawn / re-attach the user sees.
func (m *Model) showCursorSession(force bool) {
	sess, ok := m.cursorSession()
	if !ok {
		m.respawnPlaceholder()
		return
	}
	if !force && sess.ID == m.currentSessionID {
		return
	}
	// Clear first so switchToSession does not take its "already displaying
	// this" early return, then detach: the inner session behind the current
	// attach may already be dead, and switch-client cannot recover from that.
	m.currentSessionID = ""
	m.detachInnerClient()
	m.switchToSession(sess.ID)
}

// displaysLiveSession reports whether the display pane is showing a session
// that is still in the list. False for the placeholder, for a not-yet-decided
// pane at startup, and for a session that disappeared between polls.
func (m Model) displaysLiveSession() bool {
	if m.currentSessionID == "" || m.currentSessionID == placeholderSessionID {
		return false
	}
	return slices.ContainsFunc(m.sessions, func(s session.Info) bool {
		return s.ID == m.currentSessionID
	})
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
	if m.isDeleting(sess) {
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

// wheelScrollLines is how many lines one wheel notch moves the card viewport.
// Three is the conventional terminal step and keeps part of a card in view
// across a notch, so the list never jumps without a reference row.
//
// Deliberately NOT a multiple of sessionRowHeight. That looks like the way to
// keep the viewport on row boundaries and is measurably worse: a fleet header is
// one row, so a step of sessionRowHeight lands mid-session on every notch where
// three lands there on half of them. scrollBy aligns the landing instead.
const wheelScrollLines = 3

// handleMouse handles pointer input over the session list: the wheel scrolls,
// and a left click takes two taps to switch sessions — the first moves the
// cursor onto the row, the second acts on it. A click on a fleet header, on
// empty space, or on a session being deleted does nothing — unlike keyboard
// movement, a click names one specific target.
//
// Stated exactly, the rule is not "two taps" but "a click on the cursor's row
// acts, any other click moves the cursor there" — so a row the cursor already
// sits on acts on the FIRST tap. That is not a corner case: the cursor starts on
// the first session, which makes the top row the one a new user is likeliest to
// try. The user-facing text says "two clicks" because that is what the pointer
// does from a standing start; this is where the exception is written down.
//
// The two taps exist because this list is driven by a fingertip over SSH as
// often as by a mouse, and a row is two cells tall against a finger that covers
// more. Splitting look from act makes a mis-tap "I looked at the wrong session",
// which the next tap fixes, instead of a switch that costs a switch back. The
// keyboard keeps its one-key Enter — the cursor is already where the user put it.
//
// What the first tap arms is a ROW, not a session: the list is replaced whole
// every two seconds and m.cursor does not follow a session across the swap, so a
// second tap lands on whatever session now sits under the pointer. That is the
// same target a one-tap click would have hit; the only thing the swap costs is
// the promise that the second tap acts on the session the first one previewed.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.scrollBy(-wheelScrollLines)
		return m, nil

	case tea.MouseButtonWheelDown:
		m.scrollBy(wheelScrollLines)
		return m, nil

	case tea.MouseButtonLeft:
		// Press only: drag-motion and release events carry the same button and
		// would otherwise re-fire the selection.
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		// Hit-test before touching m.warning — clearing it shifts the card area
		// up two rows, and the user aimed at the layout they could see.
		idx, ok := m.sessionIndexAtRow(msg.Y)
		if !ok {
			return m, nil
		}
		if _, ok := m.sessionAt(idx); !ok {
			return m, nil
		}
		// Read before the cursor moves: after the assignment every click looks
		// like a click on the cursor.
		act := idx == m.cursor
		m.warning = ""
		m.cursor = idx
		m.adjustScrollForCursor()
		m.writeCursorEnv()
		if !act {
			return m, nil
		}
		return m.handleSelectSession()
	}
	return m, nil
}

// sessionAt returns the session at a display-index. The bool is false when
// there is nothing actionable there: empty list, index out of range, or a
// target already transitioning away (being deleted). Every caller that acts on
// "some selected session" goes through this so the guards cannot drift apart —
// the cursor via cursorSession, the pointer via handleMouse.
func (m Model) sessionAt(idx int) (session.Info, bool) {
	ps := m.getDisplaySessions()
	if idx < 0 || idx >= len(ps) {
		return session.Info{}, false
	}
	sess := ps[idx]
	if m.isDeleting(sess) {
		return session.Info{}, false
	}
	return sess, true
}

// cursorSession returns the session under the cursor, or false when there is
// nothing actionable there (see sessionAt).
func (m Model) cursorSession() (session.Info, bool) {
	return m.sessionAt(m.cursor)
}

// currentCursorSessionID returns the session ID under the cursor, or "" when
// there is no actionable target (see cursorSession).
func (m Model) currentCursorSessionID() string {
	sess, ok := m.cursorSession()
	if !ok {
		return ""
	}
	return sess.ID
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

// popupOpener is the whole outer-tmux surface openPopup needs: the one call
// that actually spawns the popup. *tmux.Client satisfies it directly; tests
// inject a recorder.
//
// The interface exists because that spawn is otherwise unobservable, and that is
// measured rather than assumed: with the call wired straight to the concrete
// client, replacing openPopup's whole body with a no-op passed the entire suite
// including under -tags e2e, and so did deleting only the fallback retry below.
type popupOpener interface {
	DisplayPopup(tmux.DisplayPopupOptions) error
}

// openPopup runs one of the hidden `jin <name>-popup` UIs inside a tmux popup,
// sized via configMgr.GetPopupSize(name). No-op when the popup opener or
// configMgr is unwired; popup errors are swallowed, since there is no useful
// recovery from a failed spawn mid-update-loop.
func (m Model) openPopup(name, title string) {
	if m.popups == nil || m.configMgr == nil {
		return
	}
	opts := m.popupDisplayOptions(name, title)
	if err := m.popups.DisplayPopup(opts); err == nil {
		return
	}
	// A popup sized in absolute cells is refused outright by tmux when the client
	// is smaller than the request rather than being shrunk to fit, and the failure
	// is invisible from here. Retry at the percentage fallback, which cannot
	// exceed the client — otherwise a client too small for the confirm dialog
	// would turn a destructive keypress into a no-op. The fallback reports false
	// when this popup was not sized in cells to begin with.
	w, h, ok := m.configMgr.PopupFallbackPercent(name)
	if !ok {
		return
	}
	opts.Width, opts.Height = w, h
	_ = m.popups.DisplayPopup(opts)
}

// popupDisplayOptions resolves the tmux display-popup arguments for a canonical
// popup name. Both the size and the subcommand are looked up from config's popup
// catalog, so config keys and cobra subcommand names cannot silently drift.
//
// Cmd and Env answer two different questions, and only one is about this build.
// What these popups open is more of this UI, so os.Executable() is the right
// binary to run — jinenv.Identity.BinPath says why it is the wrong one to
// advertise as JIN_BIN, which is a callback address rather than a program.
//
// Env carries the identity this Model was handed, because leaving it unset does
// not mean "inherit sensibly". A popup tmux opens is given the tmux server's
// environment with the session's entries layered over it — neither of which this
// process writes, and both of which outlive it: with a stale JIN_SOCKET on the
// outer server, this TUI and its popups alike reached a daemon `jin ui` had not
// validated, 3 of 3 trials. -e beats both layers (3 of 3).
//
// JIN_PLUGIN_DEPTH is among them, always empty: a depth the outer server happens
// to hold would otherwise make every `jin plugin run` issued from inside this
// popup refuse itself as a chain, with nothing on screen to say so. The session
// id is empty on purpose — a popup is this UI, not work belonging to one of the
// sessions it lists — and emitting it empty is also what clears a stale id the
// outer server may be holding.
func (m Model) popupDisplayOptions(name, title string) tmux.DisplayPopupOptions {
	width, height := m.configMgr.GetPopupSize(name)
	selfBin, _ := os.Executable()
	return tmux.DisplayPopupOptions{
		Width:  width,
		Height: height,
		Cmd:    fmt.Sprintf("'%s' %s", selfBin, config.PopupSubcmd(name)),
		Title:  title,
		Env:    m.identity.TmuxEnviron(""),
	}
}

// handleNew opens the session-creation popup in outer tmux. Matches the
// former inline keys.New case verbatim.
func (m Model) handleNew() (tea.Model, tea.Cmd) {
	m.openPopup(config.PopupCreate, " New Session ")
	return m, nil
}

// openConfirmPopup asks the standalone `jin confirm-popup` UI to confirm a
// destructive action. The prompt runs in its own tmux popup rather than in this
// pane because a popup owns keyboard focus while open — when the action palette
// launched the action, focus sits on the display pane, so an in-pane prompt
// could not be answered without a manual pane switch.
//
// No-op without an outer tmux client (legacy mode / tests).
func (m Model) openConfirmPopup(req confirmRequest) {
	if m.tmuxClient == nil {
		return
	}
	// A prompt this process could not describe in full must not be shown: the
	// popup would name whichever fragment of req landed while
	// dispatchConfirmResult acted on the rest. writeConfirmRequest wipes the
	// handshake on its way out of a failure, so the user just presses again.
	if err := writeConfirmRequest(m.tmuxClient, req); err != nil {
		return
	}
	m.openPopup(config.PopupConfirm, " Confirm ")
}

// confirmRequest is one confirm-popup invocation: which dialog to show and
// which session it names.
type confirmRequest struct {
	mode       string
	targetID   string
	targetDesc string
}

// Outer-tmux env keys carrying the confirm handshake: the parent writes the
// prompt, the popup writes the answer, the parent consumes all four on its next
// envTick. Spelled once because four sites have to agree on them exactly, and a
// key only one site knows about is a destructive request stranded in a tmux
// server that outlives this process. Exported for the fourth, another process.
const (
	EnvConfirmMode       = "JIN_CONFIRM_MODE"
	EnvConfirmTargetID   = "JIN_CONFIRM_TARGET_ID"
	EnvConfirmTargetDesc = "JIN_CONFIRM_TARGET_DESC"
	EnvConfirmResult     = "JIN_CONFIRM_RESULT"
)

// confirmEnvKeys is the whole handshake, the unit every clear works in: a
// leftover subset is a destructive request a later tick — or the next TUI
// process — can pair with something else.
var confirmEnvKeys = []string{EnvConfirmResult, EnvConfirmMode, EnvConfirmTargetID, EnvConfirmTargetDesc}

// confirmEnvWriter is the minimal outer-tmux surface the confirm handshake
// writes through. *tmux.Client satisfies it directly; tests inject a fake.
// Same shape as cmd/jin/cmd's agentEnvSetter, for the same reason: the writes
// are the whole observable effect of the code below.
type confirmEnvWriter interface {
	SetEnvironment(session, name, value string) error
	UnsetEnvironment(session, name string) error
}

// writeConfirmRequest publishes one confirm-popup invocation to the outer tmux
// env. These three values are the popup's whole input; the target ID is written
// for our own benefit, since the popup never reads it but it comes back
// alongside the answer so dispatchConfirmResult knows what to act on.
//
// The whole handshake — not just the previous answer — is wiped before any of
// the new values land, and a failed write wipes it again before returning. Both
// halves matter:
//
//   - A popup answered less than one envTick ago has left a result behind that
//     this tick would otherwise pair with the target being written here,
//     destroying a session the user answered "no" to, or never saw.
//   - A dismissed popup (Ctrl+C) deliberately leaves its mode/target behind, so
//     writing only some of the new values on top would splice two requests.
//     Clearing turns any partial write into an *empty* key, which both the popup
//     and dispatchConfirmResult already refuse to act on.
//
// A non-nil error therefore means "the env holds no request at all", which is
// what lets openConfirmPopup answer it by simply not opening the popup.
func writeConfirmRequest(tc confirmEnvWriter, req confirmRequest) error {
	if err := clearConfirmEnv(tc, confirmEnvKeys); err != nil {
		return err
	}
	for _, kv := range []struct{ key, value string }{
		{EnvConfirmMode, req.mode},
		{EnvConfirmTargetID, req.targetID},
		{EnvConfirmTargetDesc, req.targetDesc},
	} {
		if err := tc.SetEnvironment(tmux.SessionName, kv.key, kv.value); err != nil {
			_ = clearConfirmEnv(tc, confirmEnvKeys)
			return fmt.Errorf("set %s: %w", kv.key, err)
		}
	}
	return nil
}

// clearConfirmEnv unsets the given handshake keys, which includes a dismissed
// prompt's leftovers. Every key is attempted even after a failure — each one
// still set is a fragment of a destructive request that a later tick, or the
// next TUI process, can pair with something else — and the failures are joined
// so the caller can refuse to build on a half-cleared env.
func clearConfirmEnv(tc confirmEnvWriter, keys []string) error {
	var errs []error
	for _, key := range keys {
		if err := tc.UnsetEnvironment(tmux.SessionName, key); err != nil {
			errs = append(errs, fmt.Errorf("unset %s: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// staleConfirmKeys returns the handshake keys present in env, a snapshot of the
// outer tmux env. The startup clear narrows to these rather than wiping all four
// unconditionally: unsetting a key that was never set changes nothing, so the
// guarantee holds while the usual case costs no tmux calls before the first frame.
func staleConfirmKeys(env map[string]string) []string {
	var stale []string
	for _, key := range confirmEnvKeys {
		if _, ok := env[key]; ok {
			stale = append(stale, key)
		}
	}
	return stale
}

// confirmAnswer is one completed confirm-popup round trip: the prompt this
// Model wrote before opening the popup, plus what the user chose.
type confirmAnswer struct {
	mode     string
	targetID string
	result   string
}

// envRequests is what one envTick found waiting in the outer tmux env. An
// answer is present exactly when its result is non-empty: a dismissed popup
// writes no result, and the empty result is the "do nothing" case everywhere
// else too.
type envRequests struct {
	focusSessionID string
	answer         confirmAnswer
}

// consumeEnvRequests drains the popup→parent env handshake for one tick.
// consume returns a key's value and unsets it on tmux, so anything read here is
// gone whether or not the caller acts on it.
//
// The confirm answer is read on this pass, alongside the focus IDs, rather than
// further into the tick: the caller's focus handling can return early from
// Update, and an answer left in the tmux env outlives this TUI process — the
// next one would find a destructive request it has no context for.
func consumeEnvRequests(consume func(key string) string) envRequests {
	var req envRequests
	// Any popup that wants the parent TUI to focus a session pushes the ID here.
	// JIN_CREATED_SESSION, JIN_NOTIFY_SESSION and JIN_FOCUS_SESSION all share the
	// same downstream (switchToSession) via focusSessionID.
	for _, key := range []string{"JIN_CREATED_SESSION", "JIN_NOTIFY_SESSION", "JIN_FOCUS_SESSION"} {
		if id := consume(key); id != "" {
			req.focusSessionID = id
		}
	}
	// A dismissed popup (Ctrl+C) writes no result, so the prompt keys are
	// left alone here — nothing to act on, and clearing them is the job of
	// the next writeConfirmRequest or of the startup clear.
	if result := consume(EnvConfirmResult); result != "" {
		mode := consume(EnvConfirmMode)
		targetID := consume(EnvConfirmTargetID)
		_ = consume(EnvConfirmTargetDesc)
		req.answer = confirmAnswer{mode: mode, targetID: targetID, result: result}
	}
	return req
}

// handleEnvTick is the whole body of one envTick: it drains the popup→parent
// handshake out of env and acts on whatever was waiting. env is the snapshot the
// caller read from tmux and unset removes a key from the tmux server, so a test
// can drive every branch below — including the destructive one — against a
// synthetic env instead of a live tmux client. Without that seam the entire
// feature could be deleted from the Update loop with the suite still green.
func (m Model) handleEnvTick(env map[string]string, unset func(key string)) (tea.Model, tea.Cmd) {
	// consume reads a JIN_* key and, if set, unsets it on tmux so the same
	// value isn't picked up again on the next tick.
	consume := func(key string) string {
		v := env[key]
		if v != "" {
			unset(key)
		}
		return v
	}

	req := consumeEnvRequests(consume)
	if req.focusSessionID != "" {
		m.focusSessionID = req.focusSessionID
	}
	// The approved destructive action runs ahead of the focus fast path
	// below, which can return early from this tick.
	if req.answer.result != "" {
		next, cmd := m.dispatchConfirmResult(req.answer.mode, req.answer.targetID, req.answer.result)
		if nm, ok := next.(Model); ok {
			m = nm
		}
		if cmd != nil {
			return m, tea.Batch(envTickCmd(), cmd)
		}
	}
	// Fast path: resolve now, or kick a fetch so the sessionsMsg slow path
	// resolves on the next round-trip instead of after the next sessionTick
	// (~2s). JIN_CREATED_WARNING / JIN_ACTION_ID stay in tmux env and surface
	// on the next envTick.
	if !m.resolveFocusSession() {
		return m, tea.Batch(envTickCmd(), m.fetchSessions)
	}
	// Non-fatal warning from the create popup (e.g. hook not allowlisted).
	// Read alongside JIN_CREATED_SESSION so it surfaces on the same tick.
	if w := consume("JIN_CREATED_WARNING"); w != "" {
		m.warning = w
	}
	// Poll for an action ID pushed by the action-popup, then route through
	// dispatchAction so palette and direct-key paths share the same helpers.
	// If the helper returns a Cmd (e.g. Refresh), merge it into the tick's
	// Batch so tea sees a single frame.
	if id := consume("JIN_ACTION_ID"); id != "" {
		next, cmd := m.dispatchAction(id)
		if nm, ok := next.(Model); ok {
			m = nm
		}
		if cmd != nil {
			return m, tea.Batch(envTickCmd(), cmd)
		}
	}
	return m, envTickCmd()
}

// confirmRequestForAction resolves the confirmation a destructive action would
// raise against the cursor session — including the worktree variant of delete,
// which asks an extra question. The bool is false when the action is not one
// that confirms, or there is no actionable target under the cursor.
//
// Split out so the action→dialog mapping stays unit-testable: once the prompt
// moved into a popup, the handler's only remaining effect is a tmux env write.
// Keying it by action ID also means the palette and the direct keys resolve
// through one table rather than two that could drift.
func (m Model) confirmRequestForAction(actionID string) (confirmRequest, bool) {
	var mode string
	switch actionID {
	case action.IDKill:
		mode = ConfirmModeKill
	case action.IDDelete:
		mode = ConfirmModeDelete
	default:
		return confirmRequest{}, false
	}
	sess, ok := m.cursorSession()
	if !ok {
		return confirmRequest{}, false
	}
	if mode == ConfirmModeDelete && sess.IsWorktree {
		mode = ConfirmModeDeleteWorktree
	}
	return confirmRequest{mode: mode, targetID: sess.ID, targetDesc: sess.Description}, true
}

// handleDestructiveAction opens the confirmation popup for a kill / delete
// action on the cursor session. Shared by the action palette and the direct
// keys; a no-op when there is nothing to confirm.
func (m Model) handleDestructiveAction(actionID string) (tea.Model, tea.Cmd) {
	if req, ok := m.confirmRequestForAction(actionID); ok {
		m.openConfirmPopup(req)
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
		if m.isDeleting(sess) {
			return m, nil
		}
		go m.openVSCode(&sess)
	}
	return m, nil
}

// handleMarkSeen acknowledges the cursor session's completion receipt, so the
// unseen dot can be cleared without leaving the TUI. Failures surface on
// m.err.
//
// It refetches rather than waiting for the next poll: the dot and the
// unseen-first partition are both derived from the list, so up to two seconds
// would pass with the row unchanged — which reads as the action not having
// worked.
func (m Model) handleMarkSeen() (tea.Model, tea.Cmd) {
	if m.client == nil {
		return m, nil
	}
	sess, ok := m.cursorSession()
	if !ok {
		return m, nil
	}
	if _, err := m.client.MarkSeen(sess.ID); err != nil {
		m.err = fmt.Errorf("mark seen %s: %w", sess.Description, err)
		return m, nil
	}
	return m, m.fetchSessions
}

// handleSessionFilter opens the switch-session popup — the same popup
// bound at the outer-tmux root key table via keybindings.search. Wired
// here so the action palette can launch it without depending on the
// tmux root binding being set (or on the user's default key).
func (m Model) handleSessionFilter() (tea.Model, tea.Cmd) {
	m.openPopup(config.PopupSessionFilter, " Switch Session ")
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
	case action.IDKill, action.IDDelete:
		return m.handleDestructiveAction(id)
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
	case action.IDMarkSeen:
		return m.handleMarkSeen()
	}
	// Plugin palette IDs are three-segment ("plugin:<name>:<action>");
	// anything else — a core ID that missed the switch above, or a stale
	// two-segment ID left in the tmux env by an older binary — falls through
	// to the no-op return.
	if name, actionID, ok := action.ParsePluginActionID(id); ok {
		return m.handlePluginRun(name, actionID)
	}
	return m, nil
}

// handlePluginRun issues a plugin-run request to the daemon for the given
// plugin name and action ID, targeting the current cursor session (empty
// session ID => global action). An empty actionID lets the daemon select
// the plugin's default action. Failures surface on m.err.
func (m Model) handlePluginRun(name, actionID string) (tea.Model, tea.Cmd) {
	if m.client == nil {
		return m, nil
	}
	req := daemon.PluginRunRequest{
		Plugin:           name,
		Action:           actionID,
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

// dispatchConfirmResult carries out the destructive action the user approved in
// the confirm popup. mode and targetID come back from the tmux env this Model
// wrote before opening the popup; result is what the popup wrote there.
//
// Every branch below destroys something, and the inputs are shared tmux env that
// can go stale. So the routing is exhaustive by construction: only the listed
// mode/result pairs act, and everything else — any "no", an unrecognized mode or
// result, a mismatched pair, an empty target — falls through to the no-op return.
func (m Model) dispatchConfirmResult(mode, targetID, result string) (tea.Model, tea.Cmd) {
	if targetID == "" {
		return m, nil
	}
	switch {
	case mode == ConfirmModeKill && result == ConfirmResultYes:
		return m.killSession(targetID)

	// Session-only delete, reached from three prompts: plain delete, the
	// worktree prompt's "keep the worktree" answer, and declining the force
	// prompt (which falls back to leaving the dirty worktree in place).
	case mode == ConfirmModeDelete && result == ConfirmResultYes,
		mode == ConfirmModeDeleteWorktree && result == ConfirmResultYes,
		mode == ConfirmModeDeleteWorktreeForce && result == ConfirmResultForceNo:
		return m.deleteSession(targetID, false, false)

	case mode == ConfirmModeDeleteWorktree && result == ConfirmResultWorktree:
		return m.deleteSession(targetID, true, false)

	case mode == ConfirmModeDeleteWorktreeForce && result == ConfirmResultForceYes:
		return m.deleteSession(targetID, true, true)
	}
	return m, nil
}

// killSession issues the daemon Kill for targetID. pendingKillID makes the
// first sessionsMsg that shows the kill landed reconnect the display pane to
// whatever the cursor lands on.
func (m Model) killSession(targetID string) (tea.Model, tea.Cmd) {
	m.processingMsg = "Stopping..."
	m.pendingKillID = targetID
	client := m.client
	return m, func() tea.Msg {
		if err := client.Kill(targetID); err != nil {
			return errMsg(fmt.Errorf("kill failed: %w", err))
		}
		sessions, err := client.List()
		if err != nil {
			return errMsg(err)
		}
		return sessionsMsg(sessions)
	}
}

// deleteSession greys out targetID, slides the cursor off it, and issues the
// daemon Delete. The removeWorktree/force pair matches daemon.Client.Delete.
//
// Both worktree sentinels are handled here for every flag combination even
// though only one can raise them. The dirty case is the one call that can come
// back asking another question — worktreeDirtyMsg re-opens the popup for a force
// decision — and it carries the description resolved before the Cmd runs, so the
// follow-up prompt and the error text name the session the user saw.
func (m Model) deleteSession(targetID string, removeWorktree, force bool) (tea.Model, tea.Cmd) {
	name := m.sessionDescription(targetID)
	m.deletingIDs[targetID] = true
	m.skipDeletingSessions(1)
	// Move the display pane off the target before the daemon touches it: the
	// delete finalizes on a background goroutine (see docs/gotchas.md), so an
	// attach left in place keeps the doomed session on screen for the whole
	// removal and then shows the dead frame it leaves behind.
	if m.currentSessionID == targetID {
		m.showCursorSession(false)
	}
	client := m.client
	return m, func() tea.Msg {
		if err := client.Delete(targetID, removeWorktree, force); err != nil {
			if errors.Is(err, session.ErrWorktreeDirty) {
				return worktreeDirtyMsg{sessionID: targetID, name: name}
			}
			if errors.Is(err, session.ErrNotWorktree) {
				return deleteErrMsg{
					sessionID: targetID,
					err:       fmt.Errorf("worktree not found for session %q (already removed, or session is not in a worktree)", name),
				}
			}
			return deleteErrMsg{sessionID: targetID, err: fmt.Errorf("delete failed: %w", err)}
		}
		sessions, err := client.List()
		if err != nil {
			return errMsg(err)
		}
		return sessionsMsg(sessions)
	}
}

// sessionDescription returns the session's current description, falling back to
// the ID when the list no longer holds it (deleted between the prompt and the
// answer).
func (m Model) sessionDescription(targetID string) string {
	if i := slices.IndexFunc(m.sessions, func(s session.Info) bool { return s.ID == targetID }); i >= 0 {
		return m.sessions[i].Description
	}
	return targetID
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

	// Ignore user input while processing, only handle completion messages
	if m.processingMsg != "" {
		switch msg.(type) {
		case tea.KeyMsg, tea.MouseMsg:
			return m, nil
		}
	}

	return m.updateListMode(msg)
}

func (m Model) updateListMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		// Dismiss any transient warning on the first key press.
		m.warning = ""

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
			return m.handleDestructiveAction(action.IDKill)

		case key.Matches(msg, m.keys.Delete):
			return m.handleDestructiveAction(action.IDDelete)

		case key.Matches(msg, m.keys.Refresh):
			return m.handleRefresh()

		case key.Matches(msg, m.keys.Help):
			return m.handleHelp()

		case key.Matches(msg, m.keys.PrevPage):
			m.scrollBy(-m.pageScrollLines())
			m.writeCursorEnv()
			return m, nil

		case key.Matches(msg, m.keys.NextPage):
			m.scrollBy(m.pageScrollLines())
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
		// The unseen-first partition reorders the list under the cursor, and it
		// does so on a two-second poll nobody asked for. Follow the session the
		// cursor was on by ID rather than leaving it on an index that now names
		// a different row.
		cursorID := ""
		if prev := m.getDisplaySessions(); m.cursor >= 0 && m.cursor < len(prev) {
			cursorID = prev[m.cursor].ID
		}
		m.sessions = partitionUnseenFirst(msg)
		m.err = nil

		// Check whether any deleting session has resolved: either the record
		// disappeared (successful async finalize) or it flipped back to Stopped
		// with ErrorMessage set (MarkDeletionFailed rolled it back). Both clear the
		// grey-out; only the record-gone case triggers a reswitch.
		deleteCompleted := false
		if len(m.deletingIDs) > 0 {
			current := make(map[string]session.Info, len(m.sessions))
			for _, s := range m.sessions {
				current[s.ID] = s
			}
			for id := range m.deletingIDs {
				live, stillExists := current[id]
				if !stillExists {
					delete(m.deletingIDs, id)
					deleteCompleted = true
					continue
				}
				// Async finalize failed: MarkDeletionFailed rolled Status
				// back to Stopped and populated ErrorMessage. Drop the
				// grey-out so the user can see the error and retry.
				if live.Status == session.StatusStopped && live.ErrorMessage != "" {
					delete(m.deletingIDs, id)
				}
			}
		}

		// Align cursor to the session restored from JIN_CURRENT_SESSION so a
		// relaunched TUI selects whatever the right pane is showing. Runs once per
		// startup; if the target no longer exists between runs, IndexFunc returns
		// -1 and the cursor keeps its default.
		// An ID that no longer resolves falls through to the clamp below,
		// which is the pre-existing behaviour for a session that disappeared
		// between polls.
		wantID := cursorID
		if m.pendingCursorRestore {
			m.pendingCursorRestore = false
			wantID = m.currentSessionID
		}
		if wantID != "" {
			if i := slices.IndexFunc(m.getDisplaySessions(), func(s session.Info) bool {
				return s.ID == wantID
			}); i >= 0 {
				m.cursor = i
			}
		}

		// Clamp before the focus fast path below, not after: that path returns
		// early, and a frame where sessions disappeared and a pending focus missed
		// would otherwise reach the renderer with a cursor past the end of the list
		// — which indexes the slice to pick the detail pane's subject.
		displaySessions := m.getDisplaySessions()
		if m.cursor >= len(displaySessions) {
			m.cursor = max(len(displaySessions)-1, 0)
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
		// Session list changed: clamp scroll so we cannot land past the last
		// card, and ensure the cursor's card stays in view.
		m.adjustScrollForCursor()
		// A kill has landed once the list says so, not when the next snapshot
		// happens to arrive: the session poll runs on its own clock and can answer
		// "still running" from a read that predates the Kill — see docs/gotchas.md.
		//
		// Resolved here rather than beside the deletingIDs bookkeeping above so the
		// focus fast path cannot return between the arm being cleared and the
		// re-point it was cleared for. One slot is enough for overlapping kills: the
		// re-point target is the cursor rather than the killed session, and the
		// force below keys off the pane instead of the arm.
		killCompleted, displayedIsDead := false, false
		if m.pendingKillID != "" {
			// "Not listed as alive" is both landings at once: the record
			// stopped, or it left the list entirely.
			stillAlive := slices.ContainsFunc(m.sessions, func(s session.Info) bool {
				return s.ID == m.pendingKillID && isSessionAlive(s.Status)
			})
			if !stillAlive {
				m.pendingKillID = ""
				killCompleted = true
				displayedIsDead = slices.ContainsFunc(m.sessions, func(s session.Info) bool {
					return s.ID == m.currentSessionID && !isSessionAlive(s.Status)
				})
			}
		}

		// Re-point the display pane whenever it is not showing a session that is
		// still in the list: the list went empty, the displayed session disappeared
		// between polls, nothing has been displayed yet, or the placeholder is up
		// and a session became attachable again.
		//
		// A finished delete/kill re-points unconditionally, since the slot the pane
		// was on has changed under it. Force is for the one arrangement that still
		// looks settled: the pane holding an attach to a session that is listed but
		// no longer alive. Testing the pane rather than "was this the newest kill's
		// target?" is deliberate — the latter strands a pane parked on an earlier
		// kill. A delete must NOT have force: deleteSession already moved the pane
		// at request time, and forcing here would tear the same attach down and
		// rebuild it as a visible flash.
		if killCompleted || deleteCompleted || !m.displaysLiveSession() {
			m.showCursorSession(displayedIsDead)
		}
		// Re-push the pane label for the displayed session so it picks up
		// Layer C description upgrades without a manual switch. Idempotent:
		// only pushes when the Description changed since the last poll.
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
		// The daemon refused the worktree removal because it is dirty. The
		// popup that asked the first question is already closed, so ask the
		// force question in a fresh one.
		m.openConfirmPopup(confirmRequest{
			mode:       ConfirmModeDeleteWorktreeForce,
			targetID:   msg.sessionID,
			targetDesc: msg.name,
		})

	case deleteErrMsg:
		delete(m.deletingIDs, msg.sessionID)
		m.err = msg.err

	case errMsg:
		m.processingMsg = ""
		m.err = msg

	case envTickMsg:
		// The nil guard stays here: handleEnvTick resolves a pending focus
		// switch, which drives the outer tmux client.
		if m.tmuxClient == nil {
			return m, envTickCmd()
		}
		return m.handleEnvTick(
			m.tmuxClient.ListEnvironment(tmux.SessionName),
			func(key string) { _ = m.tmuxClient.UnsetEnvironment(tmux.SessionName, key) },
		)

	case sessionTickMsg:
		cmds := []tea.Cmd{m.fetchSessions, sessionTickCmd()}
		if c := m.pollAttachedSessionCmd(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case attachedSessionMsg:
		m.adoptAttachedSession(string(msg))
		return m, nil
	}

	return m, nil
}

// View renders the UI
func (m Model) View() string {
	// Processing indicator
	if m.processingMsg != "" {
		return m.renderProcessingView()
	}

	paneWidth := m.width
	paneWidth = max(paneWidth, 20)
	paneHeight := m.height - helpChromeLines
	paneHeight = max(paneHeight, 5)
	// Content sits inside 1-column horizontal padding on each side.
	contentWidth := paneWidth - 2
	contentWidth = max(contentWidth, 16)
	paneStyle := createPaneStyle(paneWidth, paneHeight, m.focused)
	pane := paneStyle.Render(m.renderListContent(contentWidth))
	// The rule below the pane is drawn outside paneStyle, so it carries
	// detailIndent itself to land in the column the pane's padding puts content
	// in — the same column as the detail pane's own rule and as " ? help".
	// Without it the help block would read as a wider band than the pane above.
	return pane + "\n" +
		detailIndent + paneRule(contentWidth) + "\n" +
		helpStyle.Render(" ? help")
}

// renderProcessingView renders a processing indicator.
// Size-independent: renders correctly even before WindowSizeMsg arrives after ZoomPane/JoinPane
func (m Model) renderProcessingView() string {
	return "\n  ⟳ " + m.processingMsg
}

// skipDeletingSessions adjusts cursor to skip over sessions being deleted.
// dir: -1 for up, +1 for down.
func (m *Model) skipDeletingSessions(dir int) {
	pageSessions := m.getDisplaySessions()
	deleting := func(i int) bool {
		return i >= 0 && i < len(pageSessions) && m.isDeleting(pageSessions[i])
	}
	for deleting(m.cursor) {
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
	if deleting(m.cursor) {
		for i := m.cursor - dir; i >= 0 && i < len(pageSessions); i -= dir {
			if !deleting(i) {
				m.cursor = i
				return
			}
		}
	}
}

// renderListContent renders the session list content.
//
// Layout — the three regions headerLines / listAreaLines / detailLines budget:
//
//	[err / warn ...]        <- noticeLines()
//	[header + blank]        <- headerLines(), fixed: it does not scroll
//	[session rows]          <- listAreaLines(), windowed by m.scrollOffset
//	[detail pane]           <- detailLines(), the session under the cursor
//
// There is no separate title: renderListHeader's count line is it. The tmux pane
// border above deliberately carries no label (see tuiPaneBorderLabel in
// cmd/jin/cmd/tui.go).
func (m Model) renderListContent(contentWidth int) string {
	var content strings.Builder

	// Both notices go through one emitter so they cannot drift apart. It
	// truncates to the content width and always writes exactly two rows, which is
	// what noticeLines() promises and what the header offset, the list window and
	// the mouse hit-test origin all count from.
	writeNotice := func(color lipgloss.Color, text string) {
		content.WriteString(lipgloss.NewStyle().Foreground(color).Render(truncateString(text, contentWidth)))
		content.WriteString("\n\n")
	}
	if m.err != nil {
		writeNotice(errorColor, fmt.Sprintf("Error: %v", m.err))
	}
	if m.warning != "" {
		writeNotice(warningColor, "⚠ "+m.warning)
	}

	displaySessions := m.getDisplaySessions()
	if len(displaySessions) == 0 {
		// The empty state replaces the whole list, header and detail pane
		// included — there is no count to report and no session to describe.
		// headerLines() / detailLines() return 0 for the same reason.
		content.WriteString("\n")
		content.WriteString(helpStyle.Render("No sessions. Press 'n' to create one."))
		content.WriteString("\n")
		return content.String()
	}

	// --- Header (fixed) ---
	content.WriteString(m.renderListHeader(displaySessions, contentWidth))
	content.WriteString("\n\n") // count line + blank spacer = listHeaderLines

	// --- Scrollable list area ---
	// Build the full card content into a separate buffer so we can slice it
	// by lines and expose only the visible window (no per-page arithmetic).
	var cards strings.Builder
	idToIdx := make(map[string]int, len(displaySessions))
	for i, sess := range displaySessions {
		idToIdx[sess.ID] = i
	}
	// One header row per group, matching what sessionCardTop and totalCardLines
	// count.
	for _, group := range groupSessionsByFleet(displaySessions) {
		cards.WriteString(renderFleetHeader(group.Name, contentWidth))
		for _, sess := range group.Sessions {
			idx := idToIdx[sess.ID]
			viewed := sess.ID == m.currentSessionID
			cards.WriteString(m.renderSession(sess, idx == m.cursor, viewed, contentWidth))
		}
	}

	// Slice by rows and take a window starting at scrollOffset. The window height
	// is computed the same way adjustScrollForCursor does, so the two stay in
	// agreement. The trailing newline is dropped before the split, or the phantom
	// empty element would count as a row against the window budget.
	rows := strings.Split(strings.TrimSuffix(cards.String(), "\n"), "\n")
	avail := m.listAreaLines()
	start := m.scrollOffset
	if start < 0 {
		start = 0
	}
	if start > len(rows) {
		start = len(rows)
	}
	end := start + avail
	if end > len(rows) {
		end = len(rows)
	}
	window := rows[start:end]
	content.WriteString(strings.Join(window, "\n"))

	// --- Detail pane (fixed, bottom) ---
	if m.detailVisible() {
		// Pad the window out to exactly avail rows first. View() renders this
		// string inside a fixed-height pane style, which pads at the bottom, so
		// without this a short list would leave the detail pane floating directly
		// under the last session row instead of on the pane's bottom edge.
		content.WriteString(strings.Repeat("\n", avail-len(window)+1))
		content.WriteString(m.renderDetailPane(displaySessions[m.cursor], contentWidth))
	}
	return content.String()
}

// effectiveStatus returns the status to display for a session on this frame.
//
// m.deletingIDs is the TUI's own optimistic mark, set the moment a delete is
// accepted and cleared when the record goes away — the daemon does not report
// StatusDeleting until its next poll. Three places draw a session's status on
// the same frame, so all three ask here rather than each deciding for itself;
// otherwise the header counts a session as IDLE while its row shows "⟳".
func (m Model) effectiveStatus(sess session.Info) session.Status {
	if m.deletingIDs[sess.ID] {
		return session.StatusDeleting
	}
	return sess.Status
}

// isDeleting reports whether a session is on its way out, from either source:
// this TUI's own optimistic mark or the daemon's reported status.
//
// Both halves matter. m.deletingIDs alone misses a session another client
// deleted, and one whose delete was still running when this TUI last started —
// deletingIDs lives only in memory, so a restart forgets it. Anything reading
// only the optimistic mark would let a keypress attach to, start, or open an
// editor on a session that is being removed.
func (m Model) isDeleting(sess session.Info) bool {
	return m.effectiveStatus(sess) == session.StatusDeleting
}

// statusCount pairs a status with how many sessions currently carry it.
type statusCount struct {
	Status session.Status
	N      int
}

// statusCounts returns the non-zero per-status counts in urgency order, most
// urgent first. A status no session carries is omitted entirely, and every
// status the display vocabulary does not know collapses into one trailing
// bucket (getStatusDisplay renders it as UNKNOWN).
//
// PERMISSION leads because "stuck until a human acts" is not the same wait as
// IDLE's "finished": folding them into one number hides the one that needs
// attention, which is the whole reason this breakdown exists — and why
// session.Manager.CountActive is deliberately not reused, since it lumps
// permission in with running/thinking.
//
// Statuses come from effectiveStatus, so the breakdown agrees with the rows
// beneath it while an optimistic delete is in flight.
func (m Model) statusCounts(sessions []session.Info) []statusCount {
	order := []session.Status{
		session.StatusPermission,
		session.StatusThinking,
		session.StatusRunning,
		session.StatusCreating,
		session.StatusIdle,
		session.StatusStopped,
		session.StatusDeleting,
	}
	counts := make(map[session.Status]int, len(order))
	unknown := 0
	for _, sess := range sessions {
		status := m.effectiveStatus(sess)
		if !slices.Contains(order, status) {
			unknown++
			continue
		}
		counts[status]++
	}

	result := make([]statusCount, 0, len(order)+1)
	for _, status := range order {
		if n := counts[status]; n > 0 {
			result = append(result, statusCount{Status: status, N: n})
		}
	}
	if unknown > 0 {
		// The bucket may cover several distinct unrecognised values, so it
		// carries the zero Status: the only thing defined for it is
		// getStatusDisplay's default.
		result = append(result, statusCount{N: unknown})
	}
	return result
}

// renderListHeader renders the fixed count line, e.g.
//
//	7 SESSIONS  /  ? 1   ⚡ 2   ○ 4
//
// The label is upper-case because this line is the pane's only title, and the
// total is separated from the breakdown so the two are read as "how many" and
// "of what" rather than as one run of numbers. The separator is dimColor, a step
// darker than the counts around it: it divides the line and must not compete
// with what it divides. Each group is coloured with its own status style, which
// is what puts the PERMISSION count in the warning colour without a special case.
//
// Groups are dropped from the right when the line does not fit — statusCounts
// orders them by urgency exactly so the least urgent fall off first. The total
// is never dropped: it is the one number that is always true. The separator is
// charged to the first group that fits, so a pane too narrow for any of them
// ends at the total instead of trailing a divider with nothing after it.
func (m Model) renderListHeader(sessions []session.Info, width int) string {
	total := fmt.Sprintf("%d SESSIONS", len(sessions))
	if len(sessions) == 1 {
		total = "1 SESSION"
	}
	line := helpStyle.Render(total)
	used := lipgloss.Width(line)

	const (
		groupGap = 3
		sepText  = "  /  "
	)
	sep := lipgloss.NewStyle().Foreground(dimColor).Render(sepText)

	for i, sc := range m.statusCounts(sessions) {
		icon, _, style := getStatusDisplay(sc.Status)
		group := style.Render(fmt.Sprintf("%s %d", icon, sc.N))
		// The separator introduces the breakdown, so only the leading group
		// pays for it; the rest are spaced by a plain gap. Index 0 is also the
		// first group that FITS — the loop breaks on one that does not rather
		// than skipping it.
		lead := strings.Repeat(" ", groupGap)
		if i == 0 {
			lead = sep
		}
		cost := lipgloss.Width(lead) + lipgloss.Width(group)
		if used+cost > width {
			break
		}
		line += lead + group
		used += cost
	}
	return line
}

// sessionRowLead is the fixed prefix width of a session row: cursor bar (2) +
// attention cell (2) + status icon cell (2) + separator (1). What is left of
// the row belongs to the name, so every name starts in the same column and the
// list reads as a table.
//
// The attention cell holds its two columns whether or not it has a dot to
// draw: a cell that collapsed when empty would shift every name on the row the
// moment a turn finished.
const sessionRowLead = 7

// sanitizeRowText takes out of a string everything that could break the
// fixed-height block it is about to be drawn in. Every piece of text this file
// draws that was authored somewhere else goes through here: a session's name
// (sessionNameParts), the last user / assistant messages (renderDetailPane), and
// the repo / branch pair (renderRepoBranch).
//
// Both classes below arrive verbatim from outside the TUI, and each defeats a
// different bound this file is built on:
//
//   - Escape sequences break the WRAP. ansi.Truncate re-emits the styles open at
//     its cut and closes them, so its result is NOT a prefix of its input;
//     wrapFixedLines advances by trimming off the prefix it just drew, so with
//     one of these in hand it makes no progress and draws the same text on every
//     row it was given. They are also a hole straight to the terminal: a
//     clear-screen sequence is one the TUI would faithfully forward, and an agent
//     whose subject is terminal output writes such sequences as a matter of course.
//   - C0 control characters break the ROW COUNT, and ansi.Strip leaves them
//     alone. A newline is the one that bites: it costs nothing in width, so every
//     truncation here happily keeps it, and then splits the "one line" it was
//     measured as into two — breaking sessionRowHeight in the list and
//     detailPaneLines in the pane, both of which the scroll and hit-test
//     arithmetic treats as fact. View() clips the overflow from the BOTTOM,
//     silently, so the symptom is a missing last row rather than an error.
//
// One function rather than a rule each caller applies for itself, because the
// two do not arrive together on both paths: transcript's TruncateMessage already
// folds a message's control characters into spaces, so only the escape half is
// live there, while nothing folds either of them for a name.
//
// Replaced with a space rather than dropped, so "a" and "b" split by a newline
// read as two words.
func sanitizeRowText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, ansi.Strip(s))
}

// sessionNameParts returns the text a session's name is drawn from and the
// marker that trails it — "*" when the name was set by hand and the agent may
// not overwrite it, "" otherwise. A name with nothing left in it gets no marker.
// The text is the Description put through sanitizeRowText; see there for what
// the sequences it takes out would otherwise do to the row.
func sessionNameParts(sess session.Info) (name, mark string) {
	name = sanitizeRowText(sess.Description)
	if sess.DescriptionLocked && name != "" {
		mark = "*"
	}
	return name, mark
}

// sessionNameText fits a session's name into avail display columns. The lock
// marker is taken out of the budget rather than appended past it: both callers
// draw fixed-height blocks, and one column of overflow wraps the line into a
// second physical row.
func sessionNameText(sess session.Info, avail int) string {
	name, mark := sessionNameParts(sess)
	return truncateString(name, avail-lipgloss.Width(mark)) + mark
}

// wrapFixedLines lays text across exactly `lines` rows of `avail` columns,
// returning one string per row — blank rows included. What is left over after
// the last row is cut with an ellipsis, exactly as a list row cuts it. `mark`
// trails the text and comes out of the budget rather than past it.
//
// The row count is the contract, not a maximum: every caller draws a
// fixed-height block whose height the list geometry is subtracted from, so the
// text may neither claim a row more nor give one back.
//
// Details worth stating:
//
//   - The break is by display column, not by word. The text is as often
//     Japanese — where there is no space to break on — as English, so a
//     word-aware wrap would help one language and still break mid-word in the
//     other. Whitespace at a break is dropped rather than opening the next row.
//   - It does not reuse wrapText, which accumulates width rune by rune. A VS16
//     emoji (base plus selector) measures as its base alone that way, so a
//     "wrapped" line can still come out wider than avail, wrap in the terminal,
//     and cost the block a row. truncateToWidth walks grapheme clusters with the
//     same ruler that finally measures the line.
//   - The loop advances by trimming off the prefix it just drew, which holds only
//     while the text carries no escape sequences — every caller pays that with
//     sanitizeRowText.
//   - The marker must fit: `mark` wider than `avail` comes back on a row of its
//     own, over budget, because the last-row branch appends it past a truncation
//     already given nothing to keep. Both callers are comfortably inside that
//     today, so this is a precondition rather than a case worth branching on.
func wrapFixedLines(text, mark string, avail, lines int) []string {
	out := make([]string, max(lines, 0))
	if avail < 1 {
		return out
	}

	markWidth := lipgloss.Width(mark)

	rest := text
	for i := range out {
		if rest == "" {
			break // the text ended earlier; the remaining rows stay blank
		}
		if i == len(out)-1 {
			// The last row there is: everything still in hand has to fit on it,
			// marker included, and what does not is cut with an ellipsis.
			out[i] = truncateString(rest, avail-markWidth) + mark
			break
		}
		head := truncateToWidth(rest, avail)
		out[i] = head
		rest = strings.TrimLeft(strings.TrimPrefix(rest, head), " ")
		if rest != "" {
			continue
		}
		// The text ends on this row. Its marker rides along when there is room;
		// when there is not, the marker moves down rather than costing an
		// ellipsis and three columns of a text that had just fitted — otherwise
		// a name one column LONGER would show more of itself, not less.
		if lipgloss.Width(head)+markWidth <= avail {
			out[i] += mark
		} else {
			out[i+1] = mark
		}
		break
	}
	return out
}

// sessionNameLines lays a session's name across exactly `lines` rows, marker
// included — wrapFixedLines over the parts sessionNameParts hands out.
func sessionNameLines(sess session.Info, avail, lines int) []string {
	name, mark := sessionNameParts(sess)
	return wrapFixedLines(name, mark, avail, lines)
}

// renderSession renders a single session as one list row of two lines:
//
//	[cursor bar 2][attention 2][status icon 2][sep 1][name                  ]
//	[cursor bar 2][   blank   ][    blank    ][sep 1][repo ....... branch   ]
//	└───────────────── sessionRowLead ───────────────┘
//
// It always returns exactly sessionRowHeight lines. That is what makes
// sessionRowHeight a constant: content can no longer change a row's height, so
// the scroll and hit-test arithmetic cannot desync from what is drawn. The
// status label and the message lines the multi-line card used to carry live in
// the detail pane instead, which shows them for the one session the cursor is on.
//
// The second line is the repo / branch pair, and it earns its row three times:
//
//   - Right alignment only reads as alignment when something anchors the left of
//     the same line. The branch alone would leave a gap that is empty rather
//     than a channel.
//   - The pair cannot share line one. Real session names run to 38 columns in
//     Japanese, and 38 + a 12-column branch + sessionRowLead needs more than the
//     left pane has.
//   - It gives the row a third level of hierarchy (coloured icon, white name,
//     grey metadata), which is what lets a screen of sessions be scanned rather
//     than read.
//
// Line two always uses helpStyle, selected and deleting rows included: the
// hierarchy above only holds if the metadata is grey on every row.
//
// Two orthogonal indicators — selected paints a blue cursor bar in the first
// column, viewed paints a subdued background to the end of the row. They compose
// freely, and the roles are visually separate (bar = pointer, background =
// current location) so a single glyph never has to be disambiguated.
func (m Model) renderSession(sess session.Info, selected bool, viewed bool, width int) string {
	// Deleting sessions are dim and not selectable, but keep the row shape:
	// the geometry must not depend on a session's state.
	status := m.effectiveStatus(sess)
	deleting := m.isDeleting(sess)
	statusIcon, _, statusStyle := getStatusDisplay(status)

	// withBg composes any inline style with the viewed row background when the
	// session is being displayed on the right. Applying it per styled segment
	// rather than wrapping the whole line sidesteps ANSI reset artifacts between
	// segments — every visible cell carries the bg SGR codes.
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

	// Narrower than the fixed lead plus a single column of name: the lead alone
	// would overflow, and a row that overflows wraps into more physical rows,
	// which is the one thing sessionRowHeight may never allow. Emit blank rows of
	// exactly `width` columns instead — still sessionRowHeight of them, since
	// returning fewer breaks the same invariant from the other side.
	if width < sessionRowLead+1 {
		return strings.Repeat(padBg(width)+"\n", sessionRowHeight)
	}

	var cursorBar string
	if selected {
		cursorBar = withBg(selectedItemStyle).Render("▎ ")
	} else {
		cursorBar = padBg(2)
	}

	// Deleting wins over selected for the name: the cursor slides past
	// deleting rows, so the pairing only appears mid-transition, and dimming
	// is the signal that the row is on its way out.
	nameStyle := sessionNameStyle
	switch {
	case deleting:
		nameStyle = deletingStyle
	case selected:
		nameStyle = selectedItemStyle
	}

	// No floor on the name budget: raising it to a readable minimum would emit a
	// row wider than the pane, trading a cramped name for a wrapped one. The
	// guard above guarantees at least one column here.
	avail := width - sessionRowLead
	nameStyled := withBg(nameStyle).Render(sessionNameText(sess, avail))
	// A narrower budget than the detail pane gives this pair (which spends only
	// detailIndentWidth), so a branch that fits there may be cut here —
	// renderRepoBranch keeps its tail, which is the identifying half.
	metaStyled := renderRepoBranch(sess, avail, withBg(helpStyle))

	var b strings.Builder
	b.WriteString(cursorBar)
	// Its own column rather than a second meaning loaded onto the status icon:
	// a session can be running with an unacknowledged completion from before.
	if sess.Attention.Unseen {
		b.WriteString(withBg(attentionStyle).Render(padIcon(attentionGlyph)))
	} else {
		b.WriteString(padBg(2))
	}
	b.WriteString(withBg(statusStyle).Render(padIcon(statusIcon)))
	b.WriteString(padBg(1))
	b.WriteString(nameStyled)
	b.WriteString(padBg(avail - lipgloss.Width(nameStyled)))
	b.WriteString("\n")
	// The cursor bar repeats so that the two lines read as one row; the icon
	// cell and its separator do not, because the status is stated once.
	b.WriteString(cursorBar)
	b.WriteString(padBg(sessionRowLead - 2))
	b.WriteString(metaStyled)
	b.WriteString(padBg(avail - lipgloss.Width(metaStyled)))
	b.WriteString("\n")
	return b.String()
}

// detailIndent is the indent every detail line carries except the rule, which
// spans the full width so it reads as a divider between the list and the
// detail pane. detailIndentWidth is its display width, spelled out as a
// constant because every budget below the rule is computed as width minus this.
const (
	detailIndent      = " "
	detailIndentWidth = 1
	// msgIconWidth is what "👤 " and "🤖 " occupy: a 2-column emoji plus its
	// space. It sits out here with the other widths because it is load-bearing
	// twice over — subtracted from the message budget, and the hang the
	// continuation rows are indented by — and the two may not disagree.
	msgIconWidth = 3
)

// paneRule renders the divider the detail pane opens with and the one View()
// puts above the help line. One function so that "the two rules look the same"
// is a fact rather than a convention: they are drawn a screen apart and one row
// of drift in colour or glyph would read as two different kinds of boundary.
func paneRule(width int) string {
	return lipgloss.NewStyle().Foreground(dimColor).Render(strings.Repeat("─", max(width, 0)))
}

// renderDetailPane renders the fixed-height detail block for one session:
//
//	──────────────────────────  rule
//	 plugin registry の crawler  name, row 1 of detailNameLines
//	 を実装して                  name, row 2 (blank when unused)
//	 ⚡ THINKING        claude   status + agent kind
//	 👤 次の task を進めて。手   last user message, row 1 of detailMsgLines
//	    が空いたら registry も   row 2 (blank when unused)
//	 🤖 …name 衝突のルールを整   last assistant message, rows 1-2
//	    理して crawler に渡した  (kept from its END, see below)
//
// It always returns exactly detailPaneLines lines. An empty field still emits
// its (blank) line, because every scroll and hit-test calculation is built on
// that height — it must not depend on what the session happens to carry.
//
// The caller feeds it the session under m.cursor, NOT the one in
// m.currentSessionID: the cursor and the tmux pane on the right are
// deliberately orthogonal (moving the cursor never switches panes), and letting
// a user read a session's contents without switching to it is the entire point
// of this pane. That is also why the name is on line 2 — with two "current"
// sessions on screen, the block has to say which one it describes.
func (m Model) renderDetailPane(sess session.Info, width int) string {
	lines := make([]string, 0, detailPaneLines)

	// Every content line hangs inside the indent, so that is what the budget is
	// measured against. Below one usable column nothing but the rule fits —
	// return the full height anyway, since the geometry depends on the line
	// count and must never depend on what the session carries.
	avail := width - detailIndentWidth
	rule := paneRule(width)
	if avail < 1 {
		blank := make([]string, detailPaneLines)
		blank[0] = rule
		return strings.Join(blank, "\n")
	}

	// The pane and the session's list row are drawn from the same frame, so
	// they resolve "is this being deleted" the same way. Anything else lets
	// the two blocks describing one session disagree in front of the user.
	status := m.effectiveStatus(sess)
	deleting := m.isDeleting(sess)

	// --- Line 1: rule ---
	lines = append(lines, rule)

	// --- Lines 2-3: session name, one row per detailNameLines ---
	// Dimmed while deleting, exactly as the list row dims it. Every reserved row
	// is emitted, blank ones included — the list geometry subtracts this block's
	// height, so it may not follow the name it happens to be showing.
	nameStyle := sessionNameStyle
	if deleting {
		nameStyle = deletingStyle
	}
	for _, nameLine := range sessionNameLines(sess, avail, detailNameLines) {
		lines = append(lines, detailIndent+nameStyle.Render(nameLine))
	}

	// --- Line 4: status label, agent kind right-aligned ---
	// padIcon fixes the icon cell at 2 columns; the separating space is
	// explicit, exactly as in a list row.
	icon, label, statusStyle := getStatusDisplay(status)
	// Truncated like every other line. The longest label ("PERMISSION") plus
	// the icon cell needs 13 columns and the narrowest pane offers 27, so this
	// never fires today — but that headroom lives in View()'s width clamp, far
	// from here, and a line that outgrows it would wrap and cost the pane a row.
	statusCluster := statusStyle.Render(truncateString(padIcon(icon)+" "+label, avail))
	statusLine := detailIndent + statusCluster
	if sess.AgentKind != "" {
		// Below two columns of gap the kind reads as part of the label, so it
		// is dropped whole — the label is never shortened to make it fit.
		gap := avail - lipgloss.Width(statusCluster) - lipgloss.Width(sess.AgentKind)
		if gap >= 2 {
			statusLine += strings.Repeat(" ", gap) + helpStyle.Render(sess.AgentKind)
		}
	}
	lines = append(lines, statusLine)

	// --- Lines 5-8: the last message from each side, detailMsgLines rows each ---
	// Below four columns there is room for the icon and nothing after it, which
	// says less than a blank line does, so the whole message goes. Deliberately no
	// floor on the budget: clamping to 1 would emit a line wider than the pane.
	msgAvail := avail - msgIconWidth

	// msgRows lays one message across exactly detailMsgLines rows: the icon leads
	// the first row, continuation rows hang under the text so the message reads as
	// one block instead of two entries. A row the message never reaches stays
	// blank rather than carrying a lone icon, which is noisier than the blank.
	msgRows := func(icon, text string, fromEnd bool) []string {
		out := make([]string, detailMsgLines)
		// A message is agent-authored text arriving here verbatim, and the second
		// row it now gets is what would make an escape sequence in it visible as a
		// wrap that never advances. Where this sits relative to the emptiness test
		// below does not change what is drawn — the `row == ""` continue further
		// down is the only guard on the lone icon, and covers both cases.
		text = sanitizeRowText(text)
		if msgAvail < 1 || text == "" {
			return out
		}
		if fromEnd {
			// Cut to what the rows can hold BEFORE wrapping, so the ellipsis
			// lands at the head, where truncateStringFromEnd puts it, and the
			// tail survives to the last row.
			//
			// The -(detailMsgLines-1) is not slack. A wrap may only break in
			// front of a grapheme cluster, so a 2-column cluster straddling the
			// edge leaves its row one column unspent — with an odd msgAvail,
			// full-width text does that on every row. Those columns push past
			// the last row, where wrapFixedLines cuts exactly the tail we went
			// out of our way to keep.
			text = truncateStringFromEnd(text, msgAvail*detailMsgLines-(detailMsgLines-1))
		}
		for i, row := range wrapFixedLines(text, "", msgAvail, detailMsgLines) {
			if row == "" {
				continue
			}
			lead := icon
			if i > 0 {
				lead = strings.Repeat(" ", msgIconWidth)
			}
			out[i] = lead + row
		}
		return out
	}

	for _, row := range msgRows("👤 ", sess.LastUserMessage, false) {
		lines = append(lines, detailIndent+helpStyle.Render(row))
	}
	// Kept from the end: the assistant's answer lands in its last words, while
	// its opening is usually restating the question.
	for _, row := range msgRows("🤖 ", sess.LastAssistantMessage, true) {
		lines = append(lines, detailIndent+helpStyle.Render(row))
	}

	return strings.Join(lines, "\n")
}

// renderRepoBranch lays out the repo / branch pair inside avail columns: repo
// on the left, branch on the right. It is the second row of a list row
// (renderSession), and the only place either string is drawn.
//
// Everything here is rendered through the caller's style, the gaps included. The
// gap between repo and branch is the one stretch of the row with no glyph of its
// own, so filling it with a bare strings.Repeat(" ", n) would drop the background
// out of a viewed row exactly where the row is widest.
//
// The branch wins the width fight and is truncated from the end, so a long name
// keeps the part that identifies it ("...multi-action-dispatch" rather than
// "feat/"); the repo gets what is left. The main use is several sessions on one
// repo, where the repo name is identical on every row and the branch is what
// tells them apart.
//
// The repo is shown in full or not at all: a truncated disambiguator
// disambiguates nothing — "jind-..." fits both jind-ai and jind-ai-notifier —
// and this row is the only place it appears. With no repo name, the working
// directory stands in, truncated from the END, since its tail is what identifies
// it while its head is shared by everything under the same root.
//
// Both strings go through sanitizeRowText before anything measures them. git
// forbids control characters in a refname, but the working directory that stands
// in for a missing repo name comes from the filesystem, and POSIX lets a
// directory name hold a newline. One of those here does not merely overflow a
// pane: this row is drawn for every session in the list, all the time, so the
// extra line desyncs sessionRowHeight from every hit-test and scroll offset.
func renderRepoBranch(sess session.Info, avail int, style lipgloss.Style) string {
	repo, repoTruncatable := sess.RepoName, false
	if repo == "" {
		repo = sess.CurrentWorkDir
		if repo == "" {
			repo = sess.WorkDir
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(repo, home) {
			repo = "~" + repo[len(home):]
		}
		repoTruncatable = true
	}
	// After the fallbacks, not before. Only one kind of value tells the two orders
	// apart, and it is not the obvious one: a repo name of control characters
	// alone comes out of sanitizeRowText as spaces, which are non-empty either
	// way. An escape sequence is what differs — ansi.Strip removes it whole,
	// leaving "". Sanitizing first would let that empty result fall through to the
	// working directory and print a path beside a repo the session HAS but never
	// names.
	repo = sanitizeRowText(repo)

	branch := sanitizeRowText(sess.CurrentBranch)
	if branch == "" {
		// Nothing is competing for the columns here, so the full-or-nothing
		// rule does not apply: it exists to decide who wins a width fight, and
		// a blank line loses to a truncated repo name every time.
		return style.Render(truncateString(repo, avail))
	}

	// fitRepo reports what the repo gets to show inside budget columns, and
	// whether it earned any of them at all.
	fitRepo := func(budget int) (string, bool) {
		if repo == "" || budget < 1 {
			return "", false
		}
		if repoTruncatable {
			return truncateStringFromEnd(repo, budget), true
		}
		if ansi.StringWidth(repo) > budget {
			return "", false
		}
		return repo, true
	}

	branchText := truncateStringFromEnd(branch, avail)
	branchStyled := style.Render(branchText)
	// At least one column of separation between the two.
	repoText, ok := fitRepo(avail - lipgloss.Width(branchText) - 1)
	if !ok {
		return style.Render(strings.Repeat(" ", avail-lipgloss.Width(branchText))) + branchStyled
	}

	repoStyled := style.Render(repoText)
	gap := avail - lipgloss.Width(repoStyled) - lipgloss.Width(branchText)
	return repoStyled + style.Render(strings.Repeat(" ", gap)) + branchStyled
}

// padLine pads a string to the specified width with spaces.
func padLine(s string, width int) string {
	w := lipgloss.Width(s)
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// Display width in this file is measured with exactly one ruler: ansi
// (ansi.StringWidth, which is what lipgloss.Width calls). Everything that bounds
// a string must agree with whatever finally measures the composed line, or a
// "truncated" string still overflows its column.
//
// go-runewidth, which this file used to truncate with, is NOT that ruler and
// disagrees in two ways that both reach production:
//
//   - Variation-Selector-16 emoji (a common way for an agent to end a message)
//     are one cell to runewidth and two to ansi. Cutting a name to 33 runewidth
//     cells could therefore emit 66 real columns, wrap the row, and turn one
//     session row into two physical rows — breaking sessionRowHeight and every
//     hit-test offset below it.
//   - go-runewidth picks its East-Asian ambiguous-width table from the process
//     locale at init, so "○", "■" and "▶" become two cells under
//     LANG=ja_JP.UTF-8 and one under C.UTF-8. ansi does not follow the locale,
//     so the two rulers disagree per user.
//
// ansi also walks grapheme clusters rather than runes, so a cluster is never
// split down the middle.

// truncateString truncates a string to fit within maxWidth display columns,
// keeping the beginning and marking the cut with an ellipsis.
func truncateString(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return truncateToWidth(s, maxWidth)
	}
	return ansi.Truncate(s, maxWidth, "...")
}

// truncateStringFromEnd truncates a string, keeping the LAST maxWidth display
// columns. Used where the tail is the identifying part — a branch name
// ("...multi-action-dispatch" beats "feat/") or a filesystem path.
func truncateStringFromEnd(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return truncateFromEndToWidth(s, maxWidth)
	}
	return "..." + truncateFromEndToWidth(s, maxWidth-3)
}

// truncateToWidth truncates a string from the beginning to fit within
// maxWidth, with no ellipsis.
func truncateToWidth(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	return ansi.Truncate(s, maxWidth, "")
}

// truncateFromEndToWidth keeps at most the last maxWidth columns of s.
//
// The loop is not decoration: TruncateLeft drops whole grapheme clusters, so
// cutting exactly (width - maxWidth) columns off a string whose cluster straddles
// the cut returns a result one column too wide — a full-width character cannot
// be half-removed. Widening the cut one column at a time is the smallest
// correction that still keeps as much of the tail as fits.
func truncateFromEndToWidth(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	w := ansi.StringWidth(s)
	if w <= maxWidth {
		return s
	}
	for n := w - maxWidth; n <= w; n++ {
		out := ansi.TruncateLeft(s, n, "")
		if ansi.StringWidth(out) <= maxWidth {
			return out
		}
	}
	return ""
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
	case session.StatusDeleting:
		return "⟳", "DELETING", deletingStyle
	default:
		// "?" is PERMISSION's alone. The one-line session list shows the icon
		// without its label, so UNKNOWN can no longer share a glyph with it.
		return "·", "UNKNOWN", stoppedStyle
	}
}

// attentionGlyph is the unseen-completion dot. Padded through padIcon like a
// status icon so both cells are two columns wide however the terminal measures
// the rune.
const attentionGlyph = "●"

// padIcon pads a status icon to a fixed 2-column cell, measured with
// ansi.StringWidth. Icons already 2 columns or wider are returned
// unchanged.
//
// "⚡" occupies 2 columns while every other status icon occupies 1; without
// this pad, the name column in the one-line session list shifts by one on
// thinking rows and the list stops reading as a table.
func padIcon(icon string) string {
	if w := ansi.StringWidth(icon); w < 2 {
		return icon + strings.Repeat(" ", 2-w)
	}
	return icon
}

// partitionUnseenFirst reorders sessions so that, inside each fleet, the ones
// holding an unseen completion come first. Order is preserved in both halves
// and no session crosses a fleet boundary, so the daemon's canonical order
// (session.SortInfos) still decides everything else.
//
// Display-only, and applied where the poll's result is installed rather than
// in session.SortInfos: the CLI's list order and the switch-session popup's
// ranking are contracts of their own.
//
// Fleets are emitted in the order they first appear in the input rather than
// re-sorted, because that is the one property this function must not have an
// opinion about — groupSessionsByFleet decides fleet order downstream.
func partitionUnseenFirst(sessions []session.Info) []session.Info {
	fleetOrder := make([]string, 0, len(sessions))
	byFleet := make(map[string][]session.Info, len(sessions))
	for _, sess := range sessions {
		if _, seen := byFleet[sess.Fleet]; !seen {
			fleetOrder = append(fleetOrder, sess.Fleet)
		}
		byFleet[sess.Fleet] = append(byFleet[sess.Fleet], sess)
	}

	out := make([]session.Info, 0, len(sessions))
	for _, fleet := range fleetOrder {
		members := byFleet[fleet]
		for _, sess := range members {
			if sess.Attention.Unseen {
				out = append(out, sess)
			}
		}
		for _, sess := range members {
			if !sess.Attention.Unseen {
				out = append(out, sess)
			}
		}
	}
	return out
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
	// Truncated for the same reason the err/warn notices are: sessionCardTop
	// and totalCardLines count this header as exactly one row, so a fleet name
	// wider than the pane would wrap, become two physical rows, and push every
	// session below it out of alignment with the hit-test arithmetic.
	label := truncateString(strings.ToUpper(name), width)
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
		if ansi.StringWidth(rawLine) <= width {
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
				w := ansi.StringWidth(string(runes[end]))
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

// Identity reports which jin this Model was built to reach. Exported for
// cmd/jin/cmd's wiring test: that package builds the Model and has no other way
// to see whether the identity it passed arrived, and the answer decides which
// daemon every popup this UI opens will talk to.
func (m Model) Identity() jinenv.Identity { return m.identity }
