package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Confirm popup modes. The parent TUI writes one of these to
// JIN_CONFIRM_MODE before opening the popup; the popup renders the matching
// dialog. Values are part of the parent↔popup env contract, so they must
// stay stable even if the Go constant names change.
const (
	ConfirmModeKill                = "kill"
	ConfirmModeDelete              = "delete"
	ConfirmModeDeleteWorktree      = "delete_worktree"
	ConfirmModeDeleteWorktreeForce = "delete_worktree_force"
	ConfirmModeDeleteSubmodules    = "delete_submodules"
)

// Confirm popup results, written back to JIN_CONFIRM_RESULT. The empty
// string is not among them on purpose: it is reserved for "dismissed without
// answering", which the caller must treat as "do nothing".
const (
	ConfirmResultYes      = "yes"
	ConfirmResultNo       = "no"
	ConfirmResultWorktree = "worktree"
	ConfirmResultForceYes = "force_yes"
	ConfirmResultForceNo  = "force_no"
)

const (
	// confirmDialogMinInner is the content width below which the dialog
	// becomes uncomfortably cramped; on very narrow popups we still honor it
	// and let the text run to the edges.
	confirmDialogMinInner = 24
	// confirmDialogMaxInner caps the dialog on wide popups so it reads as a
	// centered prompt rather than a banner. Widened only if the longest
	// hint line demands more room.
	confirmDialogMaxInner = 44

	// Placement fallbacks used before the first tea.WindowSizeMsg lands. They
	// only decide the centering box for that one frame, and they are roomier
	// than the settled popup: at the default confirm size an 80-column client
	// gives an inner pty of 38 columns and 10 rows.
	confirmFallbackWidth  = 60
	confirmFallbackHeight = 20
)

// keyHint is one "<key>  <description>" row inside a confirm dialog.
type keyHint struct {
	key  string
	desc string
}

// confirmSpec is everything that differs between the confirm modes: what the
// dialog says and which keys it answers to. Keeping the visible hints and
// the accepted keys in one value means a mode can't drift into advertising a
// key it ignores (or silently accepting one it never shows). Ctrl+C is the one
// key a hint may name without appearing in results: Update dismisses on it for
// every mode, and dismissal is the absence of an answer rather than one of them.
type confirmSpec struct {
	title    string
	subtitle string
	hints    []keyHint
	// results maps a tea.KeyMsg string to the result it selects. Keys absent
	// from the map are ignored outright — a stray press must not be able to
	// answer a destructive prompt.
	results map[string]string
}

// confirmSpecFor resolves the dialog for mode, with desc interpolated into
// the title. The bool reports whether mode is known; callers use it to
// refuse rendering a dialog for a stale or garbled JIN_CONFIRM_MODE.
func confirmSpecFor(mode, desc string) (confirmSpec, bool) {
	switch mode {
	case ConfirmModeKill:
		return confirmSpec{
			title: fmt.Sprintf("Kill '%s'?", desc),
			hints: []keyHint{
				{"y", "kill"},
				{"n", "cancel"},
			},
			results: map[string]string{
				"y": ConfirmResultYes, "Y": ConfirmResultYes, "enter": ConfirmResultYes,
				"n": ConfirmResultNo, "N": ConfirmResultNo, "esc": ConfirmResultNo,
			},
		}, true
	case ConfirmModeDelete:
		return confirmSpec{
			title: fmt.Sprintf("Delete '%s'?", desc),
			hints: []keyHint{
				{"y", "delete"},
				{"n", "cancel"},
			},
			results: map[string]string{
				"y": ConfirmResultYes, "Y": ConfirmResultYes, "enter": ConfirmResultYes,
				"n": ConfirmResultNo, "N": ConfirmResultNo, "esc": ConfirmResultNo,
			},
		}, true
	case ConfirmModeDeleteWorktree:
		return confirmSpec{
			title:    fmt.Sprintf("Delete '%s'?", desc),
			subtitle: "Session is in a git worktree",
			hints: []keyHint{
				{"y", "delete session only"},
				{"w", "delete session + worktree"},
				{"n", "cancel"},
			},
			results: map[string]string{
				"y": ConfirmResultYes, "Y": ConfirmResultYes, "enter": ConfirmResultYes,
				"w": ConfirmResultWorktree, "W": ConfirmResultWorktree,
				"n": ConfirmResultNo, "N": ConfirmResultNo, "esc": ConfirmResultNo,
			},
		}, true
	case ConfirmModeDeleteWorktreeForce, ConfirmModeDeleteSubmodules:
		subtitle := "Worktree has uncommitted changes"
		if mode == ConfirmModeDeleteSubmodules {
			subtitle = "Submodule work/commits may be lost"
		}
		return confirmSpec{
			// Names the session like the other three modes: this prompt is
			// raised asynchronously in a popup of its own, so the title is
			// the only context the user has — and both answers destroy
			// something.
			//
			// The reason sits in the subtitle because the two compete for
			// one truncated line otherwise: at the default popup size the
			// content width is 34, so the reason is lost for any name longer
			// than six characters. Split, the title holds names up to 22.
			title:    fmt.Sprintf("⚠ Delete '%s'?", desc),
			subtitle: subtitle,
			hints: []keyHint{
				{"y", "force delete worktree"},
				{"n", "delete session only"},
				// The only non-destructive way out, and the one mode
				// where it has to be advertised: both answers above
				// destroy something. Accurate as written — the daemon
				// refused the dirty worktree before this prompt was
				// raised, so nothing has been deleted yet. Ctrl+C is not
				// in results: Update handles it for every mode.
				{"ctrl+c", "cancel, delete nothing"},
			},
			// No "enter" here: force-removing a dirty worktree discards work,
			// so it takes a deliberate "y". Esc means force_no rather than a
			// dismissal because the user already asked for the worktree to go
			// — falling back to session-only delete is the safe answer.
			results: map[string]string{
				"y": ConfirmResultForceYes, "Y": ConfirmResultForceYes,
				"n": ConfirmResultForceNo, "N": ConfirmResultForceNo, "esc": ConfirmResultForceNo,
			},
		}, true
	}
	return confirmSpec{}, false
}

