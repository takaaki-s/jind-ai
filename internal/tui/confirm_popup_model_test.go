package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/takaaki-s/jind-ai/internal/config"
)

// confirmKeyMsg turns a tea.KeyMsg.String() spelling back into the message
// that produces it, so the tables below can name keys the same way
// confirmSpec.results does.
func confirmKeyMsg(key string) tea.KeyMsg {
	switch key {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
}

// confirmModes is every mode the popup renders, in the order they appear in
// confirmSpecFor. Tests that must cover all of them walk this rather than
// re-listing the four names.
var confirmModes = []string{
	ConfirmModeKill,
	ConfirmModeDelete,
	ConfirmModeDeleteWorktree,
	ConfirmModeDeleteWorktreeForce,
	ConfirmModeDeleteSubmodules,
}

func TestSubmoduleRemovalConfirmation(t *testing.T) {
	spec, ok := confirmSpecFor(ConfirmModeDeleteSubmodules, "repo")
	if !ok || !strings.Contains(spec.subtitle, "commits may be lost") {
		t.Fatalf("missing warning: %+v", spec)
	}
	if spec.results["y"] != ConfirmResultForceYes || spec.results["n"] != ConfirmResultForceNo || spec.results["enter"] != "" {
		t.Fatal("force deletion must require explicit y")
	}
}

// TestConfirmPopupModel_ViewShape asserts whole lines, not fragments: every
// hint is "<key>  <desc>", so a substring check for "y" matches the session
// name and one for "n" matches "cancel" — the keys could be swapped between
// answers and it would still pass. On a destructive prompt the key/answer
// pairing is the thing worth pinning.
func TestConfirmPopupModel_ViewShape(t *testing.T) {
	cases := []struct {
		mode      string
		wantLines []string
	}{
		{
			mode:      ConfirmModeKill,
			wantLines: []string{"Kill 'my-sess'?", "y  kill", "n  cancel"},
		},
		{
			mode:      ConfirmModeDelete,
			wantLines: []string{"Delete 'my-sess'?", "y  delete", "n  cancel"},
		},
		{
			mode: ConfirmModeDeleteWorktree,
			wantLines: []string{
				"Delete 'my-sess'?", "Session is in a git worktree",
				"y  delete session only",
				"w  delete session + worktree",
				"n  cancel",
			},
		},
		{
			mode: ConfirmModeDeleteWorktreeForce,
			wantLines: []string{
				// Target in the title, reason in the subtitle: on one line the
				// reason is what a session name of any length truncates away.
				"⚠ Delete 'my-sess'?",
				"Worktree has uncommitted changes",
				"y  force delete worktree",
				"n  delete session only",
				// Both answers above destroy something, so the way out has to
				// be on screen even though Update, not the spec, handles it.
				"ctrl+c  cancel, delete nothing",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			m := newConfirmPopupModel(t, c.mode)
			next, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
			got := confirmDialogContent(t, next.(ConfirmPopupModel).View())

			for _, want := range c.wantLines {
				if !slices.Contains(got, want) {
					t.Errorf("View() has no line %q:\n%q", want, got)
				}
			}
		})
	}
}

// TestNewConfirmPopupModel_ModeGuard covers the constructor's whole contract
// with `jin confirm-popup`: a stale or garbled JIN_CONFIRM_MODE is refused, so
// the popup exits without drawing a prompt it cannot attribute to a real
// request. Answering such a prompt would destroy a session.
func TestNewConfirmPopupModel_ModeGuard(t *testing.T) {
	for _, mode := range confirmModes {
		if _, ok := NewConfirmPopupModel(mode, "my-sess"); !ok {
			t.Errorf("NewConfirmPopupModel(%q) ok = false, want true", mode)
		}
	}
	for _, mode := range []string{"", "bogus", "Kill", "KILL", "delete "} {
		if _, ok := NewConfirmPopupModel(mode, "my-sess"); ok {
			t.Errorf("NewConfirmPopupModel(%q) ok = true, want false (the popup must bail out)", mode)
		}
	}
}

// newConfirmPopupModel builds the model for a mode the test expects to be
// known, failing the test rather than returning a half-built dialog.
func newConfirmPopupModel(t *testing.T, mode string) ConfirmPopupModel {
	t.Helper()
	m, ok := NewConfirmPopupModel(mode, "my-sess")
	if !ok {
		t.Fatalf("NewConfirmPopupModel(%q) ok = false, want true", mode)
	}
	return m
}

func TestConfirmPopupModel_KeyResults(t *testing.T) {
	cases := []struct {
		name string
		mode string
		key  string
		want string // "" means the key must be ignored (no result, no quit)
	}{
		{"kill/y", ConfirmModeKill, "y", ConfirmResultYes},
		{"kill/Y", ConfirmModeKill, "Y", ConfirmResultYes},
		{"kill/enter", ConfirmModeKill, "enter", ConfirmResultYes},
		{"kill/n", ConfirmModeKill, "n", ConfirmResultNo},
		{"kill/N", ConfirmModeKill, "N", ConfirmResultNo},
		{"kill/esc", ConfirmModeKill, "esc", ConfirmResultNo},
		{"kill ignores w", ConfirmModeKill, "w", ""},

		{"delete/y", ConfirmModeDelete, "y", ConfirmResultYes},
		{"delete/enter", ConfirmModeDelete, "enter", ConfirmResultYes},
		{"delete/n", ConfirmModeDelete, "n", ConfirmResultNo},
		{"delete/esc", ConfirmModeDelete, "esc", ConfirmResultNo},
		{"delete ignores w", ConfirmModeDelete, "w", ""},

		{"worktree/y", ConfirmModeDeleteWorktree, "y", ConfirmResultYes},
		{"worktree/enter", ConfirmModeDeleteWorktree, "enter", ConfirmResultYes},
		{"worktree/w", ConfirmModeDeleteWorktree, "w", ConfirmResultWorktree},
		{"worktree/W", ConfirmModeDeleteWorktree, "W", ConfirmResultWorktree},
		{"worktree/n", ConfirmModeDeleteWorktree, "n", ConfirmResultNo},
		{"worktree/esc", ConfirmModeDeleteWorktree, "esc", ConfirmResultNo},
		{"worktree ignores q", ConfirmModeDeleteWorktree, "q", ""},

		{"force/y", ConfirmModeDeleteWorktreeForce, "y", ConfirmResultForceYes},
		{"force/Y", ConfirmModeDeleteWorktreeForce, "Y", ConfirmResultForceYes},
		{"force/n", ConfirmModeDeleteWorktreeForce, "n", ConfirmResultForceNo},
		{"force/esc", ConfirmModeDeleteWorktreeForce, "esc", ConfirmResultForceNo},
		// Enter is deliberately not a shortcut for force-delete: discarding
		// uncommitted work takes an explicit "y".
		{"force ignores enter", ConfirmModeDeleteWorktreeForce, "enter", ""},
		{"force ignores w", ConfirmModeDeleteWorktreeForce, "w", ""},
		// An unknown mode never gets this far: NewConfirmPopupModel refuses it
		// and the popup exits (TestNewConfirmPopupModel_ModeGuard).
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newConfirmPopupModel(t, c.mode)
			next, cmd := m.Update(confirmKeyMsg(c.key))
			got := next.(ConfirmPopupModel)

			if got.Selected() != c.want {
				t.Errorf("Selected() = %q, want %q", got.Selected(), c.want)
			}
			if c.want == "" {
				if cmd != nil {
					t.Errorf("key %q should be ignored, but returned a cmd", c.key)
				}
				return
			}
			if cmd == nil {
				t.Fatalf("key %q should quit, got nil cmd", c.key)
			}
			if msg := cmd(); msg != (tea.QuitMsg{}) {
				t.Errorf("cmd returned %T, want tea.QuitMsg", msg)
			}
		})
	}
}