// ConfirmPopupModel is the standalone Bubble Tea model behind
// `jin confirm-popup`: a single yes/no(/worktree) prompt for a destructive
// action. It runs in its own tmux popup so it owns keyboard focus while open —
// the parent TUI's pane does not hold focus when the action palette launched
// the action.
type ConfirmPopupModel struct {
	spec   confirmSpec
	result string
	width  int
	height int
}

// NewConfirmPopupModel builds the dialog for mode, naming targetDesc in the
// title. The bool reports whether mode is one this popup can render: false for
// a stale or garbled JIN_CONFIRM_MODE, and the caller must then run nothing
// rather than show a prompt it cannot attribute to a real request.
func NewConfirmPopupModel(mode, targetDesc string) (ConfirmPopupModel, bool) {
	spec, ok := confirmSpecFor(mode, targetDesc)
	if !ok {
		return ConfirmPopupModel{}, false
	}
	return ConfirmPopupModel{spec: spec}, true
}

// Selected returns the chosen result, or "" when the popup was dismissed
// without answering (Ctrl+C). The empty case is the safety property of this
// popup: the parent must treat it as "do nothing", so a dismissed prompt can
// never destroy a session.
func (m ConfirmPopupModel) Selected() string {
	return m.result
}

func (m ConfirmPopupModel) Init() tea.Cmd {
	return nil
}

func (m ConfirmPopupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if result, ok := m.spec.results[msg.String()]; ok {
			m.result = result
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m ConfirmPopupModel) View() string {
	width, height := m.width, m.height
	if width <= 0 {
		width = confirmFallbackWidth
	}
	if height <= 0 {
		height = confirmFallbackHeight
	}
	dialog := renderConfirmDialog(m.spec.title, m.spec.subtitle, m.spec.hints, width, height)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, dialog)
}

// renderConfirmDialog lays out the confirm dialog: warning-tinted title,
// optional dim subtitle, then a run of "<key>  <desc>" lines. Text is truncated
// to the resolved width so wide session names never sprawl. availWidth /
// availHeight are the popup's inner pty — the space tmux leaves inside the
// border it draws itself, which is why nothing here adds a frame of its own.
func renderConfirmDialog(title, subtitle string, hints []keyHint, availWidth, availHeight int) string {
	longest := lipgloss.Width(title)
	if w := lipgloss.Width(subtitle); w > longest {
		longest = w
	}
	for _, h := range hints {
		// "<key><two-space gap><desc>"
		if w := lipgloss.Width(h.key) + 2 + lipgloss.Width(h.desc); w > longest {
			longest = w
		}
	}

	// Content width: honor [min, max], but shrink further when the popup is
	// narrower than the max. On very narrow popups we still floor at the min
	// and let the placement cope — bubbletea truncates an over-wide line on
	// the right rather than wrapping it, so the title survives either way.
	maxInner := max(confirmDialogMinInner, min(confirmDialogMaxInner, availWidth))
	inner := min(max(longest, confirmDialogMinInner), maxInner)

	titleLine := confirmTitleStyle.Render(truncateString(title, inner))
	subtitleLine := ""
	if subtitle != "" {
		subtitleLine = helpStyle.Render(truncateString(subtitle, inner))
	}
	hintLines := make([]string, 0, len(hints))
	for _, h := range hints {
		descAvail := max(1, inner-lipgloss.Width(h.key)-2)
		hintLines = append(hintLines,
			confirmKeyStyle.Render(h.key)+helpStyle.Render("  "+truncateString(h.desc, descAvail)),
		)
	}

	// The whole inner pty is the vertical budget: tmux's border sits outside
	// it, so nothing has to be reserved here.
	body := clampConfirmLines(titleLine, subtitleLine, hintLines, availHeight)
	// Pad every line out to the resolved width so the block is a rectangle.
	// Without this the caller's centering would center each line on its own
	// and the hint keys would not line up in a column.
	return lipgloss.NewStyle().Width(inner).Render(strings.Join(body, "\n"))
}

// clampConfirmLines assembles the dialog body from its already-styled parts,
// shedding from least to most load-bearing until it fits maxLines rows: the
// blank spacer first, then the subtitle, then hints. The title always survives;
// the LAST hint survives whenever any hint is shown at all.
//
// It exists because bubbletea's renderer truncates an over-tall frame from the
// TOP: on a popup shorter than the dialog, the rows naming what is about to be
// destroyed are what would disappear, leaving a bare y/n prompt.
//
// Hints go from the middle rather than off the end because every mode puts its
// way out last. Trimming the tail would leave a prompt advertising only the
// answers that destroy something.
func clampConfirmLines(title, subtitle string, hints []string, maxLines int) []string {
	maxLines = max(maxLines, 1)
	head := 1 // title
	if subtitle != "" {
		head++
	}

	lines := make([]string, 0, head+1+len(hints))
	lines = append(lines, title)
	if subtitle != "" && head+len(hints) <= maxLines {
		lines = append(lines, subtitle)
	}
	if head+1+len(hints) <= maxLines {
		lines = append(lines, "")
	}
	switch room := maxLines - len(lines); {
	case room >= len(hints):
		lines = append(lines, hints...)
	case room > 0:
		lines = append(lines, hints[:room-1]...)
		lines = append(lines, hints[len(hints)-1])
	}
	return lines
}