// TestConfirmPopupModel_CtrlCDismisses guards the safety property: Ctrl+C
// closes the popup with an empty result, which the parent reads as "do
// nothing". Esc is not a dismissal — it is a real "no" answer, covered above.
func TestConfirmPopupModel_CtrlCDismisses(t *testing.T) {
	for _, mode := range confirmModes {
		t.Run(mode, func(t *testing.T) {
			m := newConfirmPopupModel(t, mode)
			next, cmd := m.Update(confirmKeyMsg("ctrl+c"))

			if got := next.(ConfirmPopupModel).Selected(); got != "" {
				t.Errorf("Selected() after ctrl+c = %q, want empty", got)
			}
			if cmd == nil {
				t.Fatal("ctrl+c should quit, got nil cmd")
			}
			if msg := cmd(); msg != (tea.QuitMsg{}) {
				t.Errorf("cmd returned %T, want tea.QuitMsg", msg)
			}
		})
	}
}

// confirmDialogContent strips styling, returning the dialog's lines as the
// user reads them. There is no frame to strip: tmux draws the popup's only
// border, outside the pty this content is rendered into.
func confirmDialogContent(t *testing.T, dialog string) []string {
	t.Helper()
	rows := strings.Split(stripANSI(dialog), "\n")
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, strings.TrimSpace(row))
	}
	return out
}

// TestRenderConfirmDialog_HeightDegradation pins the order in which the dialog
// gives up lines when the popup pty is shorter than the dialog wants to be.
// bubbletea's renderer truncates an over-tall frame from the TOP, so a dialog
// that does not clamp itself loses the title first — the user would be asked
// to approve a destruction without seeing what is being destroyed. The tallest
// dialog (delete_worktree) is the one that runs out of room first.
//
// The hint cases are the second half of the same property: hints go from the
// middle, never from the end, so the row that declines survives down to the
// last line. Trimming the tail would degrade this prompt into one that offers
// only the two answers that destroy something.
func TestRenderConfirmDialog_HeightDegradation(t *testing.T) {
	const (
		title    = "Delete 'my-sess'?"
		subtitle = "Session is in a git worktree"
	)
	hints := []keyHint{
		{"y", "delete session only"},
		{"w", "delete session + worktree"},
		{"n", "cancel"},
	}
	const (
		hintY = "y  delete session only"
		hintW = "w  delete session + worktree"
		hintN = "n  cancel"
	)

	cases := []struct {
		name   string
		height int
		want   []string
	}{
		{
			// The default confirm popup on an 80x24 client: 50% of 24 rows is
			// a 12-row popup, and tmux's own border leaves a 10-row inner pty
			// (measured). The dialog draws no frame of its own, so all 10 rows
			// are content and the tallest dialog needs 6.
			name:   "default popup on an 80x24 client",
			height: 10,
			want:   []string{title, subtitle, "", hintY, hintW, hintN},
		},
		{name: "exact fit", height: 6, want: []string{title, subtitle, "", hintY, hintW, hintN}},
		{name: "spacer goes first", height: 5, want: []string{title, subtitle, hintY, hintW, hintN}},
		{name: "then the subtitle", height: 4, want: []string{title, hintY, hintW, hintN}},
		// Hints go from the middle: "w" is dropped, "n cancel" stays.
		{name: "then hints, from the middle", height: 3, want: []string{title, hintY, hintN}},
		{name: "cancel is the last hint standing", height: 2, want: []string{title, hintN}},
		// Floor: at a single row the clamp still yields the title rather
		// than nothing at all.
		{name: "nothing fits", height: 1, want: []string{title}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := confirmDialogContent(t, renderConfirmDialog(title, subtitle, hints, 60, c.height))
			if len(got) != len(c.want) {
				t.Fatalf("content = %q, want %q", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestRenderConfirmDialog_DeclineHintNeverDropped is the invariant behind the
// table above, swept across every mode and every height a popup can hand the
// dialog: while any hint is shown at all, the last one is among them. That row
// is the escape hatch — "n cancel" for three modes, and for the force dialog
// the Ctrl+C row that leaves both the session and its dirty worktree alone. A
// truncation that ate it would leave a destructive prompt advertising only
// destructive answers.
func TestRenderConfirmDialog_DeclineHintNeverDropped(t *testing.T) {
	for _, mode := range confirmModes {
		spec, ok := confirmSpecFor(mode, "my-sess")
		if !ok {
			t.Fatalf("confirmSpecFor(%q) = not ok", mode)
		}
		rows := make([]string, 0, len(spec.hints))
		for _, h := range spec.hints {
			rows = append(rows, h.key+"  "+h.desc)
		}
		wayOut := rows[len(rows)-1]

		for height := 1; height <= 12; height++ {
			t.Run(fmt.Sprintf("%s/h%d", mode, height), func(t *testing.T) {
				got := confirmDialogContent(t, renderConfirmDialog(spec.title, spec.subtitle, spec.hints, 60, height))
				var shown int
				for _, row := range rows {
					if slices.Contains(got, row) {
						shown++
					}
				}
				if shown == 0 {
					return // Too short for any hint at all; the title still shows.
				}
				if !slices.Contains(got, wayOut) {
					t.Errorf("height %d shows %d hint(s) but not the way out %q:\n%q", height, shown, wayOut, got)
				}
			})
		}
	}
}

// TestConfirmPopupModel_ViewFitsShortPopup is the property the degradation
// order exists to protect, checked through the real View for every mode: the
// box never exceeds the pty it was given (so nothing is truncated away by the
// renderer) and the title is always on screen.
func TestConfirmPopupModel_ViewFitsShortPopup(t *testing.T) {
	for _, mode := range confirmModes {
		for height := 3; height <= 12; height++ {
			t.Run(fmt.Sprintf("%s/h%d", mode, height), func(t *testing.T) {
				m := newConfirmPopupModel(t, mode)
				next, _ := m.Update(tea.WindowSizeMsg{Width: 36, Height: height})
				view := stripANSI(next.(ConfirmPopupModel).View())

				if got := lipgloss.Height(view); got > height {
					t.Errorf("View() is %d rows in a %d-row popup:\n%s", got, height, view)
				}
				if !strings.Contains(view, "my-sess") {
					t.Errorf("View() at height %d does not name the target:\n%s", height, view)
				}
			})
		}
	}
}

// TestConfirmPopupDefaultSizeFitsTheDialog pins the invariant that spans two
// packages and was, until this test, held together by a comment in each: the
// catalog's absolute confirm size (internal/config) has to leave room for this
// package's dialog clamps. tmux draws the popup border inside the requested
// -w/-h, costing one cell per side (measured), and the dialog draws no frame of
// its own — so the inner pty must hold confirmDialogMaxInner columns and the
// tallest dialog's lines, with the leftover being the one cell of padding the
// size was chosen for. Change either side alone and the padding goes, or the
// dialog starts shedding lines at its default size.
//
// The dependency only points this way: internal/tui already imports
// internal/config, and the reverse would cycle.
func TestConfirmPopupDefaultSizeFitsTheDialog(t *testing.T) {
	mgr, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	w, h := mgr.GetPopupSize(config.PopupConfirm)

	width, err := strconv.Atoi(w)
	if err != nil {
		t.Fatalf("default confirm width = %q, want absolute cells: a percentage of an "+
			"arbitrary client cannot be checked against the dialog's fixed size", w)
	}
	height, err := strconv.Atoi(h)
	if err != nil {
		t.Fatalf("default confirm height = %q, want absolute cells", h)
	}

	const border = 2 // one cell per side, drawn by tmux inside -w/-h
	innerWidth, innerHeight := width-border, height-border

	// Derive the tallest dialog from the modes themselves rather than pinning a
	// number here: a mode gaining a hint has to move this size, not this test.
	tallest, tallestMode := 0, ""
	for _, mode := range confirmModes {
		spec, ok := confirmSpecFor(mode, "my-sess")
		if !ok {
			t.Fatalf("confirmSpecFor(%q) = not ok", mode)
		}
		// A height no dialog can reach, so nothing is clamped away.
		lines := lipgloss.Height(renderConfirmDialog(spec.title, spec.subtitle, spec.hints, innerWidth, 100))
		if lines > tallest {
			tallest, tallestMode = lines, mode
		}
	}

	if innerWidth < confirmDialogMaxInner {
		t.Errorf("default confirm popup leaves %d columns inside the border, but the dialog clamps to %d",
			innerWidth, confirmDialogMaxInner)
	}
	if innerHeight < tallest {
		t.Errorf("default confirm popup leaves %d rows inside the border, but the %s dialog is %d lines",
			innerHeight, tallestMode, tallest)
	}
}
