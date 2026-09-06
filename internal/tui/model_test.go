package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/takaaki-s/jind-ai/internal/action"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/jinenv"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/testutil"
	"github.com/takaaki-s/jind-ai/internal/tmux"
)

// Force TrueColor output during tests so styling assertions (e.g. the
// presence of a background SGR sequence on viewed cards) are reliable
// regardless of the CI environment's TTY detection.
func init() {
	lipgloss.SetColorProfile(termenv.TrueColor)
}

// --- truncateString ---

func TestTruncateString(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     string
	}{
		{
			name:     "short string within limit",
			input:    "hello",
			maxWidth: 10,
			want:     "hello",
		},
		{
			name:     "string exactly at limit",
			input:    "hello",
			maxWidth: 5,
			want:     "hello",
		},
		{
			name:     "string needs truncation",
			input:    "hello world this is long",
			maxWidth: 10,
			want:     "hello w...",
		},
		{
			name:     "maxWidth 3 gets ellipsis",
			input:    "hello world",
			maxWidth: 3,
			want:     "hel",
		},
		{
			name:     "maxWidth 2 no ellipsis",
			input:    "hello",
			maxWidth: 2,
			want:     "he",
		},
		{
			name:     "maxWidth 1",
			input:    "hello",
			maxWidth: 1,
			want:     "h",
		},
		{
			name:     "empty string",
			input:    "",
			maxWidth: 10,
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateString(tt.input, tt.maxWidth)
			if got != tt.want {
				t.Errorf("truncateString(%q, %d) = %q, want %q", tt.input, tt.maxWidth, got, tt.want)
			}
		})
	}
}

// --- truncateStringFromEnd ---

func TestTruncateStringFromEnd(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     string
	}{
		{
			name:     "short string within limit",
			input:    "hello",
			maxWidth: 10,
			want:     "hello",
		},
		{
			name:     "string exactly at limit",
			input:    "hello",
			maxWidth: 5,
			want:     "hello",
		},
		{
			name:     "string needs truncation keeps end",
			input:    "hello world",
			maxWidth: 8,
			want:     "...world",
		},
		{
			name:     "maxWidth 3 no ellipsis",
			input:    "hello world",
			maxWidth: 3,
			want:     "rld",
		},
		{
			name:     "maxWidth 2",
			input:    "hello",
			maxWidth: 2,
			want:     "lo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateStringFromEnd(tt.input, tt.maxWidth)
			if got != tt.want {
				t.Errorf("truncateStringFromEnd(%q, %d) = %q, want %q", tt.input, tt.maxWidth, got, tt.want)
			}
		})
	}
}

// --- timeAgo ---

func TestTimeAgo(t *testing.T) {
	tests := []struct {
		name   string
		offset time.Duration
		want   string
	}{
		{"just now", 10 * time.Second, "just now"},
		{"1 minute ago", 1 * time.Minute, "1m ago"},
		{"5 minutes ago", 5 * time.Minute, "5m ago"},
		{"59 minutes ago", 59 * time.Minute, "59m ago"},
		{"1 hour ago", 1 * time.Hour, "1h ago"},
		{"3 hours ago", 3 * time.Hour, "3h ago"},
		{"1 day ago", 24 * time.Hour, "1d ago"},
		{"5 days ago", 5 * 24 * time.Hour, "5d ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			past := time.Now().Add(-tt.offset)
			got := timeAgo(past)
			if got != tt.want {
				t.Errorf("timeAgo(now - %v) = %q, want %q", tt.offset, got, tt.want)
			}
		})
	}
}

// --- getStatusDisplay ---

// TestGetStatusDisplay pins the icon-collision bug that motivated the
// PERMISSION/UNKNOWN split: PERMISSION used to share "?" with the default
// case, and once the one-line session list started showing the icon without
// its label, the two became indistinguishable. It also exercises padIcon,
// since the list's column alignment depends on every icon padding to the
// same 2-column cell.
func TestGetStatusDisplay(t *testing.T) {
	// The table pins the vocabulary itself; the loop below pins the invariant
	// that no two entries may ever converge again.
	statuses := []struct {
		status    session.Status
		wantIcon  string
		wantLabel string
	}{
		{session.StatusThinking, "⚡", "THINKING"},
		{session.StatusPermission, "?", "PERMISSION"},
		{session.StatusRunning, "▶", "RUNNING"},
		{session.StatusCreating, "+", "CREATING"},
		{session.StatusIdle, "○", "IDLE"},
		{session.StatusStopped, "■", "STOPPED"},
		{session.StatusDeleting, "⟳", "DELETING"},
		{session.Status("bogus-status"), "·", "UNKNOWN"}, // falls through to default
	}

	seenIcons := make(map[string]session.Status)
	for _, tt := range statuses {
		status := tt.status
		icon, label, _ := getStatusDisplay(status)

		if icon != tt.wantIcon {
			t.Errorf("getStatusDisplay(%q) icon = %q, want %q", status, icon, tt.wantIcon)
		}
		if label != tt.wantLabel {
			t.Errorf("getStatusDisplay(%q) label = %q, want %q", status, label, tt.wantLabel)
		}

		if prior, collides := seenIcons[icon]; collides {
			t.Errorf("getStatusDisplay(%q) icon %q collides with %q's icon", status, icon, prior)
		}
		seenIcons[icon] = status

		padded := padIcon(icon)
		if w := lipgloss.Width(padded); w != 2 {
			t.Errorf("padIcon(getStatusDisplay(%q)) = %q, width = %d, want 2", status, padded, w)
		}
		// "⚡" (THINKING) is already 2 columns wide; padIcon must pass it
		// through rather than appending a space that would overshoot.
		if lipgloss.Width(icon) >= 2 && padded != icon {
			t.Errorf("padIcon(%q) = %q, want unchanged (already >= 2 columns)", icon, padded)
		}
	}
}

// --- wrapText ---

func TestWrapText(t *testing.T) {
	t.Run("single short line", func(t *testing.T) {
		got := wrapText("hello", 20)
		if len(got) != 1 || got[0] != "hello" {
			t.Errorf("wrapText(%q, 20) = %v, want [%q]", "hello", got, "hello")
		}
	})

	t.Run("long line wraps", func(t *testing.T) {
		input := "abcdefghij" // 10 chars
		got := wrapText(input, 4)
		// Should wrap into: "abcd", "efgh", "ij"
		if len(got) != 3 {
			t.Fatalf("wrapText(%q, 4) got %d lines, want 3: %v", input, len(got), got)
		}
		if got[0] != "abcd" {
			t.Errorf("line 0 = %q, want %q", got[0], "abcd")
		}
		if got[1] != "efgh" {
			t.Errorf("line 1 = %q, want %q", got[1], "efgh")
		}
		if got[2] != "ij" {
			t.Errorf("line 2 = %q, want %q", got[2], "ij")
		}
	})

	t.Run("zero width returns original", func(t *testing.T) {
		got := wrapText("hello", 0)
		if len(got) != 1 || got[0] != "hello" {
			t.Errorf("wrapText(%q, 0) = %v, want [%q]", "hello", got, "hello")
		}
	})

	t.Run("negative width returns original", func(t *testing.T) {
		got := wrapText("hello", -1)
		if len(got) != 1 || got[0] != "hello" {
			t.Errorf("wrapText(%q, -1) = %v, want [%q]", "hello", got, "hello")
		}
	})

	t.Run("text with newlines", func(t *testing.T) {
		input := "line1\nline2\nline3"
		got := wrapText(input, 20)
		if len(got) != 3 {
			t.Fatalf("wrapText with newlines got %d lines, want 3: %v", len(got), got)
		}
		if got[0] != "line1" || got[1] != "line2" || got[2] != "line3" {
			t.Errorf("got %v, want [line1, line2, line3]", got)
		}
	})
}

// --- padLine ---

func TestPadLine(t *testing.T) {
	t.Run("shorter string gets padded", func(t *testing.T) {
		got := padLine("hi", 5)
		if got != "hi   " {
			t.Errorf("padLine(%q, 5) = %q, want %q", "hi", got, "hi   ")
		}
	})

	t.Run("exact width no padding", func(t *testing.T) {
		got := padLine("hello", 5)
		if got != "hello" {
			t.Errorf("padLine(%q, 5) = %q, want %q", "hello", got, "hello")
		}
	})

	t.Run("longer string no change", func(t *testing.T) {
		got := padLine("hello world", 5)
		if got != "hello world" {
			t.Errorf("padLine(%q, 5) = %q, want %q", "hello world", got, "hello world")
		}
	})

	t.Run("empty string gets full padding", func(t *testing.T) {
		got := padLine("", 3)
		if got != "   " {
			t.Errorf("padLine(%q, 3) = %q, want %q", "", got, "   ")
		}
	})
}

// --- isSessionAlive ---

func TestIsSessionAlive(t *testing.T) {
	alive := []session.Status{
		session.StatusRunning,
		session.StatusThinking,
		session.StatusIdle,
		session.StatusPermission,
		session.StatusCreating,
	}
	for _, s := range alive {
		t.Run(string(s)+"_alive", func(t *testing.T) {
			if !isSessionAlive(s) {
				t.Errorf("isSessionAlive(%q) = false, want true", s)
			}
		})
	}

	dead := []session.Status{
		session.StatusStopped,
		session.Status("unknown"),
	}
	for _, s := range dead {
		name := string(s)
		if name == "" {
			name = "empty"
		}
		t.Run(name+"_not_alive", func(t *testing.T) {
			if isSessionAlive(s) {
				t.Errorf("isSessionAlive(%q) = true, want false", s)
			}
		})
	}
}

// --- helper: verify truncation lengths ---

func TestTruncateStringLengthProperties(t *testing.T) {
	// Verify the truncated result has a display width <= maxWidth
	input := "this is a longer string for testing"
	for _, maxWidth := range []int{1, 2, 3, 5, 10, 15} {
		got := truncateString(input, maxWidth)
		// For ASCII-only strings, len is the display width
		if len(got) > maxWidth {
			t.Errorf("truncateString(%q, %d) = %q (len %d), exceeds maxWidth",
				input, maxWidth, got, len(got))
		}
	}
}

func TestTruncateStringFromEndLengthProperties(t *testing.T) {
	input := "this is a longer string for testing"
	for _, maxWidth := range []int{1, 2, 3, 5, 10, 15} {
		got := truncateStringFromEnd(input, maxWidth)
		if len(got) > maxWidth {
			t.Errorf("truncateStringFromEnd(%q, %d) = %q (len %d), exceeds maxWidth",
				input, maxWidth, got, len(got))
		}
	}
}

// Verify truncateStringFromEnd keeps the end of the string
func TestTruncateStringFromEndKeepsEnd(t *testing.T) {
	got := truncateStringFromEnd("/home/user/very/long/path/to/project", 20)
	if !strings.HasSuffix(got, "to/project") {
		t.Errorf("truncateStringFromEnd should keep the end, got %q", got)
	}
	if !strings.HasPrefix(got, "...") {
		t.Errorf("truncateStringFromEnd should start with '...', got %q", got)
	}
}

// --- truncateToWidth ---

func TestTruncateToWidth(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     string
	}{
		{
			name:     "ASCII within limit",
			input:    "hello",
			maxWidth: 10,
			want:     "hello",
		},
		{
			name:     "ASCII exact width",
			input:    "hello",
			maxWidth: 5,
			want:     "hello",
		},
		{
			name:     "ASCII truncated",
			input:    "hello world",
			maxWidth: 5,
			want:     "hello",
		},
		{
			name:     "empty string",
			input:    "",
			maxWidth: 10,
			want:     "",
		},
		{
			name:     "CJK characters fit",
			input:    "\u3042\u3044\u3046",
			maxWidth: 6,
			want:     "\u3042\u3044\u3046",
		},
		{
			name:     "CJK truncated at boundary",
			input:    "\u3042\u3044\u3046",
			maxWidth: 5,
			// Each CJK char is 2 cells wide; 2 chars = 4 cells fits, 3 chars = 6 cells > 5
			want: "\u3042\u3044",
		},
		{
			name:     "mixed ASCII and CJK",
			input:    "Aあ",
			maxWidth: 3,
			// 'A'=1 + 'あ'=2 = 3, fits exactly
			want: "Aあ",
		},
		{
			name:     "mixed ASCII and CJK truncated",
			input:    "Aあい",
			maxWidth: 3,
			// 'A'=1 + 'あ'=2 = 3, 'い' would be 5 > 3
			want: "Aあ",
		},
		{
			name:     "CJK does not fit partial",
			input:    "あ",
			maxWidth: 1,
			// 'あ' is 2 cells wide, does not fit in 1
			want: "",
		},
		{
			name:     "zero width",
			input:    "hello",
			maxWidth: 0,
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateToWidth(tt.input, tt.maxWidth)
			if got != tt.want {
				t.Errorf("truncateToWidth(%q, %d) = %q, want %q", tt.input, tt.maxWidth, got, tt.want)
			}
		})
	}
}

// --- truncateFromEndToWidth ---

func TestTruncateFromEndToWidth(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     string
	}{
		{
			name:     "ASCII within limit",
			input:    "hello",
			maxWidth: 10,
			want:     "hello",
		},
		{
			name:     "ASCII exact width",
			input:    "hello",
			maxWidth: 5,
			want:     "hello",
		},
		{
			name:     "ASCII truncated keeps end",
			input:    "hello world",
			maxWidth: 5,
			want:     "world",
		},
		{
			name:     "empty string",
			input:    "",
			maxWidth: 10,
			want:     "",
		},
		{
			name:     "CJK characters fit",
			input:    "あい",
			maxWidth: 4,
			want:     "あい",
		},
		{
			name:     "CJK truncated keeps end",
			input:    "あいう",
			maxWidth: 4,
			// Each CJK char is 2 cells; from end: 'う'=2, 'い'=2+2=4 fits, 'あ'=4+2=6 > 4
			want: "いう",
		},
		{
			name:     "CJK does not fit partial",
			input:    "あ",
			maxWidth: 1,
			// 'あ' is 2 cells wide, does not fit in 1
			want: "",
		},
		{
			name:     "mixed ASCII and CJK keeps end",
			input:    "あtest",
			maxWidth: 4,
			// from end: 't'=1, 's'=2, 'e'=3, 't'=4 => "test"
			want: "test",
		},
		{
			name:     "zero width",
			input:    "hello",
			maxWidth: 0,
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateFromEndToWidth(tt.input, tt.maxWidth)
			if got != tt.want {
				t.Errorf("truncateFromEndToWidth(%q, %d) = %q, want %q", tt.input, tt.maxWidth, got, tt.want)
			}
		})
	}
}

// --- list geometry ---

// TestSessionCardTop pins the line offsets the scroll and hit-test arithmetic
// is built on: sessionRowHeight rows per session, plus one row per fleet
// header. The height is now state-independent — a deleting session occupies
// the same rows as any other, which is what removed the old
// cardHeight/renderSession hand-sync contract.
func TestSessionCardTop(t *testing.T) {
	t.Run("single fleet", func(t *testing.T) {
		// Line 0 is the fleet header; session idx follows at
		// 1 + idx*sessionRowHeight, so with a two-row grid that is 1, 3, 5.
		m := cardListModel(3)
		for idx := range 3 {
			wantTop := 1 + idx*sessionRowHeight
			top, height := m.sessionCardTop(idx)
			if top != wantTop || height != sessionRowHeight {
				t.Errorf("sessionCardTop(%d) = (%d, %d), want (%d, %d)", idx, top, height, wantTop, sessionRowHeight)
			}
		}
	})

	t.Run("deleting session occupies the same rows", func(t *testing.T) {
		m := cardListModel(3)
		m.deletingIDs["0"] = true
		m.sessions[1].Status = session.StatusDeleting
		for idx := range 3 {
			wantTop := 1 + idx*sessionRowHeight
			top, height := m.sessionCardTop(idx)
			if top != wantTop || height != sessionRowHeight {
				t.Errorf("sessionCardTop(%d) = (%d, %d), want (%d, %d)", idx, top, height, wantTop, sessionRowHeight)
			}
		}
	})

	t.Run("second fleet adds one header row", func(t *testing.T) {
		// DefaultFleet always renders last, so the lines are: 0 = ALPHA,
		// 1.. = session 0, then DEFAULT, then sessions 1 and 2. Session 1
		// therefore starts one header row further down than it otherwise would.
		m := cardListModel(3)
		m.sessions[0].Fleet = "alpha"
		m.sessions[1].Fleet = session.DefaultFleet
		m.sessions[2].Fleet = session.DefaultFleet
		wantTops := []int{
			1,                        // ALPHA header, then session 0
			1 + sessionRowHeight + 1, // + session 0's rows + the DEFAULT header
			2 + 2*sessionRowHeight,   // + session 1's rows
		}
		for idx, wantTop := range wantTops {
			top, _ := m.sessionCardTop(idx)
			if top != wantTop {
				t.Errorf("sessionCardTop(%d) top = %d, want %d", idx, top, wantTop)
			}
		}
	})

	t.Run("out of range", func(t *testing.T) {
		m := cardListModel(3)
		for _, idx := range []int{-1, 3} {
			if top, height := m.sessionCardTop(idx); top != -1 || height != 0 {
				t.Errorf("sessionCardTop(%d) = (%d, %d), want (-1, 0)", idx, top, height)
			}
		}
	})
}

func TestTotalCardLines(t *testing.T) {
	t.Run("empty list has no rows", func(t *testing.T) {
		m := cardListModel(0)
		if got := m.totalCardLines(); got != 0 {
			t.Errorf("totalCardLines() = %d, want 0 (no sessions, no fleet header)", got)
		}
	})

	t.Run("single fleet", func(t *testing.T) {
		// 1 fleet header + 3 sessions of sessionRowHeight rows each.
		m := cardListModel(3)
		if want := 1 + 3*sessionRowHeight; m.totalCardLines() != want {
			t.Errorf("totalCardLines() = %d, want %d", m.totalCardLines(), want)
		}
	})

	t.Run("two fleets", func(t *testing.T) {
		// 2 fleet headers + 3 sessions of sessionRowHeight rows each.
		m := cardListModel(3)
		m.sessions[0].Fleet = "alpha"
		if want := 2 + 3*sessionRowHeight; m.totalCardLines() != want {
			t.Errorf("totalCardLines() = %d, want %d", m.totalCardLines(), want)
		}
	})

	t.Run("agrees with the last row's bottom", func(t *testing.T) {
		m := cardListModel(6)
		top, height := m.sessionCardTop(5)
		if got := m.totalCardLines(); got != top+height {
			t.Errorf("totalCardLines() = %d, want %d (bottom of the last row)", got, top+height)
		}
	})
}

// detailPaneHeightThreshold is the smallest m.height, with no notices and a
// valid cursor, that still leaves the detail pane a home: the chrome below the
// pane (rule + help line), the list header, the pane itself, and the shortest
// list worth putting above it. Derived rather than written out, so it follows
// detailNameLines, detailMsgLines and helpChromeLines.
const detailPaneHeightThreshold = helpChromeLines + listHeaderLines + detailPaneLines + minListLines // 18

// TestListAreaLines covers how contentAreaLines is split into the three
// regions, and the D-3 boundary where the detail pane is dropped whole rather
// than shrunk.
//
// Derivation, with no notices and a valid cursor:
//
//	contentAreaLines = max(m.height-helpChromeLines(2), 3)
//	list             = contentAreaLines - listHeaderLines(2) - detailPaneLines(8)
//	detail is drawn iff list >= minListLines(6), i.e. m.height >= 18
func TestListAreaLines(t *testing.T) {
	t.Run("the floor is a whole number of session rows", func(t *testing.T) {
		// An odd floor would spend its last row on the top half of a session: a
		// name with no repo/branch under it, and a row the user can see but not
		// scroll into. Every other list height may be odd — a fleet header costs
		// a row, so it usually is — but this one we choose outright.
		//
		// Nothing else pins it. Every geometry expectation below derives FROM
		// minListLines, so putting it back to 5 moves all of them with it and
		// the suite stays green.
		if got := minListLines % sessionRowHeight; got != 0 {
			t.Errorf("minListLines(%d) %% sessionRowHeight(%d) = %d, want 0", minListLines, sessionRowHeight, got)
		}
	})

	t.Run("detail pane appears at the height threshold", func(t *testing.T) {
		// One row short of the threshold the detail pane goes away entirely and
		// the list takes every row it leaves behind, rather than the pane
		// shrinking by one.
		shortContent := detailPaneHeightThreshold - 1 - helpChromeLines // 15
		tests := []struct {
			height        int
			wantDetail    int
			wantListLines int
		}{
			{height: detailPaneHeightThreshold - 1, wantDetail: 0, wantListLines: shortContent - listHeaderLines},
			{height: detailPaneHeightThreshold, wantDetail: detailPaneLines, wantListLines: minListLines},
			{height: detailPaneHeightThreshold + 1, wantDetail: detailPaneLines, wantListLines: minListLines + 1},
		}
		for _, tt := range tests {
			m := cardListModel(3)
			m.height = tt.height
			if got := m.detailLines(); got != tt.wantDetail {
				t.Errorf("height %d: detailLines() = %d, want %d", tt.height, got, tt.wantDetail)
			}
			if got := m.listAreaLines(); got != tt.wantListLines {
				t.Errorf("height %d: listAreaLines() = %d, want %d", tt.height, got, tt.wantListLines)
			}
		}
	})

	t.Run("notices shrink the same budget", func(t *testing.T) {
		// An error notice costs 2 rows, so the threshold height plus those two
		// lands on exactly the same 16-row content area as the threshold does
		// with no notice at all.
		const noticeRows = 2
		m := cardListModel(3)
		m.height = detailPaneHeightThreshold + noticeRows
		m.err = errors.New("boom")
		if got := m.listAreaLines(); got != minListLines {
			t.Errorf("listAreaLines() with an error notice = %d, want %d", got, minListLines)
		}
		m.warning = "hook not allowlisted"
		// Two notices (4 rows) drop the content area to 14: 14-2-8 = 4 < 6.
		if got := m.detailLines(); got != 0 {
			t.Errorf("detailLines() with two notices = %d, want 0", got)
		}
		// contentArea = height - helpChromeLines - 2 notices, all of which the
		// list keeps once the pane is dropped, bar the header.
		wantList := m.height - helpChromeLines - 2*noticeRows - listHeaderLines
		if got := m.listAreaLines(); got != wantList {
			t.Errorf("listAreaLines() with two notices = %d, want %d", got, wantList)
		}
	})

	t.Run("regions add up to the content area", func(t *testing.T) {
		m := cardListModel(3)
		m.height = 30
		if sum := m.headerLines() + m.listAreaLines() + m.detailLines(); sum != m.contentAreaLines() {
			t.Errorf("header+list+detail = %d, want contentAreaLines() = %d", sum, m.contentAreaLines())
		}
	})

	t.Run("empty list keeps the whole content area", func(t *testing.T) {
		m := cardListModel(0)
		m.height = 30
		if got := m.headerLines(); got != 0 {
			t.Errorf("headerLines() with no sessions = %d, want 0", got)
		}
		if got := m.detailLines(); got != 0 {
			t.Errorf("detailLines() with no sessions = %d, want 0", got)
		}
		if got := m.listAreaLines(); got != m.contentAreaLines() {
			t.Errorf("listAreaLines() with no sessions = %d, want %d", got, m.contentAreaLines())
		}
	})

	t.Run("out-of-range cursor drops the detail pane", func(t *testing.T) {
		// The pane describes the session under the cursor; with no such
		// session there is nothing to draw, so the list gets the rows.
		m := cardListModel(3)
		m.height = 30
		m.cursor = len(m.sessions)
		if m.detailVisible() {
			t.Error("detailVisible() = true with an out-of-range cursor, want false")
		}
		if got := m.listAreaLines(); got != m.contentAreaLines()-listHeaderLines {
			t.Errorf("listAreaLines() = %d, want %d", got, m.contentAreaLines()-listHeaderLines)
		}
	})

	t.Run("never reports a zero-height list", func(t *testing.T) {
		for _, h := range []int{0, 1, 2, 3} {
			m := cardListModel(3)
			m.height = h
			if got := m.listAreaLines(); got < 1 {
				t.Errorf("height %d: listAreaLines() = %d, want >= 1", h, got)
			}
		}
	})
}

// --- model fixtures ---

// plainModel is the minimal Model the renderers need: no optimistic deletes
// pending. deletingIDs is the only map they read, and every render helper is a
// Model method so its answers can agree with the rows drawn beneath it (see
// effectiveStatus).
func plainModel() Model {
	return Model{deletingIDs: map[string]bool{}}
}

// cardListModel builds a model holding n session rows. Every session renders as
// exactly sessionRowHeight (2) lines, and the rendered list is one fleet header
// row followed by the rows, so session i sits on lines 1+2i and 2+2i and the
// whole list spans 1+2n lines.
//
// Height 18+2: the pane holds 18 rows, of which the list header takes 2 and the
// detail pane 8, leaving listAreaLines() = 8 — above minListLines, so these
// models exercise the full three-region layout, and small enough to watch the
// viewport move. Pane rows map to regions as:
//
//	rows 0..1    list header      (noticeLines() + headerLines() = 2)
//	rows 2..9    list area        (listAreaLines() = 8)
//	rows 10..17  detail pane      (detailPaneLines = 8)
//
// Inside the list area row 2 is the fleet header, so session i occupies pane
// rows 3+2i and 4+2i.
//
// Every geometry test below is calibrated to those numbers: if the row height or
// the region budget changes, the hand-computed constants move together.
func cardListModel(n int) Model {
	m := plainModel()
	m.sessions = make([]session.Info, n)
	for i := range m.sessions {
		m.sessions[i] = session.Info{ID: string(rune('0' + i)), Description: "s"}
	}
	m.height = 20 // contentAreaLines() → 18; 18-2-8 = 8 list rows
	return m
}

// --- viewport scroll ---

// TestAdjustScrollForCursor verifies the viewport follows the cursor.
func TestAdjustScrollForCursor(t *testing.T) {
	newModel := func(cursor int) *Model {
		m := cardListModel(10)
		m.cursor = cursor
		return &m
	}

	t.Run("cursor on first card keeps scroll at 0", func(t *testing.T) {
		m := newModel(0)
		m.adjustScrollForCursor()
		if m.scrollOffset != 0 {
			t.Errorf("scrollOffset = %d, want 0 (cursor on first card)", m.scrollOffset)
		}
	})

	t.Run("cursor below viewport scrolls down", func(t *testing.T) {
		m := newModel(7)
		// Session 7's rows start at 1 (fleet header) + 7*2 = 15 and end at 17,
		// against a viewport of 8. The scroll is anchored on the row's bottom:
		// scrollOffset = 17 - 8 = 9.
		const bottom = 1 + 8*sessionRowHeight
		m.adjustScrollForCursor()
		if want := bottom - m.listAreaLines(); m.scrollOffset != want {
			t.Errorf("scrollOffset = %d, want %d (cursor below fold)", m.scrollOffset, want)
		}
	})

	t.Run("cursor at end anchors scroll to last visible page", func(t *testing.T) {
		m := newModel(9)
		m.adjustScrollForCursor()
		// Last row top = 1 + 9*2 = 19, height = 2, bottom = 21 = totalCardLines,
		// so the bottom-anchored offset and the clamp agree at 21 - 8 = 13.
		if want := m.totalCardLines() - m.listAreaLines(); m.scrollOffset != want {
			t.Errorf("scrollOffset = %d, want %d (cursor at end)", m.scrollOffset, want)
		}
	})

	t.Run("both rows of the cursor's session end up in view", func(t *testing.T) {
		// The reason the viewport is anchored on the row's BOTTOM: with a
		// two-row grid, stopping as soon as the first line is visible would
		// leave the cursor's session cut in half at the fold, which is exactly
		// the row the next key press acts on.
		for cursor := range 10 {
			m := newModel(cursor)
			m.adjustScrollForCursor()
			top, height := m.sessionCardTop(cursor)
			if top < m.scrollOffset || top+height > m.scrollOffset+m.listAreaLines() {
				t.Errorf("cursor %d: rows [%d,%d) are not inside the viewport [%d,%d)",
					cursor, top, top+height, m.scrollOffset, m.scrollOffset+m.listAreaLines())
			}
		}
	})

	t.Run("clampScroll bounds within [0, total-avail]", func(t *testing.T) {
		m := newModel(0)
		wantMax := m.totalCardLines() - m.listAreaLines() // 21 - 8 = 13
		m.scrollOffset = 999
		m.clampScroll()
		if m.scrollOffset != wantMax {
			t.Errorf("clampScroll from overshoot = %d, want %d", m.scrollOffset, wantMax)
		}
		m.scrollOffset = -50
		m.clampScroll()
		if m.scrollOffset != 0 {
			t.Errorf("clampScroll from negative = %d, want 0", m.scrollOffset)
		}
	})
}

// assertListOpensOnAWholeRow fails unless the first line of the list window is
// a fleet header or the FIRST line of a session — never a session's second
// line, which arrives under the header with its name scrolled away and reads as
// a session that has no name.
//
// The last page is exempt, and has to be: the list has run out of rows to align
// to there, and aligning anyway would put the final row out of reach for good.
func assertListOpensOnAWholeRow(t *testing.T, m *Model, where string) {
	t.Helper()
	if m.scrollOffset >= m.totalCardLines()-m.listAreaLines() {
		return
	}
	idx, ok := m.sessionIndexAtLine(m.scrollOffset)
	if !ok {
		return // a fleet header row, or past the last row
	}
	top, _ := m.sessionCardTop(idx)
	if top != m.scrollOffset {
		t.Fatalf("%s: scrollOffset = %d is line %d of session %d (which starts at %d) — the list opens on a metadata row",
			where, m.scrollOffset, m.scrollOffset-top+1, idx, top)
	}
}

// TestScrollBy_LandsOnWholeRows covers the retrofit that the two-line row made
// necessary: a scroll-only input lands wherever its step puts it, and half of
// those landings cut a session in two at the TOP of the viewport, where nothing
// pulls it back. adjustScrollForCursor rescues the bottom edge; the top edge
// has no such owner, because the wheel deliberately does not move the cursor.
func TestScrollBy_LandsOnWholeRows(t *testing.T) {
	models := []struct {
		name string
		m    func() Model
	}{
		{name: "one fleet", m: func() Model { return cardListModel(10) }},
		// A second fleet header shifts the parity of every row top below it,
		// which is the whole reason no step size can stay in phase with the
		// grid on its own: under one header the row tops are the odd lines,
		// under two the second group's are the even ones.
		{name: "two fleets", m: func() Model {
			m := cardListModel(10)
			for i := 5; i < len(m.sessions); i++ {
				m.sessions[i].Fleet = "beta"
			}
			return m
		}},
		// An odd list area is the case where the LAST page is itself a
		// half-drawn row: total 21 lines against a viewport of 7 puts the
		// bottom at line 14, the second line of a session. Aligning there —
		// the one place the alignment must not run — would leave the final
		// line of the list permanently below the fold.
		{name: "odd list area", m: func() Model {
			m := cardListModel(10)
			m.height = 19
			return m
		}},
	}

	for _, mc := range models {
		for _, step := range []struct {
			name  string
			lines func(*Model) int
		}{
			{name: "wheel", lines: func(*Model) int { return wheelScrollLines }},
			{name: "page", lines: func(m *Model) int { return m.pageScrollLines() }},
		} {
			t.Run(mc.name+"/"+step.name, func(t *testing.T) {
				m := mc.m()
				bottom := m.totalCardLines() - m.listAreaLines()

				// Twice as many presses as there are lines to cover, so the
				// walk ends hard against the clamp rather than near it.
				for i := 1; i <= 2*m.totalCardLines(); i++ {
					m.scrollBy(step.lines(&m))
					assertListOpensOnAWholeRow(t, &m, fmt.Sprintf("down %d", i))
				}
				// The alignment may never cost the user the end of the list —
				// the last row is where the newest sessions are.
				if m.scrollOffset != bottom {
					t.Errorf("scrolling down repeatedly stopped at %d, want %d (the last page must stay reachable)", m.scrollOffset, bottom)
				}

				for i := 1; i <= 2*m.totalCardLines(); i++ {
					m.scrollBy(-step.lines(&m))
					assertListOpensOnAWholeRow(t, &m, fmt.Sprintf("up %d", i))
				}
				if m.scrollOffset != 0 {
					t.Errorf("scrolling up repeatedly stopped at %d, want 0", m.scrollOffset)
				}
			})
		}
	}

	t.Run("a step shorter than a row still advances", func(t *testing.T) {
		// Height 6 leaves a two-row list and no detail pane, so PageDown moves
		// listAreaLines-1 = 1 line. Pulling that one line back would leave the
		// viewport where it started and the rest of the list unreachable — the
		// alignment gives way instead.
		m := cardListModel(10)
		m.height = 6
		if got := m.pageScrollLines(); got != 1 {
			t.Fatalf("pageScrollLines() = %d, want 1 — this case no longer exercises the short step", got)
		}
		bottom := m.totalCardLines() - m.listAreaLines()
		for range 2 * m.totalCardLines() {
			m.scrollBy(m.pageScrollLines())
		}
		if m.scrollOffset != bottom {
			t.Errorf("scrollOffset = %d, want %d — a one-line step must not be cancelled by the alignment", m.scrollOffset, bottom)
		}
	})

	t.Run("cursor-driven scrolling is not aligned", func(t *testing.T) {
		// The boundary this alignment must not cross. adjustScrollForCursor
		// anchors on the BOTTOM of the cursor's row so both of its lines are
		// visible; pulling that offset back to a row top would push the
		// cursor's last line off the fold — the one line that must never go.
		//
		// Height 19 leaves an odd seven-row list, which is what makes the
		// bottom-anchored offset land mid-row at all: with an even list the
		// arithmetic keeps the parity of the row tops and the case never
		// arises.
		m := cardListModel(10)
		m.height = 19
		if got := m.listAreaLines(); got != 7 {
			t.Fatalf("listAreaLines() = %d, want 7 — this case no longer lands mid-row", got)
		}
		m.cursor = 4
		m.adjustScrollForCursor()

		top, height := m.sessionCardTop(m.cursor)
		if want := top + height - m.listAreaLines(); m.scrollOffset != want {
			t.Fatalf("scrollOffset = %d, want %d (the bottom of the cursor's row, unaligned)", m.scrollOffset, want)
		}
		idx, ok := m.sessionIndexAtLine(m.scrollOffset)
		if !ok {
			t.Fatalf("scrollOffset %d is not inside a session row; the case is no longer what it was written for", m.scrollOffset)
		}
		if openTop, _ := m.sessionCardTop(idx); openTop == m.scrollOffset {
			t.Fatalf("scrollOffset %d is a row top; the case is no longer what it was written for", m.scrollOffset)
		}
		if top < m.scrollOffset || top+height > m.scrollOffset+m.listAreaLines() {
			t.Errorf("cursor rows [%d,%d) are not inside the viewport [%d,%d)",
				top, top+height, m.scrollOffset, m.scrollOffset+m.listAreaLines())
		}
	})
}

// --- mouse hit-testing ---

func TestSessionIndexAtLine(t *testing.T) {
	m := cardListModel(10)
	lastTop := 1 + 9*sessionRowHeight // 19

	tests := []struct {
		name    string
		line    int
		wantIdx int
		wantOK  bool
	}{
		{name: "negative line", line: -1, wantOK: false},
		{name: "fleet header row", line: 0, wantOK: false},
		{name: "first session, first row", line: 1, wantIdx: 0, wantOK: true},
		{name: "first session, second row", line: 2, wantIdx: 0, wantOK: true},
		{name: "second session, first row", line: 3, wantIdx: 1, wantOK: true},
		{name: "last session, first row", line: lastTop, wantIdx: 9, wantOK: true},
		{name: "last session, second row", line: lastTop + 1, wantIdx: 9, wantOK: true},
		{name: "past the last row", line: lastTop + sessionRowHeight, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx, ok := m.sessionIndexAtLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("sessionIndexAtLine(%d) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if ok && idx != tt.wantIdx {
				t.Errorf("sessionIndexAtLine(%d) = %d, want %d", tt.line, idx, tt.wantIdx)
			}
		})
	}

	t.Run("every row of a session resolves to that session", func(t *testing.T) {
		// The property the two-row grid rests on: a row is a target, not a
		// glyph. Aiming at the name and aiming at the repo/branch under it are
		// the same gesture, and the fleet header between two sessions belongs to
		// neither. A grid that only answered on its first row would make half of
		// every target dead space.
		seen := map[int]int{}
		for line := range m.totalCardLines() {
			idx, ok := m.sessionIndexAtLine(line)
			if !ok {
				continue
			}
			seen[idx]++
			top, _ := m.sessionCardTop(idx)
			if line < top || line >= top+sessionRowHeight {
				t.Errorf("line %d resolved to session %d, whose rows start at %d", line, idx, top)
			}
		}
		for idx := range m.sessions {
			if seen[idx] != sessionRowHeight {
				t.Errorf("session %d owns %d lines, want %d", idx, seen[idx], sessionRowHeight)
			}
		}
	})
}

func TestSessionIndexAtRow(t *testing.T) {
	// Rows 0..1 are the list header, rows 2..9 the list area (8 rows), rows
	// 10..17 the detail pane. Inside the list area, row 2 is the fleet header,
	// so session 0 takes rows 3..4 and session 1 rows 5..6.
	newModel := func() *Model {
		m := cardListModel(10)
		return &m
	}

	t.Run("row maps straight through at scroll 0", func(t *testing.T) {
		m := newModel()
		for _, y := range []int{5, 6} {
			if idx, ok := m.sessionIndexAtRow(y); !ok || idx != 1 {
				t.Errorf("sessionIndexAtRow(%d) = (%d, %v), want (1, true)", y, idx, ok)
			}
		}
	})

	t.Run("row in the list header misses", func(t *testing.T) {
		m := newModel()
		for _, y := range []int{0, 1} {
			if _, ok := m.sessionIndexAtRow(y); ok {
				t.Errorf("sessionIndexAtRow(%d) should miss: rows 0..1 are the list header", y)
			}
		}
	})

	t.Run("row in the detail pane misses", func(t *testing.T) {
		m := newModel()
		firstDetailRow := m.noticeLines() + m.headerLines() + m.listAreaLines() // 10
		if _, ok := m.sessionIndexAtRow(firstDetailRow); ok {
			t.Errorf("sessionIndexAtRow(%d) should miss: the list area ends at row %d",
				firstDetailRow, firstDetailRow-1)
		}
	})

	t.Run("scroll offset shifts the mapping", func(t *testing.T) {
		m := newModel()
		m.scrollOffset = 1 + 3*sessionRowHeight // the top of session 3
		// Row 2 (first list row) now shows session 3's first line, and row 3
		// its second.
		for _, y := range []int{2, 3} {
			if idx, ok := m.sessionIndexAtRow(y); !ok || idx != 3 {
				t.Errorf("sessionIndexAtRow(%d) at offset %d = (%d, %v), want (3, true)",
					y, m.scrollOffset, idx, ok)
			}
		}
	})

	t.Run("error notice pushes the list area down", func(t *testing.T) {
		m := newModel()
		m.err = errors.New("boom")
		// Rows 0..1 are the error line + spacer, rows 2..3 the list header,
		// row 4 the fleet header, so session 0 takes rows 5..6.
		for _, y := range []int{1, 3, 4} {
			if _, ok := m.sessionIndexAtRow(y); ok {
				t.Errorf("sessionIndexAtRow(%d) should miss while an error notice is shown", y)
			}
		}
		for _, y := range []int{5, 6} {
			if idx, ok := m.sessionIndexAtRow(y); !ok || idx != 0 {
				t.Errorf("sessionIndexAtRow(%d) with error = (%d, %v), want (0, true)", y, idx, ok)
			}
		}
	})

	t.Run("both rows of a session hit it, whatever is above the list", func(t *testing.T) {
		// The same property TestSessionIndexAtLine pins, but through the
		// coordinate the mouse actually reports — which is where the notice
		// band and the scroll offset get their chance to shift the grid by one
		// and turn every second click into a hit on the neighbouring session.
		for _, notice := range []bool{false, true} {
			for _, scroll := range []int{0, 1, 5, 13} {
				m := newModel()
				if notice {
					m.warning = "hook not allowlisted"
				}
				m.scrollOffset = scroll
				m.clampScroll()
				label := fmt.Sprintf("notice=%v scroll=%d", notice, m.scrollOffset)

				listTop := m.noticeLines() + m.headerLines()
				for idx := range m.sessions {
					top, _ := m.sessionCardTop(idx)
					first := listTop + top - m.scrollOffset
					for row := first; row < first+sessionRowHeight; row++ {
						if row < listTop || row >= listTop+m.listAreaLines() {
							continue // scrolled out of the viewport
						}
						got, ok := m.sessionIndexAtRow(row)
						if !ok || got != idx {
							t.Errorf("%s: sessionIndexAtRow(%d) = (%d, %v), want (%d, true) — row %d of session %d",
								label, row, got, ok, idx, row-first, idx)
						}
					}
				}
			}
		}
	})
}

// --- mouse input ---

func TestHandleMouseWheel(t *testing.T) {
	newModel := func() Model { return cardListModel(10) } // 21 lines total, 8 visible

	wheel := func(m Model, button tea.MouseButton) Model {
		got, _ := m.handleMouse(tea.MouseMsg{Y: 4, Button: button, Action: tea.MouseActionPress})
		return got.(Model)
	}

	t.Run("wheel down scrolls the viewport without moving the cursor", func(t *testing.T) {
		m := wheel(newModel(), tea.MouseButtonWheelDown)
		if m.scrollOffset != wheelScrollLines {
			t.Errorf("scrollOffset = %d, want %d", m.scrollOffset, wheelScrollLines)
		}
		if m.cursor != 0 {
			t.Errorf("cursor = %d, want 0 (wheel must not move the cursor)", m.cursor)
		}
	})

	t.Run("wheel up at the top stays clamped at 0", func(t *testing.T) {
		m := wheel(newModel(), tea.MouseButtonWheelUp)
		if m.scrollOffset != 0 {
			t.Errorf("scrollOffset = %d, want 0", m.scrollOffset)
		}
	})

	t.Run("wheel down at the bottom stays clamped", func(t *testing.T) {
		m := newModel()
		bottom := m.totalCardLines() - m.listAreaLines() // 21 - 8 = 13
		m.scrollOffset = bottom
		m = wheel(m, tea.MouseButtonWheelDown)
		if m.scrollOffset != bottom {
			t.Errorf("scrollOffset = %d, want %d (clamped at the last page)", m.scrollOffset, bottom)
		}
	})
}

func TestHandleMouseLeftClick(t *testing.T) {
	click := func(m Model, y int) Model {
		got, _ := m.handleMouse(tea.MouseMsg{Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		return got.(Model)
	}

	newModel := func() Model { return cardListModel(10) }

	// Session 1 occupies pane rows 5 and 6 (list header 2 rows, fleet header on
	// row 2, session 0 on rows 3..4).
	sessionOneRows := []int{5, 6}

	t.Run("click on either row of a session moves the cursor there", func(t *testing.T) {
		for _, y := range sessionOneRows {
			if m := click(newModel(), y); m.cursor != 1 {
				t.Errorf("click on row %d: cursor = %d, want 1", y, m.cursor)
			}
		}
	})

	t.Run("click on the fleet header leaves the cursor alone", func(t *testing.T) {
		m := newModel()
		m.cursor = 3
		if got := click(m, 2); got.cursor != 3 {
			t.Errorf("cursor = %d, want 3 (fleet header row is not selectable)", got.cursor)
		}
	})

	t.Run("click on the list header leaves the cursor alone", func(t *testing.T) {
		m := newModel()
		m.cursor = 3
		if got := click(m, 0); got.cursor != 3 {
			t.Errorf("cursor = %d, want 3 (list header rows are not selectable)", got.cursor)
		}
	})

	t.Run("click on the detail pane leaves the cursor alone", func(t *testing.T) {
		// Row 10 is the first detail pane row. It describes the cursor's
		// session; clicking it must not re-target anything.
		m := newModel()
		m.cursor = 3
		firstDetailRow := m.noticeLines() + m.headerLines() + m.listAreaLines()
		if got := click(m, firstDetailRow); got.cursor != 3 {
			t.Errorf("cursor = %d, want 3 (the detail pane is not the list)", got.cursor)
		}
	})

	t.Run("click below the last row leaves the cursor alone", func(t *testing.T) {
		m := newModel()
		m.sessions = cardListModel(1).sessions // fleet header + 2 rows = 3 lines
		m.cursor = 0
		// Row 5 is line 3, one past the only session's last row, and still
		// inside the eight-row list area.
		if got := click(m, 5); got.cursor != 0 {
			t.Errorf("cursor = %d, want 0 (empty space is not selectable)", got.cursor)
		}
	})

	t.Run("click on a deleting row is ignored", func(t *testing.T) {
		m := newModel()
		m.deletingIDs = map[string]bool{"1": true}
		for _, y := range sessionOneRows {
			if got := click(m, y); got.cursor != 0 {
				t.Errorf("click on row %d: cursor = %d, want 0 (deleting rows are not selectable)", y, got.cursor)
			}
		}
	})

	t.Run("release and drag events do not select", func(t *testing.T) {
		for _, action := range []tea.MouseAction{tea.MouseActionRelease, tea.MouseActionMotion} {
			m := newModel()
			got, _ := m.handleMouse(tea.MouseMsg{Y: 5, Button: tea.MouseButtonLeft, Action: action})
			if got.(Model).cursor != 0 {
				t.Errorf("cursor moved on %v; only press should select", action)
			}
		}
	})

	t.Run("click dismisses a transient warning", func(t *testing.T) {
		m := newModel()
		m.warning = "hook not allowlisted"
		// The warning occupies rows 0..1 and the list header rows 2..3, so
		// row 4 is the fleet header and session 0 takes rows 5..6.
		got := click(m, 5)
		if got.warning != "" {
			t.Errorf("warning = %q, want empty after a click", got.warning)
		}
		if got.cursor != 0 {
			t.Errorf("cursor = %d, want 0", got.cursor)
		}
	})
}

// TestHandleMouseLeftClick_TwoStage pins the split between looking and acting:
// the first tap on a row only moves the cursor, and only a tap on the row the
// cursor is already on reaches handleSelectSession. handleMouse has why.
//
// A session in StatusCreating is what makes the act path observable without a
// tmux client: handleSelectSession refuses it and records an error, which no
// other branch of handleMouse does.
func TestHandleMouseLeftClick_TwoStage(t *testing.T) {
	newModel := func() Model {
		m := cardListModel(3)
		for i := range m.sessions {
			m.sessions[i].Status = session.StatusCreating
		}
		m.currentSessionID = "0"
		return m
	}
	press := func(m Model, y int) (Model, tea.Cmd) {
		got, cmd := m.handleMouse(tea.MouseMsg{Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		return got.(Model), cmd
	}

	// Session 2 sits on pane rows 7 and 8; the cursor starts on session 0.
	for _, row := range []int{7, 8} {
		t.Run(fmt.Sprintf("row %d of the target", row), func(t *testing.T) {
			m := newModel()
			looked, cmd := press(m, row)
			if looked.cursor != 2 {
				t.Fatalf("first tap: cursor = %d, want 2", looked.cursor)
			}
			if cmd != nil {
				t.Errorf("first tap returned a Cmd (%T); it must only move the cursor", cmd)
			}
			if looked.err != nil {
				t.Errorf("first tap reached the selection path: err = %v", looked.err)
			}
			if looked.currentSessionID != "0" {
				t.Errorf("first tap changed the displayed session to %q", looked.currentSessionID)
			}

			acted, _ := press(looked, row)
			if acted.cursor != 2 {
				t.Errorf("second tap: cursor = %d, want it to stay on 2", acted.cursor)
			}
			if acted.err == nil {
				t.Error("second tap did not reach handleSelectSession — a tap on the cursor's own row must act")
			}
		})
	}

	t.Run("a tap that lands on nothing does not arm the next one", func(t *testing.T) {
		// The fleet header returns before the cursor moves, so the row under
		// the pointer is still not the cursor's row and the following tap on it
		// is a first tap.
		m := newModel()
		m.cursor = 1
		got, _ := press(m, 2) // the fleet header
		if got.cursor != 1 {
			t.Fatalf("cursor = %d, want 1", got.cursor)
		}
		looked, cmd := press(got, 7) // session 2's first row
		if looked.cursor != 2 || cmd != nil || looked.err != nil {
			t.Errorf("cursor = %d, cmd = %T, err = %v; want a plain first tap", looked.cursor, cmd, looked.err)
		}
	})
}

// --- convertDirHistoryEntries ---

func TestConvertDirHistoryEntries(t *testing.T) {
	now := time.Now()

	t.Run("empty input returns empty", func(t *testing.T) {
		got := convertDirHistoryEntries(nil)
		if len(got) != 0 {
			t.Errorf("convertDirHistoryEntries(nil) should return empty, got %d entries", len(got))
		}
	})

	t.Run("home prefix converted to tilde in DisplayPath", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("cannot determine home directory")
		}
		entries := []config.DirHistoryEntry{
			{Path: home + "/myproject", LastUsedAt: now},
		}

		got := convertDirHistoryEntries(entries)

		if len(got) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(got))
		}
		if got[0].Path != home+"/myproject" {
			t.Errorf("Path should remain absolute: got %q", got[0].Path)
		}
		if got[0].DisplayPath != "~/myproject" {
			t.Errorf("DisplayPath = %q, want %q", got[0].DisplayPath, "~/myproject")
		}
	})

	t.Run("preserves LastUsedAt", func(t *testing.T) {
		entries := []config.DirHistoryEntry{
			{Path: "/a", LastUsedAt: now},
		}

		got := convertDirHistoryEntries(entries)
		if !got[0].LastUsedAt.Equal(now) {
			t.Errorf("LastUsedAt not preserved")
		}
	})
}

// --- groupSessionsByFleet ---

func TestGroupSessionsByFleet(t *testing.T) {
	now := time.Now()

	t.Run("groups by fleet name", func(t *testing.T) {
		sessions := []session.Info{
			{ID: "1", Description: "s1", Fleet: "backend", CreatedAt: now},
			{ID: "2", Description: "s2", Fleet: "frontend", CreatedAt: now},
			{ID: "3", Description: "s3", Fleet: "backend", CreatedAt: now.Add(time.Minute)},
		}

		groups := groupSessionsByFleet(sessions)
		if len(groups) != 2 {
			t.Fatalf("expected 2 groups, got %d", len(groups))
		}
		if groups[0].Name != "backend" {
			t.Errorf("first group: got %q, want %q", groups[0].Name, "backend")
		}
		if len(groups[0].Sessions) != 2 {
			t.Errorf("backend group: got %d sessions, want 2", len(groups[0].Sessions))
		}
		if groups[1].Name != "frontend" {
			t.Errorf("second group: got %q, want %q", groups[1].Name, "frontend")
		}
	})

	t.Run("default fleet is last", func(t *testing.T) {
		sessions := []session.Info{
			{ID: "1", Description: "s1", Fleet: session.DefaultFleet, CreatedAt: now},
			{ID: "2", Description: "s2", Fleet: "alpha", CreatedAt: now},
			{ID: "3", Description: "s3", Fleet: "beta", CreatedAt: now},
		}

		groups := groupSessionsByFleet(sessions)
		if len(groups) != 3 {
			t.Fatalf("expected 3 groups, got %d", len(groups))
		}
		if groups[0].Name != "alpha" {
			t.Errorf("first group: got %q, want %q", groups[0].Name, "alpha")
		}
		if groups[1].Name != "beta" {
			t.Errorf("second group: got %q, want %q", groups[1].Name, "beta")
		}
		if groups[2].Name != session.DefaultFleet {
			t.Errorf("last group: got %q, want %q", groups[2].Name, session.DefaultFleet)
		}
	})

	t.Run("sessions with default fleet group correctly", func(t *testing.T) {
		sessions := []session.Info{
			{ID: "1", Description: "s1", Fleet: session.DefaultFleet, CreatedAt: now},
			{ID: "2", Description: "s2", Fleet: session.DefaultFleet, CreatedAt: now},
		}

		groups := groupSessionsByFleet(sessions)
		if len(groups) != 1 {
			t.Fatalf("expected 1 group, got %d", len(groups))
		}
		if groups[0].Name != session.DefaultFleet {
			t.Errorf("group name: got %q, want %q", groups[0].Name, session.DefaultFleet)
		}
		if len(groups[0].Sessions) != 2 {
			t.Errorf("group sessions: got %d, want 2", len(groups[0].Sessions))
		}
	})

	t.Run("single fleet still returns one group", func(t *testing.T) {
		sessions := []session.Info{
			{ID: "1", Description: "s1", Fleet: "only", CreatedAt: now},
		}

		groups := groupSessionsByFleet(sessions)
		if len(groups) != 1 {
			t.Fatalf("expected 1 group, got %d", len(groups))
		}
	})

	t.Run("empty sessions", func(t *testing.T) {
		groups := groupSessionsByFleet(nil)
		if len(groups) != 0 {
			t.Errorf("expected 0 groups, got %d", len(groups))
		}
	})
}

// --- deleting sessions are not actionable, whichever source says so ---

// TestIsDeleting_BothSources pins the fix for a split this branch introduced.
// Adding StatusDeleting to getStatusDisplay made a daemon-reported delete
// render dim with the ⟳ icon, but every guard on the action paths still read
// only m.deletingIDs — this TUI's own optimistic mark. A session the daemon
// reports as deleting therefore looked like it was going away while still
// answering to Enter, VS Code and the mouse.
//
// Both sources are reachable with an empty deletingIDs map: another client
// issued the delete, or this TUI restarted while one was in flight (the map is
// in-memory only, so a restart forgets it while the daemon keeps reporting).
func TestIsDeleting_BothSources(t *testing.T) {
	sessions := []session.Info{
		{ID: "live", Description: "live", Status: session.StatusIdle, Fleet: session.DefaultFleet},
		{ID: "optimistic", Description: "optimistic", Status: session.StatusIdle, Fleet: session.DefaultFleet},
		{ID: "reported", Description: "reported", Status: session.StatusDeleting, Fleet: session.DefaultFleet},
	}
	newModel := func() Model {
		m := plainModel()
		m.sessions = sessions
		m.height = 16
		m.width = 40
		m.deletingIDs = map[string]bool{"optimistic": true}
		return m
	}

	t.Run("isDeleting sees both the mark and the reported status", func(t *testing.T) {
		m := newModel()
		for _, tc := range []struct {
			idx  int
			want bool
		}{{0, false}, {1, true}, {2, true}} {
			if got := m.isDeleting(sessions[tc.idx]); got != tc.want {
				t.Errorf("isDeleting(%q) = %v, want %v", sessions[tc.idx].ID, got, tc.want)
			}
		}
	})

	t.Run("sessionAt refuses every deleting session", func(t *testing.T) {
		m := newModel()
		if _, ok := m.sessionAt(0); !ok {
			t.Error("sessionAt(0) refused a live session")
		}
		for _, idx := range []int{1, 2} {
			if got, ok := m.sessionAt(idx); ok {
				t.Errorf("sessionAt(%d) returned %q; a session on its way out is not actionable", idx, got.ID)
			}
		}
	})

	t.Run("select and vscode are no-ops on a daemon-reported delete", func(t *testing.T) {
		// Index 2 is the case the old guard missed entirely. With no tmux
		// client wired these would be no-ops anyway, so the assertion that
		// carries weight is that neither records an error and neither reaches
		// past the guard — verified by the absence of a nil-client panic.
		m := newModel()
		m.cursor = 2

		next, cmd := m.handleSelectSession()
		if cmd != nil {
			t.Errorf("handleSelectSession returned a Cmd for a deleting session: %T", cmd)
		}
		if got := next.(Model); got.err != nil {
			t.Errorf("handleSelectSession set err = %v, want a silent no-op", got.err)
		}

		next, cmd = m.handleVscode()
		if cmd != nil {
			t.Errorf("handleVscode returned a Cmd for a deleting session: %T", cmd)
		}
		if got := next.(Model); got.err != nil {
			t.Errorf("handleVscode set err = %v, want a silent no-op", got.err)
		}
	})

	t.Run("the cursor skips a daemon-reported delete", func(t *testing.T) {
		// deletingIDs is empty here: only the reported status can move the
		// cursor, which is exactly what the old code could not do.
		m := plainModel()
		m.sessions = []session.Info{
			{ID: "a", Description: "a", Status: session.StatusIdle},
			{ID: "b", Description: "b", Status: session.StatusDeleting},
			{ID: "c", Description: "c", Status: session.StatusIdle},
		}
		m.height = 100
		m.cursor = 1
		m.skipDeletingSessions(1)
		if m.cursor != 2 {
			t.Errorf("cursor = %d, want 2 — the cursor must not rest on a session being removed", m.cursor)
		}
	})

	t.Run("a mouse click on a deleting row selects nothing", func(t *testing.T) {
		m := newModel()
		// Row 2 of the pane is the first list row (notices 0 + header 2), and
		// the fleet header takes it, so the sessions start at row 3.
		listTop := m.noticeLines() + m.headerLines()
		for idx, wantOK := range map[int]bool{0: true, 1: false, 2: false} {
			top, _ := m.sessionCardTop(idx)
			row := listTop + top - m.scrollOffset
			gotIdx, ok := m.sessionIndexAtRow(row)
			if !ok || gotIdx != idx {
				t.Fatalf("row %d does not map back to session %d (%d, %v)", row, idx, gotIdx, ok)
			}
			if _, actionable := m.sessionAt(gotIdx); actionable != wantOK {
				t.Errorf("clicking session %q: actionable = %v, want %v", m.sessions[idx].ID, actionable, wantOK)
			}
		}
	})
}

// --- skipDeletingSessions ---

func TestSkipDeletingSessions(t *testing.T) {
	makeSessions := func(ids ...string) []session.Info {
		var ss []session.Info
		for _, id := range ids {
			ss = append(ss, session.Info{ID: id, Description: id})
		}
		return ss
	}

	t.Run("no deleting sessions", func(t *testing.T) {
		m := Model{
			sessions:    makeSessions("a", "b", "c"),
			cursor:      1,
			deletingIDs: make(map[string]bool),
			height:      100,
		}
		m.skipDeletingSessions(1)
		if m.cursor != 1 {
			t.Errorf("expected cursor 1, got %d", m.cursor)
		}
	})

	t.Run("skip down over deleting session", func(t *testing.T) {
		m := Model{
			sessions:    makeSessions("a", "b", "c"),
			cursor:      1,
			deletingIDs: map[string]bool{"b": true},
			height:      100,
		}
		m.skipDeletingSessions(1)
		if m.cursor != 2 {
			t.Errorf("expected cursor 2, got %d", m.cursor)
		}
	})

	t.Run("skip up over deleting session", func(t *testing.T) {
		m := Model{
			sessions:    makeSessions("a", "b", "c"),
			cursor:      1,
			deletingIDs: map[string]bool{"b": true},
			height:      100,
		}
		m.skipDeletingSessions(-1)
		if m.cursor != 0 {
			t.Errorf("expected cursor 0, got %d", m.cursor)
		}
	})

	t.Run("clamp at end and fallback to opposite direction", func(t *testing.T) {
		m := Model{
			sessions:    makeSessions("a", "b", "c"),
			cursor:      2,
			deletingIDs: map[string]bool{"c": true},
			height:      100,
		}
		m.skipDeletingSessions(1) // going down, hits end, clamp, fallback up
		if m.cursor != 1 {
			t.Errorf("expected cursor 1, got %d", m.cursor)
		}
	})

	t.Run("clamp at start and fallback to opposite direction", func(t *testing.T) {
		m := Model{
			sessions:    makeSessions("a", "b", "c"),
			cursor:      0,
			deletingIDs: map[string]bool{"a": true},
			height:      100,
		}
		m.skipDeletingSessions(-1) // going up, hits start, clamp, fallback down
		if m.cursor != 1 {
			t.Errorf("expected cursor 1, got %d", m.cursor)
		}
	})

	t.Run("all sessions deleting stays on cursor 0", func(t *testing.T) {
		m := Model{
			sessions:    makeSessions("a", "b"),
			cursor:      0,
			deletingIDs: map[string]bool{"a": true, "b": true},
			height:      100,
		}
		m.skipDeletingSessions(1)
		// All deleting: cursor stays at clamped position (transient state)
		if m.cursor < 0 || m.cursor >= 2 {
			t.Errorf("expected cursor in range [0,1], got %d", m.cursor)
		}
	})

	t.Run("multiple deleting sessions skip all", func(t *testing.T) {
		m := Model{
			sessions:    makeSessions("a", "b", "c", "d"),
			cursor:      1,
			deletingIDs: map[string]bool{"b": true, "c": true},
			height:      100,
		}
		m.skipDeletingSessions(1)
		if m.cursor != 3 {
			t.Errorf("expected cursor 3, got %d", m.cursor)
		}
	})
}

// --- Delete confirmation cursor skip ---

// TestDeleteConfirmMoveCursorToNextSession drives the answered-popup entry
// point (dispatchConfirmResult) rather than a key press, since the prompt now
// lives in its own tmux popup process. What it pins is unchanged: an approved
// delete greys out the target and slides the cursor off it, so the user is
// never left pointing at a session that is transitioning away.
func TestDeleteConfirmMoveCursorToNextSession(t *testing.T) {
	makeSessions := func(ids ...string) []session.Info {
		var ss []session.Info
		for _, id := range ids {
			ss = append(ss, session.Info{ID: id, Description: id})
		}
		return ss
	}

	t.Run("cursor moves to next session after delete confirm", func(t *testing.T) {
		m := Model{
			sessions:    makeSessions("a", "b", "c"),
			cursor:      1, // on "b"
			deletingIDs: make(map[string]bool),
			height:      100,
		}
		result, _ := m.dispatchConfirmResult(ConfirmModeDelete, "b", ConfirmResultYes)
		rm := result.(Model)
		if rm.cursor == 1 {
			t.Errorf("cursor should have moved away from deleted session, got %d", rm.cursor)
		}
		if !rm.deletingIDs["b"] {
			t.Error("session 'b' should be marked as deleting")
		}
	})

	t.Run("cursor moves up when deleting last session", func(t *testing.T) {
		m := Model{
			sessions:    makeSessions("a", "b", "c"),
			cursor:      2, // on "c" (last)
			deletingIDs: make(map[string]bool),
			height:      100,
		}
		result, _ := m.dispatchConfirmResult(ConfirmModeDelete, "c", ConfirmResultYes)
		rm := result.(Model)
		if rm.cursor != 1 {
			t.Errorf("expected cursor 1 (previous session), got %d", rm.cursor)
		}
	})
}

// --- pendingCursorRestore ---

// TestPendingCursorRestore covers the "quit-then-relaunch keeps the cursor on
// the right-pane session" flow. NewModelWithTmux arms the flag from the
// restored JIN_CURRENT_SESSION; the first sessionsMsg after startup consumes
// it and slides the cursor onto the matching row. The flag is always cleared
// after the first sessionsMsg (checked once at the bottom, not per case).
func TestPendingCursorRestore(t *testing.T) {
	msg := sessionsMsg([]session.Info{
		{ID: "a", Description: "a"},
		{ID: "b", Description: "b"},
		{ID: "c", Description: "c"},
	})
	tests := []struct {
		name             string
		cursor           int
		currentSessionID string
		pending          bool
		wantCursor       int
	}{
		{"lands on the restored right-pane session", 0, "b", true, 1},
		{"flag clears when the restored session is gone", 0, "gone", true, 0},
		{"consumed flag does not clobber user cursor movement", 2, "b", false, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				cursor:               tt.cursor,
				deletingIDs:          make(map[string]bool),
				height:               100,
				currentSessionID:     tt.currentSessionID,
				pendingCursorRestore: tt.pending,
			}
			result, _ := m.updateListMode(msg)
			rm := result.(Model)
			if rm.cursor != tt.wantCursor {
				t.Errorf("cursor = %d, want %d", rm.cursor, tt.wantCursor)
			}
			if rm.pendingCursorRestore {
				t.Error("pendingCursorRestore should be cleared after sessionsMsg")
			}
		})
	}
}

// --- list header ---

// TestStatusCounts pins the three properties the header depends on: urgency
// order (PERMISSION first, so a narrow pane drops the least urgent groups),
// omission of empty statuses, and one trailing bucket for everything the
// display vocabulary does not know.
func TestStatusCounts(t *testing.T) {
	t.Run("urgency order regardless of input order", func(t *testing.T) {
		// Deliberately scrambled, and IDLE appears twice to prove counting.
		sessions := []session.Info{
			{ID: "1", Status: session.StatusIdle},
			{ID: "2", Status: session.StatusDeleting},
			{ID: "3", Status: session.StatusThinking},
			{ID: "4", Status: session.StatusStopped},
			{ID: "5", Status: session.StatusIdle},
			{ID: "6", Status: session.StatusCreating},
			{ID: "7", Status: session.StatusPermission},
			{ID: "8", Status: session.StatusRunning},
		}
		want := []statusCount{
			{session.StatusPermission, 1},
			{session.StatusThinking, 1},
			{session.StatusRunning, 1},
			{session.StatusCreating, 1},
			{session.StatusIdle, 2},
			{session.StatusStopped, 1},
			{session.StatusDeleting, 1},
		}
		got := plainModel().statusCounts(sessions)
		if len(got) != len(want) {
			t.Fatalf("statusCounts() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("statusCounts()[%d] = %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("statuses with no sessions are omitted", func(t *testing.T) {
		sessions := []session.Info{
			{ID: "1", Status: session.StatusIdle},
			{ID: "2", Status: session.StatusIdle},
		}
		got := plainModel().statusCounts(sessions)
		want := []statusCount{{session.StatusIdle, 2}}
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("statusCounts() = %v, want %v", got, want)
		}
	})

	t.Run("no sessions yields no groups", func(t *testing.T) {
		if got := plainModel().statusCounts(nil); len(got) != 0 {
			t.Errorf("statusCounts(nil) = %v, want empty", got)
		}
	})

	t.Run("unrecognised statuses collapse into one trailing bucket", func(t *testing.T) {
		sessions := []session.Info{
			{ID: "1", Status: session.Status("zombie")},
			{ID: "2", Status: session.Status("")},
			{ID: "3", Status: session.StatusIdle},
		}
		got := plainModel().statusCounts(sessions)
		if len(got) != 2 {
			t.Fatalf("statusCounts() = %v, want 2 groups (idle + unknown bucket)", got)
		}
		if got[0].Status != session.StatusIdle {
			t.Errorf("first group = %v, want the known status first", got[0])
		}
		bucket := got[1]
		if bucket.N != 2 {
			t.Errorf("unknown bucket N = %d, want 2 (both unrecognised statuses)", bucket.N)
		}
		// The bucket covers several distinct values, so the only thing defined
		// for its Status is how getStatusDisplay renders it.
		if _, label, _ := getStatusDisplay(bucket.Status); label != "UNKNOWN" {
			t.Errorf("unknown bucket renders as %q, want UNKNOWN", label)
		}
	})
}

// TestRenderListHeader covers the count line: its shape at a comfortable
// width, the singular wording, the separator that divides "how many" from "of
// what", and what survives when the pane is too narrow to hold every group.
func TestRenderListHeader(t *testing.T) {
	sessions := func(perStatus map[session.Status]int) []session.Info {
		var out []session.Info
		for _, status := range []session.Status{
			session.StatusPermission, session.StatusThinking, session.StatusRunning,
			session.StatusCreating, session.StatusIdle,
		} {
			for i := 0; i < perStatus[status]; i++ {
				out = append(out, session.Info{ID: fmt.Sprintf("%s-%d", status, i), Status: status})
			}
		}
		return out
	}

	t.Run("total plus one group per non-empty status", func(t *testing.T) {
		got := plainModel().renderListHeader(sessions(map[session.Status]int{
			session.StatusPermission: 1,
			session.StatusThinking:   2,
			session.StatusIdle:       4,
		}), 40)
		for _, want := range []string{"7 SESSIONS", "  /  ", "? 1", "⚡ 2", "○ 4"} {
			if !strings.Contains(got, want) {
				t.Errorf("renderListHeader() = %q, want it to contain %q", got, want)
			}
		}
		if w := lipgloss.Width(got); w > 40 {
			t.Errorf("rendered width = %d, want <= 40: %q", w, got)
		}
	})

	t.Run("singular total", func(t *testing.T) {
		got := plainModel().renderListHeader(sessions(map[session.Status]int{session.StatusIdle: 1}), 40)
		if !strings.Contains(got, "1 SESSION") || strings.Contains(got, "1 SESSIONS") {
			t.Errorf("renderListHeader() = %q, want the singular %q", got, "1 SESSION")
		}
	})

	t.Run("narrow pane drops groups from the least urgent end", func(t *testing.T) {
		all := sessions(map[session.Status]int{
			session.StatusPermission: 1,
			session.StatusThinking:   2,
			session.StatusIdle:       4,
		})
		// "7 SESSIONS" (10) + separator 5 + "? 1" (3) = 18 fits exactly; the
		// next group would need 3 + 4 = 7 more.
		const width = 18
		got := plainModel().renderListHeader(all, width)
		if w := lipgloss.Width(got); w > width {
			t.Fatalf("rendered width = %d, want <= %d: %q", w, width, got)
		}
		// The total is never dropped: it is the one number always worth the
		// space.
		if !strings.Contains(got, "7 SESSIONS") {
			t.Errorf("renderListHeader() = %q, want the total to survive", got)
		}
		if !strings.Contains(got, "? 1") {
			t.Errorf("renderListHeader() = %q, want PERMISSION to survive as the most urgent group", got)
		}
		for _, dropped := range []string{"⚡", "○"} {
			if strings.Contains(got, dropped) {
				t.Errorf("renderListHeader() = %q, want the less urgent %q dropped", got, dropped)
			}
		}
	})

	t.Run("a width that fits nothing but the total", func(t *testing.T) {
		got := plainModel().renderListHeader(sessions(map[session.Status]int{session.StatusPermission: 3}), 10)
		if got != helpStyle.Render("3 SESSIONS") {
			t.Errorf("renderListHeader() = %q, want the bare total", got)
		}
	})

	t.Run("the separator goes with the group it introduces", func(t *testing.T) {
		// The separator is charged to the FIRST group rather than printed
		// unconditionally, so the two cannot come apart: a pane one column too
		// narrow for "? 3" ends at the total instead of trailing a divider with
		// nothing after it, which reads as a header that lost its contents.
		all := sessions(map[session.Status]int{session.StatusPermission: 3})
		// "3 SESSIONS" (10) + separator (5) + "? 3" (3).
		const fits = 10 + 5 + 3
		for _, width := range []int{fits, fits - 1, fits - 5, 10, 3, 0} {
			got := plainModel().renderListHeader(all, width)
			wantGroup := width >= fits
			if hasGroup := strings.Contains(got, "? 3"); hasGroup != wantGroup {
				t.Errorf("width %d: group present = %v, want %v: %q", width, hasGroup, wantGroup, got)
			}
			if hasSep := strings.Contains(got, "/"); hasSep != wantGroup {
				t.Errorf("width %d: separator present = %v, want %v: %q", width, hasSep, wantGroup, got)
			}
		}
	})

	t.Run("no width overflows, except by the total it will not drop", func(t *testing.T) {
		// The line is built by accumulating widths, and the lead in front of a
		// group is 5 columns for the first ("  /  ") and 3 for the rest. Charge
		// the wrong one and the header outgrows the pane at some widths and not
		// others — which is why this sweeps rather than sampling.
		//
		// Overflow here is not a cosmetic ragged edge: a header one column too
		// wide wraps, listHeaderLines(2) becomes 3 in fact but not in the
		// arithmetic, the whole list shifts down a row, and every
		// sessionIndexAtRow answer below it is off by one.
		all := sessions(map[session.Status]int{
			session.StatusPermission: 3,
			session.StatusThinking:   4,
			session.StatusRunning:    2,
			session.StatusCreating:   2,
			session.StatusIdle:       4,
		})
		// The total is the one thing renderListHeader never drops, so below its
		// own width there is no answer that fits and overflowing is correct.
		floor := lipgloss.Width(helpStyle.Render(fmt.Sprintf("%d SESSIONS", len(all))))
		for width := 0; width <= 60; width++ {
			got := plainModel().renderListHeader(all, width)
			if w := lipgloss.Width(got); w > max(width, floor) {
				t.Errorf("width %d: header is %d columns, want <= %d: %q", width, w, max(width, floor), stripANSI(got))
			}
		}
	})
}

// TestSessionsMsg_ClampsCursorOnEveryPath pins the invariant renderListContent
// relies on: after a session list update the cursor indexes a real session.
// The detail pane's subject is picked with displaySessions[m.cursor], so an
// out-of-range cursor is a panic, not a blank pane. The pending-focus branch
// returns early, so it has to be clamped before that fork, not after.
func TestSessionsMsg_ClampsCursorOnEveryPath(t *testing.T) {
	sessions := func(n int) []session.Info {
		out := make([]session.Info, n)
		for i := range out {
			out[i] = session.Info{
				ID:          fmt.Sprintf("id-%d", i),
				Description: fmt.Sprintf("s%d", i),
				Status:      session.StatusIdle,
				Fleet:       session.DefaultFleet,
			}
		}
		return out
	}

	cases := []struct {
		name           string
		focusSessionID string
	}{
		{name: "plain update"},
		{name: "pending focus that cannot be resolved", focusSessionID: "ghost-id"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, remaining := range []int{0, 1, 3} {
				m := Model{
					sessions: sessions(9),
					cursor:   8, // valid before the shrink, past the end after
					// Tall enough to draw the detail pane, and that is the whole
					// point: renderListContent indexes the session slice with
					// m.cursor to pick the pane's subject, so the out-of-range
					// cursor this test manufactures only reaches a panic while
					// the pane is visible. Below the threshold the renderer
					// survives for the uninteresting reason that it never looks.
					height:         20,
					width:          40,
					deletingIDs:    map[string]bool{},
					focusSessionID: tc.focusSessionID,
				}
				next, _ := m.Update(sessionsMsg(sessions(remaining)))
				got := next.(Model)

				if got.cursor < 0 || (remaining > 0 && got.cursor >= remaining) {
					t.Fatalf("remaining=%d: cursor = %d, out of range", remaining, got.cursor)
				}
				// The renderer must survive the same frame.
				_ = got.renderListContent(38)
			}
		})
	}
}

// --- one frame, one answer ---

// TestEffectiveStatus_OneAnswerPerFrame pins the agreement between the three
// places that draw a session's status. m.deletingIDs is the TUI's own
// optimistic mark, set the moment a delete is accepted and before the daemon
// reports StatusDeleting, so any renderer that reads sess.Status directly
// disagrees with its neighbours for a whole poll interval.
func TestEffectiveStatus_OneAnswerPerFrame(t *testing.T) {
	sessions := []session.Info{
		{ID: "going", Description: "going-away", Status: session.StatusIdle, Fleet: session.DefaultFleet},
		{ID: "staying", Description: "staying-put", Status: session.StatusIdle, Fleet: session.DefaultFleet},
	}
	m := Model{
		sessions:    sessions,
		height:      16,
		width:       40,
		deletingIDs: map[string]bool{"going": true},
	}

	t.Run("the header counts the optimistic delete", func(t *testing.T) {
		got := m.statusCounts(sessions)
		want := []statusCount{{session.StatusIdle, 1}, {session.StatusDeleting, 1}}
		if len(got) != len(want) {
			t.Fatalf("statusCounts() = %v, want %v — the header still counts the session the list already shows as deleting", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("statusCounts()[%d] = %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("the row, the pane and the header show the same icon", func(t *testing.T) {
		deletingIcon, _, _ := getStatusDisplay(session.StatusDeleting)

		row := m.renderSession(sessions[0], true, false, 38)
		if !strings.Contains(row, deletingIcon) {
			t.Errorf("list row = %q, want the deleting icon %q", row, deletingIcon)
		}
		m.cursor = 0
		pane := m.renderDetailPane(sessions[0], 38)
		if !strings.Contains(pane, deletingIcon) {
			t.Errorf("detail pane = %q, want the deleting icon %q", pane, deletingIcon)
		}
		if !strings.Contains(pane, "DELETING") {
			t.Errorf("detail pane = %q, want the DELETING label", pane)
		}
		header := m.renderListHeader(sessions, 38)
		if !strings.Contains(header, deletingIcon) {
			t.Errorf("header = %q, want a group for the deleting session", header)
		}
	})

	t.Run("the pane dims the name like the row does", func(t *testing.T) {
		// deletingStyle's foreground is what "dim" means here; asserting on the
		// rendered SGR keeps the test honest about what the user sees.
		want := deletingStyle.Render("going-away")
		pane := m.renderDetailPane(sessions[0], 38)
		if !strings.Contains(pane, want) {
			t.Errorf("detail pane does not dim the name of a deleting session: %q", pane)
		}
	})
}

// TestRenderDetailPane_NarrowWidths drives the pane below the width View()
// ever hands it. The status line was the one line with no truncation of its
// own, relying on a clamp several functions away; a line that outgrew the pane
// would wrap and cost the fixed-height block a row.
func TestRenderDetailPane_NarrowWidths(t *testing.T) {
	m := plainModel()
	sess := session.Info{
		ID: "s", Description: "a session", Status: session.StatusPermission,
		AgentKind: "claude", RepoName: "jind-ai", CurrentBranch: "feat/x",
		LastUserMessage: "hello", LastAssistantMessage: "hi",
	}
	for width := 1; width <= 30; width++ {
		got := m.renderDetailPane(sess, width)
		if n := detailPaneLineCount(got); n != detailPaneLines {
			t.Errorf("width %d: %d lines, want %d", width, n, detailPaneLines)
		}
		for i, line := range strings.Split(got, "\n") {
			if w := lipgloss.Width(line); w > width {
				t.Errorf("width %d: line %d is %d columns: %q", width, i, w, line)
			}
		}
	}
}

// sessionRowLines splits a rendered list row into its lines, dropping the
// trailing newline every row ends with. Every caller then asserts against
// sessionRowHeight, so the invariant is stated once per test rather than
// re-derived from a newline count.
func sessionRowLines(row string) []string {
	return strings.Split(strings.TrimSuffix(row, "\n"), "\n")
}

// TestRenderSession_NarrowWidths is the list-row counterpart to
// TestRenderDetailPane_NarrowWidths: a row must stay sessionRowHeight rows and
// stay inside the pane no matter how little room there is, since
// sessionRowHeight is a constant the scroll and hit-test arithmetic is built
// on. Below sessionRowLead+1 the renderer bails out to blank rows, and it has
// to emit the same number of them — returning one breaks the invariant from the
// other side.
func TestRenderSession_NarrowWidths(t *testing.T) {
	m := Model{deletingIDs: map[string]bool{"del": true}}
	cases := []session.Info{
		{ID: "s", Description: "a session", Status: session.StatusThinking, RepoName: "jind-ai", CurrentBranch: "feat/x"},
		{ID: "s", Description: strings.Repeat("全角", 20), Status: session.StatusPermission, DescriptionLocked: true,
			RepoName: strings.Repeat("リポジトリ", 4), CurrentBranch: strings.Repeat("ブランチ", 4)},
		{ID: "del", Description: "going away", Status: session.StatusIdle, WorkDir: "/var/tmp/deeply/nested/place"},
		{ID: "s", Status: session.StatusIdle},
	}
	for _, sess := range cases {
		for width := 1; width <= 30; width++ {
			for _, selected := range []bool{false, true} {
				got := m.renderSession(sess, selected, true, width)
				if n := strings.Count(got, "\n"); n != sessionRowHeight {
					t.Fatalf("width %d selected=%v: %d newlines, want %d: %q", width, selected, n, sessionRowHeight, got)
				}
				for i, line := range sessionRowLines(got) {
					if w := lipgloss.Width(line); w > width {
						t.Errorf("width %d selected=%v: line %d is %d columns: %q", width, selected, i, w, line)
					}
				}
			}
		}
	}
}

// --- one ruler ---

// TestOneRuler_TruncationBoundsRenderedWidth is the regression for the defect
// that made the whole row geometry a lie: strings were cut with go-runewidth but
// the composed line was measured with lipgloss, and the two disagree in two ways
// that both reach production — see the ruler note at the foot of model.go and
// the init in styles.go.
//
// Asserting through lipgloss.Width is the point: it is the ruler that decides
// whether a line fits the terminal, so it must be the ruler that bounds it.
func TestOneRuler_TruncationBoundsRenderedWidth(t *testing.T) {
	inputs := []struct {
		name string
		s    string
	}{
		{"VS16 emoji run", strings.Repeat("✔️", 40)},
		{"VS16 emoji mixed with ASCII", strings.Repeat("done ✔️ ", 20)},
		{"warning sign", strings.Repeat("⚠️ alert ", 20)},
		{"locale-ambiguous glyphs", strings.Repeat("○■▶·", 30)},
		{"full-width CJK", strings.Repeat("全角文字列", 30)},
		{"CJK mixed with ASCII", strings.Repeat("全角abc", 30)},
		{"plain ASCII", strings.Repeat("plain-ascii-", 30)},
	}

	for _, in := range inputs {
		t.Run(in.name, func(t *testing.T) {
			for width := 1; width <= 40; width++ {
				if got := truncateString(in.s, width); lipgloss.Width(got) > width {
					t.Errorf("truncateString(%s, %d) is %d columns wide: %q",
						in.name, width, lipgloss.Width(got), got)
				}
				if got := truncateStringFromEnd(in.s, width); lipgloss.Width(got) > width {
					t.Errorf("truncateStringFromEnd(%s, %d) is %d columns wide: %q",
						in.name, width, lipgloss.Width(got), got)
				}
			}
		})
	}
}

// TestOneRuler_RowAndPaneSurviveWideGlyphs drives the two fixed-height
// renderers with the same hostile strings, since a line that overflows there
// wraps in the terminal and silently adds a physical row — breaking
// sessionRowHeight / detailPaneLines and every offset computed from them.
func TestOneRuler_RowAndPaneSurviveWideGlyphs(t *testing.T) {
	hostile := []string{
		strings.Repeat("✔️", 40),
		strings.Repeat("⚠️ ", 30),
		strings.Repeat("○■▶", 30),
		strings.Repeat("絵文字と全角", 20),
	}

	m := plainModel()
	for _, s := range hostile {
		sess := session.Info{
			ID: "s", Description: s, Status: session.StatusIdle, AgentKind: s,
			RepoName: s, CurrentBranch: s, LastUserMessage: s, LastAssistantMessage: s,
		}
		for _, width := range []int{minTUIWidth - 2, 28, 38, maxTUIWidth - 2} {
			row := m.renderSession(sess, true, true, width)
			for i, line := range sessionRowLines(row) {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("renderSession at width %d: line %d is %d columns: %q", width, i, w, line)
				}
			}
			if n := strings.Count(row, "\n"); n != sessionRowHeight {
				t.Errorf("renderSession at width %d produced %d newlines, want %d", width, n, sessionRowHeight)
			}

			pane := m.renderDetailPane(sess, width)
			if n := detailPaneLineCount(pane); n != detailPaneLines {
				t.Errorf("renderDetailPane at width %d produced %d lines, want %d", width, n, detailPaneLines)
			}
			for i, line := range strings.Split(pane, "\n") {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("renderDetailPane at width %d: line %d is %d columns: %q", width, i, w, line)
				}
			}
		}
	}
}

// --- renderDetailPane ---

// detailPaneLineCount splits a rendered detail pane into its lines.
func detailPaneLineCount(s string) int {
	return len(strings.Split(s, "\n"))
}

// TestRenderDetailPane_FixedHeight is the load-bearing test for the detail
// pane: listAreaLines(), every scroll clamp and every mouse hit-test subtract
// detailPaneLines as a constant, so a pane that grew or shrank with its
// session's contents would silently move the list out from under the cursor.
func TestRenderDetailPane_FixedHeight(t *testing.T) {
	m := plainModel()
	const width = 38

	long := strings.Repeat("absurdly-long-value-", 30)

	tests := []struct {
		name string
		sess session.Info
	}{
		{
			name: "fully populated",
			sess: session.Info{
				ID: "s", Description: "plugin registry の crawler を実装して",
				Status: session.StatusThinking, AgentKind: "claude",
				RepoName: "jind-ai", CurrentBranch: "feat/registry",
				LastUserMessage: "次の task を進めて", LastAssistantMessage: "name 衝突ルールを整理しました",
			},
		},
		{name: "entirely empty", sess: session.Info{ID: "s"}},
		{name: "no messages", sess: session.Info{ID: "s", Description: "d", Status: session.StatusIdle, RepoName: "r", CurrentBranch: "b"}},
		{name: "no branch", sess: session.Info{ID: "s", Description: "d", Status: session.StatusIdle, RepoName: "r"}},
		{name: "no repo, falls back to workdir", sess: session.Info{ID: "s", Description: "d", Status: session.StatusIdle, WorkDir: "/tmp/somewhere", CurrentBranch: "b"}},
		{name: "no repo and no branch", sess: session.Info{ID: "s", Description: "d", Status: session.StatusIdle, WorkDir: "/tmp/somewhere"}},
		{name: "locked description", sess: session.Info{ID: "s", Description: "d", DescriptionLocked: true, Status: session.StatusIdle}},
		{
			name: "absurdly long everything",
			sess: session.Info{
				ID: "s", Description: long, Status: session.StatusPermission, AgentKind: long,
				RepoName: long, CurrentBranch: long, LastUserMessage: long, LastAssistantMessage: long,
			},
		},
		{
			name: "full-width CJK everywhere",
			sess: session.Info{
				ID: "s", Description: strings.Repeat("セッション", 20), Status: session.StatusRunning,
				RepoName: strings.Repeat("リポジトリ", 10), CurrentBranch: strings.Repeat("ブランチ", 10),
				LastUserMessage: strings.Repeat("質問", 40), LastAssistantMessage: strings.Repeat("回答", 40),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.renderDetailPane(tt.sess, width)
			if n := detailPaneLineCount(got); n != detailPaneLines {
				t.Errorf("renderDetailPane() produced %d lines, want exactly %d", n, detailPaneLines)
			}
			for i, line := range strings.Split(got, "\n") {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("line %d is %d columns wide, want <= %d: %q", i, w, width, line)
				}
			}
		})
	}
}

// Row indexes inside a rendered detail pane: the rule comes first, then the
// name rows, then the status line, then the two messages of detailMsgLines rows
// each. Derived rather than hard-coded so they follow detailNameLines and
// detailMsgLines.
//
// There is no repo/branch row: it moved onto the second line of every list row,
// where TestRenderSession_BranchPriority now covers it.
const (
	detailNameRow         = 1
	detailStatusRow       = detailNameRow + detailNameLines
	detailUserMsgRow      = detailStatusRow + 1
	detailAssistantMsgRow = detailUserMsgRow + detailMsgLines
)

// --- the detail pane's session name ---

// detailRows returns n rows of a rendered detail pane starting at `first`, with
// their styling and pane indent removed, so assertions read the text the user
// sees.
func detailRows(pane string, first, n int) []string {
	rows := strings.Split(pane, "\n")[first : first+n]
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = strings.TrimPrefix(stripANSI(row), detailIndent)
	}
	return out
}

// detailNameRows returns the pane's name rows.
func detailNameRows(pane string) []string {
	return detailRows(pane, detailNameRow, detailNameLines)
}

// nameShapeSessions are the name shapes the pane's name block has to survive:
// one that fits a row, one that wraps onto the second, one that outruns both,
// full-width text, an empty name, and a locked one. Spread over two fleets, so
// the list above the pane carries fleet headers as well as session rows.
func nameShapeSessions() []session.Info {
	return []session.Info{
		{ID: "a", Description: "a", Status: session.StatusIdle, Fleet: session.DefaultFleet},
		{ID: "b", Description: strings.Repeat("medium-length-name-", 3), Status: session.StatusIdle, Fleet: session.DefaultFleet},
		{ID: "c", Description: strings.Repeat("absurdly-long-name-", 40), Status: session.StatusThinking, Fleet: "backend"},
		{ID: "d", Description: strings.Repeat("全角の長い名前", 20), Status: session.StatusPermission, Fleet: "backend"},
		{ID: "e", Description: "", Status: session.StatusIdle, Fleet: session.DefaultFleet},
		{ID: "f", Description: "short", DescriptionLocked: true, Status: session.StatusIdle, Fleet: "backend"},
	}
}

// TestRenderDetailPane_NameSpansTwoRows is the point of the pane's name block:
// it must show more of a long name than the list row above it does, and it must
// do that from a row budget that never moves.
func TestRenderDetailPane_NameSpansTwoRows(t *testing.T) {
	m := plainModel()
	const width = 38

	t.Run("a short name leaves the second row blank", func(t *testing.T) {
		sess := session.Info{ID: "s", Description: "short-name", Status: session.StatusIdle}
		rows := detailNameRows(m.renderDetailPane(sess, width))
		if rows[0] != "short-name" {
			t.Errorf("row 1 = %q, want the whole name", rows[0])
		}
		if rows[1] != "" {
			t.Errorf("row 2 = %q, want a blank pad row", rows[1])
		}
	})

	t.Run("a long name continues onto the second row", func(t *testing.T) {
		// Long enough to need a second row, short enough to fit inside two.
		sess := session.Info{
			ID:          "s",
			Description: "plugin registry の crawler で name 衝突を解決する",
			Status:      session.StatusIdle,
		}
		rows := detailNameRows(m.renderDetailPane(sess, width))
		if rows[1] == "" {
			t.Fatalf("row 2 is blank; the name should have continued onto it: %q", rows[0])
		}
		if !strings.Contains(sess.Description, rows[1]) {
			t.Errorf("row 2 = %q is not a slice of the name %q", rows[1], sess.Description)
		}
		// The whole reason for the second row: strictly more of the name than
		// the list row shows, measured with the ruler that decides what fits.
		paneName := lipgloss.Width(rows[0] + rows[1])
		listName := lipgloss.Width(sessionNameText(sess, width-sessionRowLead))
		if paneName <= listName {
			t.Errorf("the pane shows %d columns of the name (%q + %q), the list row %d — the second row bought nothing",
				paneName, rows[0], rows[1], listName)
		}
	})

	t.Run("a name past two rows is cut, never spilled onto a third", func(t *testing.T) {
		sess := session.Info{ID: "s", Description: strings.Repeat("very-long-name-", 20), Status: session.StatusIdle}
		pane := m.renderDetailPane(sess, width)
		if n := detailPaneLineCount(pane); n != detailPaneLines {
			t.Fatalf("pane is %d lines, want %d", n, detailPaneLines)
		}
		rows := detailNameRows(pane)
		if !strings.HasSuffix(rows[1], "...") {
			t.Errorf("row 2 = %q, want an ellipsis marking the cut", rows[1])
		}
		// The row after the name block belongs to the status line, not to an
		// overflowing name.
		status := stripANSI(strings.Split(pane, "\n")[detailStatusRow])
		if !strings.Contains(status, "IDLE") {
			t.Errorf("the row after the name block = %q, want the status line", status)
		}
	})

	t.Run("the lock marker rides the row the name ends on", func(t *testing.T) {
		short := session.Info{ID: "s", Description: "kept", DescriptionLocked: true, Status: session.StatusIdle}
		rows := detailNameRows(m.renderDetailPane(short, width))
		if rows[0] != "kept*" {
			t.Errorf("row 1 = %q, want the marker beside the name it belongs to", rows[0])
		}
		if rows[1] != "" {
			t.Errorf("row 2 = %q, want the marker to cost no row of its own", rows[1])
		}

		long := session.Info{
			ID: "s", Description: strings.Repeat("wrapping-name-", 10),
			DescriptionLocked: true, Status: session.StatusIdle,
		}
		rows = detailNameRows(m.renderDetailPane(long, width))
		if !strings.HasSuffix(rows[1], "*") {
			t.Errorf("row 2 = %q, want the marker on the last row the name reaches", rows[1])
		}
		avail := width - detailIndentWidth
		if w := lipgloss.Width(rows[1]); w > avail {
			t.Errorf("row 2 is %d columns with the marker, want <= %d: %q", w, avail, rows[1])
		}
	})

	t.Run("an empty name still costs its rows", func(t *testing.T) {
		rows := detailNameRows(m.renderDetailPane(session.Info{ID: "s", Status: session.StatusIdle}, width))
		if len(rows) != detailNameLines {
			t.Fatalf("got %d name rows, want %d", len(rows), detailNameLines)
		}
		for i, row := range rows {
			if row != "" {
				t.Errorf("row %d = %q, want blank", i+1, row)
			}
		}
	})
}

// msgIconColumns is the width of the "👤 " / "🤖 " lead: a two-column emoji
// plus its space. Measured with the same ruler the renderer budgets by rather
// than written as 3, since a lead one column off would put every continuation
// row out of line with the text above it.
var msgIconColumns = lipgloss.Width("👤 ")

// detailMsgRows returns one message's rows from a rendered pane, `first` being
// the row its icon is on (detailUserMsgRow or detailAssistantMsgRow).
func detailMsgRows(pane string, first int) []string {
	return detailRows(pane, first, detailMsgLines)
}

// TestRenderDetailPane_MessageRows is the point of giving each message
// detailMsgLines rows rather than one: one row held about sixteen Japanese
// characters at the widths this pane runs at, which says a message exists
// without saying which one it is.
func TestRenderDetailPane_MessageRows(t *testing.T) {
	m := plainModel()
	const width = 38 // avail 37, so msgAvail is 34

	t.Run("a long message continues onto the second row, hanging under the text", func(t *testing.T) {
		// 54 columns: long enough to need a second row, short enough to fit
		// inside two, so the continuation is the message rather than a cut of
		// it.
		msg := "plugin registry の crawler を実装して、name 衝突も見て"
		sess := session.Info{ID: "s", Status: session.StatusIdle, LastUserMessage: msg}
		rows := detailMsgRows(m.renderDetailPane(sess, width), detailUserMsgRow)

		if !strings.HasPrefix(rows[0], "👤 ") {
			t.Fatalf("row 1 = %q, want the icon leading it", rows[0])
		}
		if rows[1] == "" {
			t.Fatalf("row 2 is blank; the message should have continued onto it: %q", rows[0])
		}
		// The continuation hangs by exactly the icon's width, so the message
		// reads as one block instead of as two entries.
		cont := strings.TrimLeft(rows[1], " ")
		if hang := lipgloss.Width(rows[1]) - lipgloss.Width(cont); hang != msgIconColumns {
			t.Errorf("row 2 = %q hangs %d columns, want %d (the icon's width)", rows[1], hang, msgIconColumns)
		}
		if !strings.Contains(msg, cont) {
			t.Errorf("row 2 = %q is not a slice of the message %q", cont, msg)
		}
		// The whole reason for the second row: strictly more of the message
		// than one row could hold, measured with the ruler that decides what
		// fits.
		msgAvail := width - detailIndentWidth - msgIconColumns
		shown := lipgloss.Width(strings.TrimPrefix(rows[0], "👤 ")) + lipgloss.Width(cont)
		if oneRow := lipgloss.Width(truncateString(msg, msgAvail)); shown <= oneRow {
			t.Errorf("two rows show %d columns of the message, one row shows %d — the second row bought nothing", shown, oneRow)
		}
	})

	t.Run("an absent message costs its rows and no ink", func(t *testing.T) {
		// The height is what the geometry subtracts, so the rows are spent
		// whether or not there is a message. Drawing a lone icon into them
		// would say less than the blank rows do.
		pane := m.renderDetailPane(session.Info{ID: "s", Description: "d", Status: session.StatusIdle}, width)
		if n := detailPaneLineCount(pane); n != detailPaneLines {
			t.Fatalf("pane is %d lines, want %d", n, detailPaneLines)
		}
		for _, first := range []int{detailUserMsgRow, detailAssistantMsgRow} {
			for i, row := range detailMsgRows(pane, first) {
				if strings.TrimSpace(row) != "" {
					t.Errorf("row %d starting at %d = %q, want blank", i, first, row)
				}
			}
		}
	})

	t.Run("the user message is cut from its head, the assistant's from its tail", func(t *testing.T) {
		// A question is identified by how it opens and an answer by how it
		// lands, so the two are truncated from opposite ends. This is the
		// asymmetry the tail-budget arithmetic below exists to protect.
		head, tail := "OPENING-", "-CLOSING"
		long := head + strings.Repeat("filler-", 30) + tail
		sess := session.Info{ID: "s", Status: session.StatusIdle, LastUserMessage: long, LastAssistantMessage: long}
		pane := m.renderDetailPane(sess, width)

		user := detailMsgRows(pane, detailUserMsgRow)
		if !strings.Contains(user[0], head) {
			t.Errorf("user row 1 = %q, want it to keep the message's opening %q", user[0], head)
		}
		if !strings.HasSuffix(user[detailMsgLines-1], "...") {
			t.Errorf("user row %d = %q, want the overflow marked at the end", detailMsgLines, user[detailMsgLines-1])
		}

		assistant := detailMsgRows(pane, detailAssistantMsgRow)
		if !strings.Contains(assistant[0], "...") {
			t.Errorf("assistant row 1 = %q, want the cut marked at the head", assistant[0])
		}
		if !strings.HasSuffix(assistant[detailMsgLines-1], tail) {
			t.Errorf("assistant row %d = %q, want the message's last words %q", detailMsgLines, assistant[detailMsgLines-1], tail)
		}
	})
}

// TestRenderDetailPane_AssistantTailSurvivesAnOddBudget is the regression for
// the -(detailMsgLines-1) in the assistant message's truncation budget; the
// comment at that line has why it is not slack.
//
// The pane is 35 columns wide here because that is where the arithmetic bites:
// avail 34, msgAvail 31, and full-width text spends 30 of those 31 per row.
func TestRenderDetailPane_AssistantTailSurvivesAnOddBudget(t *testing.T) {
	const width = 35
	msgAvail := width - detailIndentWidth - msgIconColumns // 31, odd
	if msgAvail%2 == 0 {
		t.Fatalf("msgAvail = %d, want an odd budget — the case this test exists for", msgAvail)
	}

	// Exactly msgAvail*detailMsgLines columns of full-width text: one column
	// more than the rows can actually hold, and the one length where the
	// missing column overflows rather than being absorbed by the head cut.
	const tail = "答"
	msg := strings.Repeat("回", (msgAvail*detailMsgLines-lipgloss.Width(tail))/2) + tail
	if w := lipgloss.Width(msg); w != msgAvail*detailMsgLines {
		t.Fatalf("message is %d columns, want %d", w, msgAvail*detailMsgLines)
	}

	sess := session.Info{ID: "s", Description: "d", Status: session.StatusIdle, LastAssistantMessage: msg}
	pane := plainModel().renderDetailPane(sess, width)
	if n := detailPaneLineCount(pane); n != detailPaneLines {
		t.Fatalf("pane is %d lines, want %d", n, detailPaneLines)
	}
	rows := detailMsgRows(pane, detailAssistantMsgRow)

	last := rows[detailMsgLines-1]
	if strings.HasSuffix(last, "...") {
		t.Errorf("the last row = %q ends in an ellipsis — the tail the head cut went out of its way to keep was thrown away", last)
	}
	if !strings.HasSuffix(last, tail) {
		t.Errorf("the last row = %q, want it to end on the message's last character %q", last, tail)
	}
	// The ellipsis belongs at the head, where truncateStringFromEnd puts it.
	if !strings.Contains(rows[0], "...") {
		t.Errorf("row 1 = %q, want the cut marked at the head", rows[0])
	}
	for i, row := range rows {
		if w := lipgloss.Width(row); w > width-detailIndentWidth {
			t.Errorf("row %d is %d columns, want <= %d: %q", i, w, width-detailIndentWidth, row)
		}
	}
}

// TestSessionName_ControlCharactersKeepTheRowCount is the regression for text
// that carries its own line breaks; sanitizeRowText has what a newline does to
// the row count and why View() hides the symptom.
//
// Names come from whatever the IPC caller sent (`jin session rename`, the
// agents' own rename hook) and the messages from whatever the agent wrote into
// its transcript, so both are reachable input rather than hypotheticals — hence
// the same hostile string in every field a row or a pane draws.
//
// The repo/branch row is in here too, and its working-directory variant needs no
// agent to misbehave at all: git forbids a control character in a refname, but
// POSIX allows a newline in a directory name, and a session outside a git repo
// draws its working directory in the repo's place — on every list row, so a
// break there costs the whole list's hit-testing.
func TestSessionName_ControlCharactersKeepTheRowCount(t *testing.T) {
	m := plainModel()
	const width = 38

	names := []string{"a\nb", "a\nb\nc\nd", "\nleading", "trailing\n", "tab\there", "cr\rhere", "bell\a", "\x1b[31mstyled\x1b[0m"}
	for _, name := range names {
		// The repo half of the second row has two sources and only one is
		// reachable per session, so each hostile string is run through both.
		for _, sess := range []session.Info{
			{
				ID: "s", Description: name, Status: session.StatusIdle, DescriptionLocked: true,
				LastUserMessage: name, LastAssistantMessage: name,
				RepoName: name, CurrentBranch: name,
			},
			{
				ID: "s", Description: name, Status: session.StatusIdle, DescriptionLocked: true,
				LastUserMessage: name, LastAssistantMessage: name,
				CurrentWorkDir: name, WorkDir: name, CurrentBranch: name,
			},
		} {
			pane := m.renderDetailPane(sess, width)
			if n := detailPaneLineCount(pane); n != detailPaneLines {
				t.Errorf("name %q: detail pane is %d lines, want %d", name, n, detailPaneLines)
			}
			for i, line := range strings.Split(pane, "\n") {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("name %q: pane line %d is %d columns: %q", name, i, w, line)
				}
			}
			row := m.renderSession(sess, false, false, width)
			if n := strings.Count(row, "\n"); n != sessionRowHeight {
				t.Errorf("name %q: list row has %d newlines, want %d", name, n, sessionRowHeight)
			}
			// A row wider than the pane wraps, which costs the same row the
			// newline above does — by the other route.
			for i, line := range sessionRowLines(row) {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("name %q: list row line %d is %d columns: %q", name, i, w, line)
				}
			}
		}
	}

	t.Run("a line break reads as a word break", func(t *testing.T) {
		rows := detailNameRows(m.renderDetailPane(session.Info{ID: "s", Description: "first\nsecond"}, width))
		if rows[0] != "first second" {
			t.Errorf("row 1 = %q, want the break shown as a space rather than swallowed", rows[0])
		}
	})

	t.Run("a message made of nothing but escapes spends no ink", func(t *testing.T) {
		// Sanitizing before the emptiness test is what keeps this from drawing
		// a lone 👤 with nothing after it — an icon introducing nothing reads
		// worse than the blank row it replaced, and the row is paid for either
		// way. An agent whose subject is terminal output writes such sequences
		// out as a matter of course, so a clear-screen reaching the terminal
		// from here is a live path, not a hypothetical.
		sess := session.Info{ID: "s", Status: session.StatusIdle, LastUserMessage: "\x1b[2J", LastAssistantMessage: "\x1b[2J"}
		pane := m.renderDetailPane(sess, width)
		if strings.Contains(pane, "\x1b[2J") {
			t.Errorf("the pane forwarded a clear-screen from a message: %q", pane)
		}
		for i, row := range strings.Split(pane, "\n")[detailUserMsgRow:] {
			if got := strings.TrimSpace(stripANSI(row)); got != "" {
				t.Errorf("message row %d = %q, want blank", i, got)
			}
		}
	})
}

// TestWrapFixedLines covers the wrap itself, with no session and no marker in
// the way. The cases below are the ones a name never reaches: an empty marker, a
// marker that is not "*", and a row budget of zero.
//
// What the wrap does NOT do is sanitize — both callers pay that debt before
// calling in — so there is no case for it here, because pinning the behaviour
// would pin the bug.
func TestWrapFixedLines(t *testing.T) {
	t.Run("returns exactly the rows it was asked for, none wider than its budget", func(t *testing.T) {
		texts := []string{
			"", "s", "a name that fits", strings.Repeat("long-", 40), strings.Repeat("全角", 40),
			// The glyphs the two rulers disagree about; see
			// TestOneRuler_TruncationBoundsRenderedWidth for why they matter.
			strings.Repeat("✔️", 40),
			strings.Repeat("○■▶·", 30),
		}
		for _, text := range texts {
			for _, mark := range []string{"", "*", "!!"} {
				for avail := 1; avail <= 40; avail++ {
					// The width bound holds while the marker fits the budget,
					// which is what both callers give it: the only marker in
					// production is a one-column "*" and avail is never below
					// 1. A marker wider than avail has nowhere to go and is
					// emitted whole; see the followup rather than a case here,
					// since no caller can reach it.
					if lipgloss.Width(mark) > avail {
						continue
					}
					for _, lines := range []int{0, 1, 2, 3} {
						got := wrapFixedLines(text, mark, avail, lines)
						if len(got) != lines {
							t.Fatalf("wrapFixedLines(%q, %q, %d, %d) returned %d rows, want %d",
								text, mark, avail, lines, len(got), lines)
						}
						for i, row := range got {
							if w := lipgloss.Width(row); w > avail {
								t.Errorf("wrapFixedLines(%q, %q, %d, %d) row %d is %d columns: %q",
									text, mark, avail, lines, i, w, row)
							}
						}
					}
				}
			}
		}
	})

	t.Run("with no marker the rows are the text, cut by column", func(t *testing.T) {
		if got := wrapFixedLines("abcdefghij", "", 5, 2); got[0] != "abcde" || got[1] != "fghij" {
			t.Errorf("wrapFixedLines = %q, want [abcde fghij]", got)
		}
		// The last row owes the ellipsis; the rows before it do not.
		if got := wrapFixedLines("abcdefghijkl", "", 5, 2); got[0] != "abcde" || got[1] != "fg..." {
			t.Errorf("wrapFixedLines = %q, want [abcde fg...]", got)
		}
	})

	t.Run("a text shorter than its rows leaves the rest blank", func(t *testing.T) {
		got := wrapFixedLines("hi", "", 5, 3)
		if got[0] != "hi" || got[1] != "" || got[2] != "" {
			t.Errorf("wrapFixedLines = %q, want [hi \"\" \"\"] — a short text may not give a row back", got)
		}
	})

	t.Run("the marker is arbitrary text, not a hard-coded asterisk", func(t *testing.T) {
		// It comes out of the budget rather than past it, and moves down a row
		// when the text it belongs to had just fitted without it.
		if got := wrapFixedLines("abcd", "!!", 6, 2); got[0] != "abcd!!" {
			t.Errorf("wrapFixedLines = %q, want the marker on the row the text ends on", got)
		}
		if got := wrapFixedLines("abcdef", "!!", 6, 2); got[0] != "abcdef" || got[1] != "!!" {
			t.Errorf("wrapFixedLines = %q, want [abcdef !!] — the marker moves down rather than shortening a text that fitted", got)
		}
	})

	t.Run("no rows and no columns both yield nothing to draw", func(t *testing.T) {
		// renderDetailPane guards msgAvail < 1 itself, but the wrap has to hold
		// on its own: a negative budget that fell through would emit rows wider
		// than the pane, which wrap in the terminal and cost the fixed-height
		// block a row.
		for _, lines := range []int{0, -1} {
			if got := wrapFixedLines("text", "*", 10, lines); len(got) != 0 {
				t.Errorf("wrapFixedLines with %d rows = %q, want none", lines, got)
			}
		}
		for _, avail := range []int{0, -1, -8} {
			got := wrapFixedLines("text", "*", avail, detailMsgLines)
			if len(got) != detailMsgLines {
				t.Fatalf("avail %d: got %d rows, want %d", avail, len(got), detailMsgLines)
			}
			for i, row := range got {
				if row != "" {
					t.Errorf("avail %d: row %d = %q, want blank", avail, i, row)
				}
			}
		}
	})
}

// TestSessionNameLines covers the layout helper directly, including the widths
// the pane itself never reaches. The row count is a contract, not a maximum:
// the caller subtracts it from the list height.
func TestSessionNameLines(t *testing.T) {
	t.Run("returns exactly the rows it was asked for, none wider than its budget", func(t *testing.T) {
		names := []string{
			"", "s", "a name that fits", strings.Repeat("long-", 40), strings.Repeat("全角", 40),
			// The glyphs the two rulers disagree about — VS16 emoji, and the
			// East-Asian ambiguous characters go-runewidth sizes from the
			// process locale. A row one column too wide wraps in the terminal
			// and costs the pane a row.
			strings.Repeat("✔️", 40),
			strings.Repeat("⚠️ alert ", 20),
			strings.Repeat("○■▶·", 30),
			strings.Repeat("絵文字と全角", 20),
			strings.Repeat("全角abc", 30),
		}
		for _, name := range names {
			for _, locked := range []bool{false, true} {
				sess := session.Info{Description: name, DescriptionLocked: locked}
				for avail := 1; avail <= 100; avail++ {
					for _, lines := range []int{1, 2, 3} {
						got := sessionNameLines(sess, avail, lines)
						if len(got) != lines {
							t.Fatalf("sessionNameLines(%q, %d, %d) returned %d rows, want %d",
								name, avail, lines, len(got), lines)
						}
						for i, row := range got {
							if w := lipgloss.Width(row); w > avail {
								t.Errorf("sessionNameLines(%q, %d, %d) locked=%v row %d is %d columns: %q",
									name, avail, lines, locked, i, w, row)
							}
						}
					}
				}
			}
		}
	})

	t.Run("the second row carries what the first could not", func(t *testing.T) {
		got := sessionNameLines(session.Info{Description: "abcdefghij"}, 5, 2)
		if got[0] != "abcde" || got[1] != "fghij" {
			t.Errorf("sessionNameLines = %q, want [abcde fghij]", got)
		}
	})

	t.Run("the first row spends its whole budget even when locked", func(t *testing.T) {
		// The marker comes out of the row the name ENDS on, so a name that
		// runs past this row must not pay for it here as well.
		got := sessionNameLines(session.Info{Description: "abcdefghij", DescriptionLocked: true}, 5, 2)
		if got[0] != "abcde" {
			t.Errorf("row 1 = %q, want the full %d columns — the marker is not owed here", got[0], 5)
		}
		// The last row does owe it, and pays the same way a list row does: the
		// ellipsis says something was cut, rather than the marker quietly
		// hiding a character.
		if want := sessionNameText(session.Info{Description: "fghij", DescriptionLocked: true}, 5); got[1] != want {
			t.Errorf("row 2 = %q, want %q — the last row budgets exactly as a one-line name does", got[1], want)
		}
	})

	t.Run("whitespace at the break is dropped", func(t *testing.T) {
		// The break has to land exactly on the space for this to bite: four
		// columns take "abcd", leaving " ef" to open the next row.
		got := sessionNameLines(session.Info{Description: "abcd ef"}, 4, 2)
		if got[0] != "abcd" || got[1] != "ef" {
			t.Errorf("sessionNameLines = %q, want [abcd ef] — the break's space should not indent row 2", got)
		}
	})

	t.Run("no columns to work with yields blank rows, not a stray marker", func(t *testing.T) {
		// Unreachable from renderDetailPane, which returns early below one
		// usable column — but the guard is what keeps a locked name from
		// emitting a lone "*" into a pane that has no room for it.
		for _, avail := range []int{0, -1, -8} {
			got := sessionNameLines(session.Info{Description: "name", DescriptionLocked: true}, avail, detailNameLines)
			if len(got) != detailNameLines {
				t.Fatalf("avail %d: got %d rows, want %d", avail, len(got), detailNameLines)
			}
			for i, row := range got {
				if row != "" {
					t.Errorf("avail %d: row %d = %q, want blank", avail, i, row)
				}
			}
		}
	})

	t.Run("the marker moves down rather than shortening a name that fitted", func(t *testing.T) {
		// A one-column window where the name fills the row exactly. Paying for
		// the marker here would cost an ellipsis plus a column while row 2 sat
		// empty, so a name one column LONGER would show more of itself.
		exact := sessionNameLines(session.Info{Description: strings.Repeat("x", 5), DescriptionLocked: true}, 5, 2)
		if exact[0] != "xxxxx" {
			t.Errorf("row 1 = %q, want the name whole — it fitted before the marker was considered", exact[0])
		}
		if exact[1] != "*" {
			t.Errorf("row 2 = %q, want the marker that had no room above", exact[1])
		}
		// Monotonic: one column more of name may not show LESS of it.
		longer := sessionNameLines(session.Info{Description: strings.Repeat("x", 6), DescriptionLocked: true}, 5, 2)
		if shown, was := lipgloss.Width(longer[0]+longer[1]), lipgloss.Width(exact[0]+exact[1]); shown < was {
			t.Errorf("a 6-column name shows %d columns (%q) but a 5-column one shows %d (%q)",
				shown, longer, was, exact)
		}
	})

	t.Run("escape sequences neither duplicate a row nor reach the terminal", func(t *testing.T) {
		// ansi.Truncate re-opens the styles it cut through, so its result is not
		// a prefix of its input and the wrap's "advance past what I drew" would
		// advance by nothing — drawing row 1 again on row 2. Stripping upstream
		// of the wrap is what makes the prefix assumption true.
		styled := session.Info{Description: "\x1b[31mred name that is quite long and wraps\x1b[0m"}
		got := sessionNameLines(styled, 30, 2)
		if got[0] == got[1] {
			t.Errorf("both rows are %q — the wrap made no progress", got[0])
		}
		// The whole sequence goes, not just its ESC: blanking the ESC alone
		// would keep the invariant and still print "[31m" at the user.
		if got[0] != "red name that is quite long an" {
			t.Errorf("row 1 = %q, want the sequence gone rather than defanged", got[0])
		}
		for i, row := range got {
			if strings.ContainsAny(row, "\x1b[") {
				t.Errorf("row %d = %q still carries part of an escape sequence", i, row)
			}
		}
		// A clear-screen in a session name must not survive to be forwarded.
		if clear := sessionNameLines(session.Info{Description: "before\x1b[2Jafter"}, 30, 2); clear[0] != "beforeafter" {
			t.Errorf("row 1 = %q, want %q", clear[0], "beforeafter")
		}
	})

	t.Run("a full-width name breaks between clusters", func(t *testing.T) {
		// Five columns cannot hold three full-width characters, so the row takes
		// two and leaves the third whole rather than splitting it — the odd
		// column is spent rather than half a glyph emitted.
		got := sessionNameLines(session.Info{Description: "あいうえ"}, 5, 2)
		if got[0] != "あい" || got[1] != "うえ" {
			t.Errorf("sessionNameLines = %q, want [あい うえ]", got)
		}
	})

	t.Run("what the last row cannot hold is cut with an ellipsis", func(t *testing.T) {
		got := sessionNameLines(session.Info{Description: "あいうえお"}, 5, 2)
		if got[0] != "あい" {
			t.Errorf("row 1 = %q, want %q", got[0], "あい")
		}
		if !strings.HasSuffix(got[1], "...") {
			t.Errorf("row 2 = %q, want the overflow marked", got[1])
		}
	})

	t.Run("one row behaves exactly like the list row does", func(t *testing.T) {
		// The pane and the list share sessionNameText's budgeting; asking for a
		// single row must not invent a different answer.
		for _, name := range []string{"short", strings.Repeat("long-", 20), "全角の名前をつける"} {
			for _, locked := range []bool{false, true} {
				sess := session.Info{Description: name, DescriptionLocked: locked}
				for _, avail := range []int{4, 10, 37} {
					got := sessionNameLines(sess, avail, 1)
					if want := sessionNameText(sess, avail); got[0] != want {
						t.Errorf("sessionNameLines(%q, %d, 1)[0] = %q, want %q", name, avail, got[0], want)
					}
				}
			}
		}
	})
}

// TestDetailPaneNameNeverMovesTheList is the constraint this change was shaped
// around. adjustScrollForCursor derives the viewport from listAreaLines, so a
// pane whose height followed the length of the name under the cursor would
// resize the list on every cursor move — the list shifts, the scroll chases it,
// and the two feed each other. The name gets a fixed row budget precisely so
// this test can hold.
func TestDetailPaneNameNeverMovesTheList(t *testing.T) {
	sessions := nameShapeSessions()

	for _, height := range []int{detailPaneHeightThreshold, detailPaneHeightThreshold + 1, 24, 40} {
		// Cursor 0 sets the reference the rest of the column must match.
		m := Model{sessions: sessions, height: height, width: 40, deletingIDs: map[string]bool{}}
		m.adjustScrollForCursor()
		wantList, wantDetail := m.listAreaLines(), m.detailLines()
		wantLines := len(strings.Split(m.renderListContent(38), "\n"))

		for cursor := 1; cursor < len(sessions); cursor++ {
			m.cursor = cursor
			m.adjustScrollForCursor()
			gotLines := len(strings.Split(m.renderListContent(38), "\n"))

			if got := m.listAreaLines(); got != wantList {
				t.Errorf("height %d cursor %d: listAreaLines() = %d, want %d — the list resized under the cursor",
					height, cursor, got, wantList)
			}
			if got := m.detailLines(); got != wantDetail {
				t.Errorf("height %d cursor %d: detailLines() = %d, want %d", height, cursor, got, wantDetail)
			}
			if gotLines != wantLines {
				t.Errorf("height %d cursor %d: rendered %d lines, want %d — the name changed the pane's height",
					height, cursor, gotLines, wantLines)
			}
		}
	}

	t.Run("the pane stays on the bottom edge whatever the name", func(t *testing.T) {
		m := Model{sessions: sessions, height: 24, width: 40, deletingIDs: map[string]bool{}}
		for cursor := range sessions {
			m.cursor = cursor
			m.adjustScrollForCursor()
			lines := strings.Split(m.renderListContent(38), "\n")
			ruleRow := m.noticeLines() + m.headerLines() + m.listAreaLines()
			if !strings.Contains(lines[ruleRow], "─") {
				t.Fatalf("cursor %d: the detail rule should be on row %d, got %q", cursor, ruleRow, lines[ruleRow])
			}
			if got := len(lines) - ruleRow; got != detailPaneLines {
				t.Errorf("cursor %d: %d rows from the rule to the bottom, want %d", cursor, got, detailPaneLines)
			}
		}
	})
}

// TestRenderSession_BranchPriority pins the width fight on the repo/branch pair;
// renderRepoBranch has why the branch wins.
//
// The pair used to live in the detail pane and now sits on the second line of
// every list row, so the row's avail is four columns narrower than the pane's
// was, and it is drawn for every session at once. Widths are given as the columns
// the PAIR gets rather than as pane widths, so the arithmetic in each case reads
// against what renderRepoBranch is deciding.
func TestRenderSession_BranchPriority(t *testing.T) {
	m := plainModel()
	sess := session.Info{
		ID: "s", Description: "d", Status: session.StatusIdle,
		RepoName: "jind-ai", CurrentBranch: "feat/plugin-multi-action-dispatch",
	}

	// repoBranchRow draws a list row wide enough to give the pair exactly
	// `avail` columns and returns its second line, styling stripped. Going
	// through renderSession rather than calling renderRepoBranch directly is
	// what keeps this honest about the columns the pair actually gets.
	//
	// It takes the subtest's own *testing.T rather than closing over the outer
	// one: t.Fatalf on a T whose test has already returned kills the goroutine
	// with "test executed panic(nil) or runtime.Goexit" and the real assertion
	// never gets printed.
	repoBranchRow := func(t *testing.T, sess session.Info, avail int) string {
		t.Helper()
		row := m.renderSession(sess, false, false, avail+sessionRowLead)
		lines := sessionRowLines(row)
		if len(lines) != sessionRowHeight {
			t.Fatalf("renderSession returned %d lines, want %d", len(lines), sessionRowHeight)
		}
		return stripANSI(lines[1])
	}

	t.Run("wide enough for both", func(t *testing.T) {
		got := repoBranchRow(t, sess, 59)
		if !strings.Contains(got, "jind-ai") {
			t.Errorf("row = %q, want the repo present at a comfortable width", got)
		}
		if !strings.Contains(got, "feat/plugin-multi-action-dispatch") {
			t.Errorf("row = %q, want the branch present in full", got)
		}
		if !strings.HasSuffix(strings.TrimRight(got, " "), "feat/plugin-multi-action-dispatch") {
			t.Errorf("row = %q, want the branch right-aligned against the repo on the left", got)
		}
	})

	t.Run("narrow keeps the branch tail and sacrifices the repo", func(t *testing.T) {
		// 29 is what the detail pane gave the pair at a 30-column width; 23 is
		// what a list row gives it at minTUIWidth (30, content 28), the four
		// columns narrower that moving the pair out of the pane cost it.
		for _, avail := range []int{29, 23} {
			got := repoBranchRow(t, sess, avail)
			if w := lipgloss.Width(got); w > avail+sessionRowLead {
				t.Fatalf("avail %d: row is %d columns wide, want <= %d: %q", avail, w, avail+sessionRowLead, got)
			}
			// Truncated from the END, so the identifying tail survives: "feat/"
			// is shared by half the branches in the repo and carries nothing.
			// The ellipsis costs three columns, so what must be left is the
			// branch's last avail-3 (all-ASCII here, so bytes are columns).
			tail := sess.CurrentBranch[len(sess.CurrentBranch)-(avail-3):]
			if !strings.Contains(got, tail) {
				t.Errorf("avail %d: row = %q, want the branch tail %q to survive", avail, got, tail)
			}
			if strings.Contains(got, "jind-ai") {
				t.Errorf("avail %d: row = %q, want the repo cut back to make room for the branch", avail, got)
			}
		}
	})

	t.Run("a repo that would only fit truncated is dropped whole", func(t *testing.T) {
		// avail 39, branch 33 columns, so the repo is offered 5 — enough for
		// "ji..." and nothing more. A truncated repo name disambiguates
		// nothing (it fits jind-ai and jind-ai-notifier alike) and this row
		// is the only place it appears, so the columns go to the branch.
		got := repoBranchRow(t, sess, 39)
		if !strings.Contains(got, "feat/plugin-multi-action-dispatch") {
			t.Fatalf("row = %q, want the branch in full at this width", got)
		}
		// Check for the stub the old truncating behaviour produced rather
		// than a bare "ji", which also matches text inside the branch.
		for _, stub := range []string{"ji...", "jind-...", "jind-ai"} {
			if strings.Contains(got, stub) {
				t.Errorf("row = %q, want no repo fragment; found %q", got, stub)
			}
		}
	})

	t.Run("the workdir fallback keeps its tail", func(t *testing.T) {
		// A path is the one thing here worth truncating: its tail identifies
		// it, while its head is shared with everything under the same root.
		noRepo := session.Info{
			ID: "s", Description: "d", Status: session.StatusIdle,
			WorkDir: "/var/opt/some/deeply/nested/place/notes", CurrentBranch: "main",
		}
		if got := repoBranchRow(t, noRepo, 29); !strings.Contains(got, "notes") {
			t.Errorf("row = %q, want the path tail %q to survive", got, "notes")
		}
	})

	t.Run("the workdir fallback wears a ~ for the home directory", func(t *testing.T) {
		// Sessions live under $HOME almost by definition, so without this the
		// stand-in spends its first columns on a prefix every row repeats — and
		// the truncation that keeps the tail would eat the part that differs
		// first. The '~' has to be the substitute, not merely the cut: dropping
		// the home prefix without it yields "/dev/notes", a path that reads as
		// absolute and points somewhere the session is not.
		t.Setenv("HOME", "/home/tester")
		noRepo := session.Info{
			ID: "s", Description: "d", Status: session.StatusIdle,
			WorkDir: "/home/tester/dev/notes", CurrentBranch: "main",
		}
		got := repoBranchRow(t, noRepo, 29)
		if !strings.Contains(got, "~/dev/notes") {
			t.Errorf("row = %q, want the home directory shortened to %q", got, "~/dev/notes")
		}
		if strings.Contains(got, "/home/tester") {
			t.Errorf("row = %q, want the home prefix replaced rather than spelled out", got)
		}
	})

	t.Run("no branch leaves the repo the whole row", func(t *testing.T) {
		noBranch := sess
		noBranch.CurrentBranch = ""
		if got := repoBranchRow(t, noBranch, 29); !strings.Contains(got, "jind-ai") {
			t.Errorf("row = %q, want the repo shown when there is no branch to compete with", got)
		}
	})

	t.Run("the pair no longer appears in the detail pane", func(t *testing.T) {
		// D-5: the row it vacated in the pane is what paid for the second
		// message row. A pane that still drew it would be a row over budget,
		// and View() clips that silently from the bottom.
		pane := plainModel().renderDetailPane(sess, 38)
		if strings.Contains(stripANSI(pane), "jind-ai") {
			t.Errorf("the detail pane still draws the repo: %q", pane)
		}
		if n := detailPaneLineCount(pane); n != detailPaneLines {
			t.Errorf("pane is %d lines, want %d", n, detailPaneLines)
		}
	})
}

// TestRenderRepoBranch_SanitizesAfterTheFallback pins the one ordering decision
// in renderRepoBranch: the repo name is sanitized AFTER the working-directory
// stand-in has been chosen, not before.
//
// Only one kind of value can tell the two orders apart, and it is worth naming
// because the obvious candidate cannot. A repo name of control characters comes
// back from sanitizeRowText as spaces — non-empty on either path — so it
// suppresses the stand-in whichever order runs. An escape sequence is different:
// ansi.Strip takes it out whole, so sanitizing first would leave "" and pull in
// the working directory, printing a path beside a repo the session HAS but
// cannot draw. Both cases below render identically today; only the first one
// moves if the two lines are swapped.
func TestRenderRepoBranch_SanitizesAfterTheFallback(t *testing.T) {
	const avail = 33
	// The repo column stays blank in both cases, so the branch is right-aligned
	// against nothing but padding.
	want := strings.Repeat(" ", avail-len("main")) + "main"

	tests := []struct {
		name string
		repo string
	}{
		{name: "an escape-only repo name draws nothing and stands in for nothing", repo: "\x1b[2J"},
		{name: "a control-character-only repo name is the same either way", repo: "\x01\x02"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := session.Info{
				RepoName:       tt.repo,
				CurrentWorkDir: "/home/u/proj",
				CurrentBranch:  "main",
			}
			got := stripANSI(renderRepoBranch(sess, avail, helpStyle))
			if got != want {
				t.Errorf("renderRepoBranch(%q) = %q, want %q — the working directory must not stand in for a repo name the session has",
					tt.repo, got, want)
			}
		})
	}
}

// TestRenderDetailPane_FollowsCursor is the spec's headline requirement: the
// detail pane describes the session under the CURSOR, while the tmux pane on
// the right keeps showing whatever the user last attached to. The two notions
// of "current" are deliberately orthogonal, and being able to read a session
// without switching to it is the entire point of the pane.
func TestRenderDetailPane_FollowsCursor(t *testing.T) {
	sessions := []session.Info{
		{ID: "a", Description: "alpha-session", Status: session.StatusIdle, Fleet: session.DefaultFleet},
		{ID: "b", Description: "bravo-session", Status: session.StatusIdle, Fleet: session.DefaultFleet},
		{ID: "c", Description: "charlie-session", Status: session.StatusIdle, Fleet: session.DefaultFleet},
	}
	newModel := func() Model {
		// Above detailPaneHeightThreshold (18), or there would be no pane to
		// follow anything.
		return Model{sessions: sessions, height: 20, width: 40, deletingIDs: map[string]bool{}}
	}

	t.Run("moving the cursor changes the described session", func(t *testing.T) {
		m := newModel()
		for i, want := range []string{"alpha-session", "bravo-session", "charlie-session"} {
			m.cursor = i
			got := m.renderListContent(38)
			if !strings.Contains(got, want) {
				t.Errorf("cursor %d: rendered content does not mention %q", i, want)
			}
			// The name appears once in its list row and once in the detail
			// pane; the other two sessions appear only in their rows.
			if n := strings.Count(got, want); n != 2 {
				t.Errorf("cursor %d: %q appears %d times, want 2 (list row + detail pane)", i, want, n)
			}
		}
	})

	t.Run("the displayed session does not steer the pane", func(t *testing.T) {
		m := newModel()
		m.cursor = 0
		// The user is attached to "charlie" on the right while the cursor sits
		// on "alpha". The pane must describe alpha.
		m.currentSessionID = "c"
		got := m.renderListContent(38)
		if n := strings.Count(got, "alpha-session"); n != 2 {
			t.Errorf("%q appears %d times, want 2 — the detail pane must follow the cursor", "alpha-session", n)
		}
		if n := strings.Count(got, "charlie-session"); n != 1 {
			t.Errorf("%q appears %d times, want 1 — the viewed session gets no detail block", "charlie-session", n)
		}
	})
}

// --- renderListContent ---

// TestRenderListContent_MatchesGeometry is the test the whole design rests on:
// it checks that what renderListContent actually EMITS agrees, row for row,
// with what the geometry functions claim. Everything else — scroll clamping,
// PageUp/PageDown, mouse hit-testing — is arithmetic derived from those
// functions, so if the renderer and the arithmetic disagree by even one row,
// every click below the disagreement lands on the wrong session.
//
// The old code kept that agreement by hand ("keep cardHeight in sync with
// renderSession"); this change replaces the contract with constants, and this
// test is what makes the replacement real rather than asserted.
func TestRenderListContent_MatchesGeometry(t *testing.T) {
	build := func(n int, fleets []string) []session.Info {
		out := make([]session.Info, n)
		for i := range out {
			out[i] = session.Info{
				ID:          fmt.Sprintf("id-%02d", i),
				Description: fmt.Sprintf("session-%02d", i),
				Status:      session.StatusIdle,
				Fleet:       fleets[i%len(fleets)],
			}
		}
		return out
	}

	for _, fleets := range [][]string{{session.DefaultFleet}, {"backend", session.DefaultFleet}} {
		for _, n := range []int{1, 2, 3, 8, 20} {
			for height := 6; height <= 26; height++ {
				for _, withNotice := range []bool{false, true} {
					for _, scroll := range []int{0, 1, 5, 999} {
						m := Model{
							sessions:     build(n, fleets),
							height:       height,
							width:        40,
							deletingIDs:  map[string]bool{},
							scrollOffset: scroll,
						}
						if withNotice {
							m.warning = "heads up"
						}
						m.cursor = min(n-1, 3)
						m.clampScroll()

						lines := strings.Split(m.renderListContent(38), "\n")
						budget := m.noticeLines() + m.headerLines() + m.listAreaLines() + m.detailLines()
						label := fmt.Sprintf("fleets=%d n=%d height=%d notice=%v scroll=%d",
							len(fleets), n, height, withNotice, scroll)

						// Never exceed the budget: an extra row pushes the
						// bottom of the content past the pane, which MaxHeight
						// then clips — silently, and from the wrong end.
						if len(lines) > budget {
							t.Fatalf("%s: rendered %d lines, geometry budgets %d (notices %d + header %d + list %d + detail %d)",
								label, len(lines), budget,
								m.noticeLines(), m.headerLines(), m.listAreaLines(), m.detailLines())
						}
						// When the detail pane is drawn the total must be
						// exact, because that is what pins the pane to the
						// bottom edge. With no detail pane the trailing blanks
						// are the pane style's job, so a short list is fine.
						if m.detailVisible() && len(lines) != budget {
							t.Fatalf("%s: detail pane visible but rendered %d lines, want exactly %d — the pane is not on the bottom edge",
								label, len(lines), budget)
						}

						// The list window must start exactly where the mouse
						// hit-test believes it starts.
						listTop := m.noticeLines() + m.headerLines()
						for idx := range m.sessions {
							top, _ := m.sessionCardTop(idx)
							row := listTop + top - m.scrollOffset
							if row < listTop || row >= listTop+m.listAreaLines() {
								continue // scrolled out of view
							}
							if !strings.Contains(lines[row], m.sessions[idx].Description) {
								t.Fatalf("%s: session %d should be drawn on row %d, got %q",
									label, idx, row, lines[row])
							}
							if got, ok := m.sessionIndexAtRow(row); !ok || got != idx {
								t.Fatalf("%s: sessionIndexAtRow(%d) = (%d, %v), want (%d, true)",
									label, row, got, ok, idx)
							}
						}

						// The detail pane must begin on the first row after the
						// list area, so it sits on the pane's bottom edge.
						if m.detailVisible() {
							ruleRow := listTop + m.listAreaLines()
							if !strings.Contains(lines[ruleRow], "─") {
								t.Fatalf("%s: detail pane rule should be on row %d, got %q",
									label, ruleRow, lines[ruleRow])
							}
							if !strings.Contains(lines[ruleRow+1], m.sessions[m.cursor].Description) {
								t.Fatalf("%s: detail pane should name the cursor's session on row %d, got %q",
									label, ruleRow+1, lines[ruleRow+1])
							}
						}
					}
				}
			}
		}
	}
}

// TestView_NarrowAndShortTerminals drives the real View() — pane style, clamps
// and all — at the smallest terminals the TUI accepts, which is where the extra
// name row has to give way. Rendering the assembled view is the check the
// per-renderer tests cannot make: MaxHeight clips silently and from the wrong
// end, so a pane one row over budget loses its bottom line rather than erroring.
func TestView_NarrowAndShortTerminals(t *testing.T) {
	sessions := nameShapeSessions()

	for _, width := range []int{minTUIWidth, minTUIWidth + 5, maxTUIWidth} {
		for height := 5; height <= detailPaneHeightThreshold+2; height++ {
			for cursor := range sessions {
				m := Model{sessions: sessions, cursor: cursor, width: width, height: height, deletingIDs: map[string]bool{}}
				m.adjustScrollForCursor()
				view := m.View()
				label := fmt.Sprintf("%dx%d cursor %d", width, height, cursor)

				lines := strings.Split(view, "\n")
				if want := max(height-helpChromeLines, 5) + helpChromeLines; len(lines) != want {
					t.Fatalf("%s: View() is %d rows, want %d (pane + rule + help line)", label, len(lines), want)
				}
				for i, line := range lines {
					if w := lipgloss.Width(line); w > max(width, 20) {
						t.Errorf("%s: row %d is %d columns wide: %q", label, i, w, line)
					}
				}

				// The chrome under the pane is drawn at every height, list or
				// no list: a rule, then the help line. Checked before the pane
				// so the two rules can be compared column for column below.
				contentWidth := max(max(width, 20)-2, 16)
				chromeRule := stripANSI(lines[len(lines)-helpChromeLines])
				if want := " " + strings.Repeat("─", contentWidth); chromeRule != want {
					t.Errorf("%s: chrome rule = %q, want %q", label, chromeRule, want)
				}
				if help := stripANSI(lines[len(lines)-1]); !strings.Contains(help, "? help") {
					t.Errorf("%s: last row = %q, want the help line", label, help)
				}

				// The rule is a full-width run, so a stray "─" inside a name or
				// a branch cannot be mistaken for it.
				//
				// Cutting helpChromeLines rather than one row is load-bearing:
				// the chrome now opens with a rule of its own, and leaving it in
				// would be found by the scan below at every height — reporting a
				// detail pane on terminals too short to have one, and passing.
				body := lines[:len(lines)-helpChromeLines]
				ruleRow := -1
				for i, line := range body {
					if strings.Contains(stripANSI(line), strings.Repeat("─", 4)) {
						ruleRow = i
						break
					}
				}

				// Below the threshold the pane is dropped whole rather than
				// shrunk — the name rows are not what gives, the block is.
				if want := m.detailVisible(); (ruleRow >= 0) != want {
					t.Errorf("%s: detail rule present = %v, want %v", label, ruleRow >= 0, want)
				}
				if !m.detailVisible() {
					continue
				}
				// Present means whole: the rule and every row under it survived
				// the height clamp.
				if got := len(body) - ruleRow; got != detailPaneLines {
					t.Errorf("%s: %d rows from the rule to the pane's bottom, want %d", label, got, detailPaneLines)
				}
				// One column of drift between the two rules would read as two
				// different kinds of boundary rather than as one repeated.
				if got := stripANSI(body[ruleRow]); strings.Index(got, "─") != strings.Index(chromeRule, "─") {
					t.Errorf("%s: the pane rule %q does not start in the chrome rule's column %q", label, got, chromeRule)
				}
			}
		}
	}
}

// TestRenderListContent_NoLineExceedsWidth guards the geometry from the one
// failure it cannot see: a line wider than the pane wraps in the terminal, so
// one rendered row becomes two physical rows and every scroll and hit-test
// offset below it is off by one.
func TestRenderListContent_NoLineExceedsWidth(t *testing.T) {
	long := strings.Repeat("wide-", 40)
	cjk := strings.Repeat("全角文字列", 20)

	sessions := []session.Info{
		{ID: "a", Description: long, Status: session.StatusThinking, Fleet: "backend", RepoName: long, CurrentBranch: long, AgentKind: "claude", LastUserMessage: long, LastAssistantMessage: long},
		{ID: "b", Description: cjk, Status: session.StatusPermission, Fleet: "backend", RepoName: cjk, CurrentBranch: cjk, LastUserMessage: cjk, LastAssistantMessage: cjk},
		{ID: "c", Description: "short", DescriptionLocked: true, Status: session.Status("bogus"), Fleet: session.DefaultFleet},
		{ID: "d", Description: "deleting", Status: session.StatusDeleting, Fleet: session.DefaultFleet},
	}

	// minTUIWidth and maxTUIWidth bound the pane the outer tmux gives us; the
	// content sits inside one column of padding on each side.
	for _, paneWidth := range []int{minTUIWidth, 40, maxTUIWidth} {
		for _, height := range []int{8, 14, 16, 30} {
			for cursor := range sessions {
				m := Model{
					sessions:         sessions,
					cursor:           cursor,
					width:            paneWidth,
					height:           height,
					deletingIDs:      map[string]bool{"d": true},
					currentSessionID: "b",
					warning:          "a warning that pushes everything down",
				}
				contentWidth := paneWidth - 2
				for i, line := range strings.Split(m.renderListContent(contentWidth), "\n") {
					if w := lipgloss.Width(line); w > contentWidth {
						t.Fatalf("pane %dx%d cursor %d: line %d is %d columns wide, want <= %d: %q",
							paneWidth, height, cursor, i, w, contentWidth, line)
					}
				}
			}
		}
	}
}

// --- renderSession ---

// TestRenderSession_TwoLines pins the invariant the whole list geometry rests
// on: a session row is exactly sessionRowHeight lines, whatever the session
// carries. It replaces the old hand-maintained "cardHeight matches
// renderSession" contract — there is nothing left to keep in sync, only this to
// keep true.
//
// The count is asserted against the constant rather than against 2, so a row
// that grows a third line has to move the constant the scroll and hit-test
// arithmetic reads, instead of only moving this expectation.
func TestRenderSession_TwoLines(t *testing.T) {
	longName := strings.Repeat("very-long-session-name-", 20)
	cjkName := strings.Repeat("セッション名前", 10) // full-width: 2 columns per rune

	tests := []struct {
		name string
		m    Model
		sess session.Info
	}{
		{
			name: "plain",
			m:    plainModel(),
			sess: session.Info{ID: "s", Description: "plain", Status: session.StatusIdle},
		},
		{
			name: "empty description",
			m:    plainModel(),
			sess: session.Info{ID: "s", Status: session.StatusIdle},
		},
		{
			name: "deleting via the TUI's optimistic mark",
			m:    Model{deletingIDs: map[string]bool{"s": true}},
			sess: session.Info{ID: "s", Description: "going away", Status: session.StatusIdle},
		},
		{
			name: "deleting via the daemon-reported status",
			m:    plainModel(),
			sess: session.Info{ID: "s", Description: "going away", Status: session.StatusDeleting},
		},
		{
			name: "very long name",
			m:    plainModel(),
			sess: session.Info{ID: "s", Description: longName, Status: session.StatusThinking},
		},
		{
			name: "full-width name",
			m:    plainModel(),
			sess: session.Info{ID: "s", Description: cjkName, Status: session.StatusThinking},
		},
		{
			name: "locked description",
			m:    plainModel(),
			sess: session.Info{ID: "s", Description: longName, DescriptionLocked: true, Status: session.StatusIdle},
		},
		{
			name: "message fields no longer render (they moved to the detail pane)",
			m:    plainModel(),
			sess: session.Info{
				ID: "s", Description: "chatty", Status: session.StatusIdle,
				CurrentBranch: "feat/x", LastUserMessage: "hi", LastAssistantMessage: "yo",
			},
		},
		{
			name: "repo and branch both far past the row's budget",
			m:    plainModel(),
			sess: session.Info{
				ID: "s", Description: "busy", Status: session.StatusRunning,
				RepoName: longName, CurrentBranch: longName,
			},
		},
		{
			name: "no repo, so the workdir stands in on line two",
			m:    plainModel(),
			sess: session.Info{
				ID: "s", Description: "outside a repo", Status: session.StatusIdle,
				WorkDir: "/var/opt/some/deeply/nested/place/notes",
			},
		},
		// The two shortest second lines there are. They are spelled out rather
		// than left to the cases above because the row is padded to width from
		// what the metadata happens to measure, so the least metadata is where
		// a missing pad shows up as the widest hole: a short repo leaves 12 of
		// 40 columns filled, and nothing at all leaves 5.
		{
			name: "a repo with no branch to compete with",
			m:    plainModel(),
			sess: session.Info{
				ID: "s", Description: "solo", Status: session.StatusIdle,
				RepoName: "jind-ai",
			},
		},
		{
			name: "no repo, no branch and no workdir, so line two is blank",
			m:    plainModel(),
			sess: session.Info{ID: "s", Description: "bare", Status: session.StatusIdle},
		},
	}

	// minTUIWidth (30) is the narrowest pane the TUI allows; 40 is typical.
	for _, width := range []int{30, 40} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%s/width=%d", tt.name, width), func(t *testing.T) {
				got := tt.m.renderSession(tt.sess, false, false, width)
				if n := strings.Count(got, "\n"); n != sessionRowHeight {
					t.Errorf("renderSession() has %d newlines, want %d: %q", n, sessionRowHeight, got)
				}
				if !strings.HasSuffix(got, "\n") {
					t.Errorf("renderSession() must end with a newline: %q", got)
				}
				// Exactly width, not at most: overflowing by one column wraps
				// the line in the terminal and breaks the geometry on screen
				// while the returned string still looks like sessionRowHeight
				// lines — and falling short by one leaves the viewed row's
				// background with a ragged right edge instead of a band, which
				// is what the padding on each line is for. A bound of "<=" was
				// blind to the second half: dropping the pad after the
				// repo/branch pair ended line two after 12 of 40 columns and no
				// test noticed.
				for i, line := range sessionRowLines(got) {
					if w := lipgloss.Width(line); w != width {
						t.Errorf("line %d is %d columns, want exactly %d: %q", i, w, width, line)
					}
				}
			})
		}
	}

	t.Run("a width too narrow for the lead still costs its rows", func(t *testing.T) {
		// The early return has to emit sessionRowHeight blank rows: returning
		// one would break the invariant from the other side, and the geometry
		// cannot tell the difference until the list has silently slid up.
		// View() clamps the pane to minTUIWidth, so this is a floor under the
		// arithmetic rather than a width a user reaches.
		m := plainModel()
		sess := session.Info{ID: "s", Description: "cramped", Status: session.StatusIdle, RepoName: "r", CurrentBranch: "b"}
		for width := range sessionRowLead + 1 {
			got := m.renderSession(sess, true, true, width)
			if n := strings.Count(got, "\n"); n != sessionRowHeight {
				t.Errorf("width %d: %d newlines, want %d: %q", width, n, sessionRowHeight, got)
			}
			for i, line := range sessionRowLines(got) {
				if w := lipgloss.Width(line); w != max(width, 0) {
					t.Errorf("width %d: line %d is %d columns, want exactly %d: %q", width, i, w, width, line)
				}
			}
		}
	})
}

// TestRenderSession_Layout pins the fixed column layout: both lines of a row
// start their text at sessionRowLead so the list reads as a table, and a locked
// description keeps its '*' marker.
func TestRenderSession_Layout(t *testing.T) {
	m := plainModel()
	const width = 40

	// textColumn reports the column `text` begins at on a rendered row line.
	textColumn := func(t *testing.T, line, text string) int {
		t.Helper()
		idx := strings.Index(line, text)
		if idx < 0 {
			t.Fatalf("%q missing from %q", text, line)
		}
		return lipgloss.Width(line[:idx])
	}

	t.Run("name starts at the same column whatever the icon width", func(t *testing.T) {
		// "⚡" (THINKING) is 2 columns, "○" (IDLE) is 1; padIcon absorbs the
		// difference.
		for _, status := range []session.Status{session.StatusIdle, session.StatusThinking, session.StatusPermission} {
			sess := session.Info{ID: "s", Description: "name", Status: status}
			line := sessionRowLines(m.renderSession(sess, false, false, width))[0]
			if col := textColumn(t, line, "name"); col != sessionRowLead {
				t.Errorf("status %q: name starts at column %d, want %d", status, col, sessionRowLead)
			}
		}
	})

	t.Run("the second line hangs its metadata under the name", func(t *testing.T) {
		// The lead is spent on the cursor bar and an empty icon cell, so the
		// repo lands in the name's column. That is what makes the two lines
		// read as one row rather than as a row and a caption.
		sess := session.Info{
			ID: "s", Description: "name", Status: session.StatusThinking,
			RepoName: "jind-ai", CurrentBranch: "feat/x",
		}
		lines := sessionRowLines(m.renderSession(sess, false, false, width))
		if got := textColumn(t, lines[1], "jind-ai"); got != sessionRowLead {
			t.Errorf("repo starts at column %d, want %d (the name's column)", got, sessionRowLead)
		}
		// The branch is the right anchor of the same line; nothing may follow
		// it, or the right alignment is only approximate.
		if got := strings.TrimRight(stripANSI(lines[1]), " "); !strings.HasSuffix(got, "feat/x") {
			t.Errorf("line 2 = %q, want the branch flush against the right edge", got)
		}
		// The status is stated once: repeating the icon on line two would make
		// the row read as two sessions.
		if icon, _, _ := getStatusDisplay(session.StatusThinking); strings.Contains(lines[1], icon) {
			t.Errorf("line 2 = %q, want no status icon — line one already carries it", lines[1])
		}
	})

	t.Run("locked description keeps its marker", func(t *testing.T) {
		sess := session.Info{ID: "s", Description: "locked", DescriptionLocked: true, Status: session.StatusIdle}
		if got := m.renderSession(sess, false, false, width); !strings.Contains(got, "locked*") {
			t.Errorf("renderSession() = %q, want the lock marker after the name", got)
		}
	})

	t.Run("empty description gets no lock marker", func(t *testing.T) {
		sess := session.Info{ID: "s", DescriptionLocked: true, Status: session.StatusIdle}
		if got := m.renderSession(sess, false, false, width); strings.Contains(got, "*") {
			t.Errorf("renderSession() = %q, want no lock marker on an empty name", got)
		}
	})
}

// TestRenderSession_Indicators verifies the two orthogonal indicators:
//   - selected → blue '▎' cursor bar in the row's first column
//   - viewed   → subdued row background (detectable via presence of an ANSI
//     SGR bg code in the rendered output)
//
// Both have to hold on BOTH lines of the row. A bar or a band that stopped
// after line one would split one session into a marked half and an unmarked
// one, which is exactly the reading the two-line row exists to prevent.
func TestRenderSession_Indicators(t *testing.T) {
	m := Model{}
	width := 40

	// A repo and a branch that cannot fill the row put a gap in the middle of
	// line two — the one stretch with no glyph of its own, and the place a
	// background painted per styled segment goes missing.
	//
	// Which of renderRepoBranch's two gap-filling paths runs depends on whether
	// the repo earns any columns, so both fixtures are needed: one row wide
	// enough for the repo (the gap sits between repo and branch), and one where
	// the repo is dropped whole and the gap becomes everything left of the
	// branch. The dropped one is the bigger hole — 31 of the row's 40 columns
	// at this width — and the one no test reached while the fixture was a
	// 7-column repo name that always fit.
	repos := []struct {
		name          string
		sess          session.Info
		wantRepoDrawn bool
	}{
		{
			name: "repo fits beside the branch",
			sess: session.Info{
				ID:            "test-id",
				Description:   "test-session",
				Status:        session.StatusIdle,
				RepoName:      "jind-ai",
				CurrentBranch: "feat/x",
			},
			wantRepoDrawn: true,
		},
		{
			name: "repo dropped whole",
			sess: session.Info{
				ID:            "test-id",
				Description:   "test-session",
				Status:        session.StatusIdle,
				RepoName:      "some-quite-long-repository-name",
				CurrentBranch: "main",
			},
		},
	}

	// The SGR sequence "\x1b[48" is the prefix for any background color set
	// (48;5;n for 256-color, 48;2;R;G;B for truecolor). Its presence is a
	// reliable signal that the viewed background was applied.
	const bgSGR = "\x1b[48"

	tests := []struct {
		name       string
		selected   bool
		viewed     bool
		wantBar    bool
		wantViewBg bool
	}{
		{name: "neither", selected: false, viewed: false, wantBar: false, wantViewBg: false},
		{name: "selected only", selected: true, viewed: false, wantBar: true, wantViewBg: false},
		{name: "viewed only", selected: false, viewed: true, wantBar: false, wantViewBg: true},
		{name: "selected and viewed", selected: true, viewed: true, wantBar: true, wantViewBg: true},
	}

	for _, rc := range repos {
		for _, tt := range tests {
			t.Run(rc.name+"/"+tt.name, func(t *testing.T) {
				lines := sessionRowLines(m.renderSession(rc.sess, tt.selected, tt.viewed, width))
				if len(lines) != sessionRowHeight {
					t.Fatalf("renderSession returned %d lines, want %d", len(lines), sessionRowHeight)
				}
				// The fixture only exercises the path it was written for while
				// the repo lands on the intended side of the width fight, and
				// nothing else here would notice it drifting: a widened row
				// would quietly put both fixtures back on the same branch.
				if drawn := strings.Contains(stripANSI(lines[1]), rc.sess.RepoName); drawn != rc.wantRepoDrawn {
					t.Fatalf("repo %q drawn = %v, want %v — the fixture no longer picks the branch it was written for: %q",
						rc.sess.RepoName, drawn, rc.wantRepoDrawn, stripANSI(lines[1]))
				}

				// The third indicator, and the one that is a colour rather than
				// a glyph: line two stays helpStyle no matter what line one
				// does. The list's information hierarchy is colour icon / white
				// name / grey metadata, and a second line that took the name's
				// style on the cursor row would flatten two of those three into
				// one. Checked against the rendered substring because the
				// branch is emitted as its own styled segment, so its escape
				// prefix is the style, verbatim.
				meta, name := helpStyle, sessionNameStyle
				if tt.selected {
					name = selectedItemStyle
				}
				if tt.viewed {
					meta, name = meta.Background(viewedRowBg), name.Background(viewedRowBg)
				}
				if !strings.Contains(lines[1], meta.Render(rc.sess.CurrentBranch)) {
					t.Errorf("line 2 = %q, want the branch drawn in helpStyle (%q)", lines[1], meta.Render(rc.sess.CurrentBranch))
				}
				if strings.Contains(lines[1], name.Render(rc.sess.CurrentBranch)) {
					t.Errorf("line 2 = %q, want the metadata NOT drawn in the name's style", lines[1])
				}

				for i, line := range lines {
					if line == "" {
						t.Fatalf("line %d is empty", i)
					}
					hasBar := strings.Contains(line, "▎")
					if hasBar != tt.wantBar {
						t.Errorf("line %d: cursor bar present = %v, want %v (%q)", i, hasBar, tt.wantBar, line)
					}
					hasViewBg := strings.Contains(line, bgSGR)
					if hasViewBg != tt.wantViewBg {
						t.Errorf("line %d: viewed background present = %v, want %v (%q)", i, hasViewBg, tt.wantViewBg, line)
					}
					// The background must reach the end of the line: with the
					// blank spacer between cards gone, a short background would
					// leave a ragged right edge instead of a continuous band.
					if tt.wantViewBg && !strings.HasSuffix(line, "\x1b[0m") {
						t.Errorf("line %d does not end in a styled segment: %q", i, line)
					}
				}

				if !tt.wantViewBg {
					return
				}
				// Continuous, not merely present at both ends. lipgloss closes
				// every styled run with a reset, so a viewed row is a chain of
				// runs each opening with an escape; text emitted outside one shows
				// up as a run starting with the text itself. The gap on line two
				// is where that bites — it is the stretch with no glyph of its
				// own, so a bare strings.Repeat(" ", n) there punches a hole
				// through the band: between the two names when the repo fits,
				// and across everything left of the branch when it does not.
				for i, line := range lines {
					for n, run := range strings.Split(line, "\x1b[0m") {
						if run != "" && !strings.HasPrefix(run, "\x1b[") {
							t.Errorf("line %d run %d is unstyled — the band has a hole at %q: %q", i, n, run, line)
						}
					}
				}
			})
		}
	}
}

// --- dispatchAction / currentCursorSessionID / writeCursorEnv ---
//
// Note: the tui package has no mock for *tmux.Client or *daemon.Client — both
// are concrete structs with unexported fields, and expanding an interface just
// for these tests would balloon this task well beyond R4. The tests below
// cover the routing/guard logic reachable without live clients; the real
// side-effect wiring (ZoomPane, SetEnvironment, PluginRun) is exercised only
// by manual verification, not by this suite.

// TestDispatchAction_CoreRouting_TogglePane verifies that IDTogglePane routes
// to handleTogglePane and that the tmuxClient=nil guard keeps it a safe
// no-op (the call the palette makes into an unwired Model).
func TestDispatchAction_CoreRouting_TogglePane(t *testing.T) {
	m := Model{deletingIDs: map[string]bool{}}
	next, cmd := m.dispatchAction(action.IDTogglePane)
	if cmd != nil {
		t.Errorf("expected nil Cmd from toggle-pane on unwired model, got %T", cmd)
	}
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("expected Model, got %T", next)
	}
	if nm.err != nil {
		t.Errorf("expected no err, got %v", nm.err)
	}
}

// TestDispatchAction_CoreRouting_SessionFilter verifies that IDSessionFilter
// routes to handleSessionFilter and that the tmuxClient=nil guard keeps it
// a safe no-op — the palette must be able to dispatch the action even on
// an unwired Model (e.g. before the outer tmux binding has been applied).
func TestDispatchAction_CoreRouting_SessionFilter(t *testing.T) {
	m := Model{deletingIDs: map[string]bool{}}
	next, cmd := m.dispatchAction(action.IDSessionFilter)
	if cmd != nil {
		t.Errorf("expected nil Cmd from session-filter on unwired model, got %T", cmd)
	}
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("expected Model, got %T", next)
	}
	if nm.err != nil {
		t.Errorf("expected no err, got %v", nm.err)
	}
}

// TestDispatchAction_PluginRouting verifies the 3-segment plugin action ID
// routes to handlePluginRun and that the m.client=nil guard prevents any
// panic / spurious m.err when the daemon is unavailable.
func TestDispatchAction_PluginRouting(t *testing.T) {
	m := Model{deletingIDs: map[string]bool{}}
	next, cmd := m.dispatchAction(action.PluginActionID("notifier", "send-dm"))
	if cmd != nil {
		t.Errorf("expected nil Cmd, got %T", cmd)
	}
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("expected Model, got %T", next)
	}
	if nm.err != nil {
		t.Errorf("expected nil m.err on nil-client no-op, got %v", nm.err)
	}
}

// TestDispatchAction_LegacyTwoSegmentPluginIDIgnored guards the "silently
// ignore" contract for stale 2-segment IDs left in the tmux env by an older
// binary — the palette expects the 3-segment form now.
func TestDispatchAction_LegacyTwoSegmentPluginIDIgnored(t *testing.T) {
	sentinel := errors.New("pre-existing error")
	m := Model{deletingIDs: map[string]bool{}, err: sentinel}
	next, cmd := m.dispatchAction(action.PluginIDPrefix + "notifier")
	if cmd != nil {
		t.Errorf("expected nil Cmd, got %T", cmd)
	}
	nm := next.(Model)
	if !errors.Is(nm.err, sentinel) {
		t.Errorf("legacy 2-segment ID must not touch m.err, got %v", nm.err)
	}
}

// TestDispatchAction_UnknownID guards the "silently ignore" contract — a
// stale JIN_ACTION_ID env value must not wedge the TUI.
func TestDispatchAction_UnknownID(t *testing.T) {
	sentinel := errors.New("pre-existing error")
	m := Model{deletingIDs: map[string]bool{}, err: sentinel}
	next, cmd := m.dispatchAction("core:bogus")
	if cmd != nil {
		t.Errorf("expected nil Cmd for unknown id, got %T", cmd)
	}
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("expected Model, got %T", next)
	}
	if !errors.Is(nm.err, sentinel) {
		t.Errorf("dispatchAction should not touch m.err on unknown id, got %v", nm.err)
	}
}

// TestConfirmRequestForAction is where the kill/delete routing is actually
// asserted: the confirmation moved into a tmux popup, so dispatchAction's
// only observable effect for those IDs is an env write a test Model cannot
// see (tmuxClient=nil). dispatchAction hands the action ID straight to this
// resolver, so pinning the ID→dialog mapping here pins the routing — and the
// worktree upgrade of delete, which is the part most likely to break.
func TestConfirmRequestForAction(t *testing.T) {
	plain := []session.Info{{ID: "s1", Description: "one"}}
	worktree := []session.Info{{ID: "s1", Description: "one", IsWorktree: true}}

	cases := []struct {
		name     string
		actionID string
		sessions []session.Info
		wantMode string
	}{
		{"kill", action.IDKill, plain, ConfirmModeKill},
		{"kill on a worktree session is still a plain kill", action.IDKill, worktree, ConfirmModeKill},
		{"delete", action.IDDelete, plain, ConfirmModeDelete},
		{"delete on a worktree session asks about the worktree", action.IDDelete, worktree, ConfirmModeDeleteWorktree},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{sessions: tt.sessions, cursor: 0, deletingIDs: map[string]bool{}}
			req, ok := m.confirmRequestForAction(tt.actionID)
			if !ok {
				t.Fatalf("confirmRequestForAction(%q) = not ok, want a request", tt.actionID)
			}
			if req.mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", req.mode, tt.wantMode)
			}
			if req.targetID != "s1" {
				t.Errorf("targetID = %q, want %q", req.targetID, "s1")
			}
			if req.targetDesc != "one" {
				t.Errorf("targetDesc = %q, want %q", req.targetDesc, "one")
			}
		})
	}
}

// TestConfirmRequestForAction_NoTarget covers the guards that keep a prompt
// from naming a session that is not there (or is on its way out), plus the
// non-destructive action IDs that must not resolve to a dialog at all.
func TestConfirmRequestForAction_NoTarget(t *testing.T) {
	sessions := []session.Info{{ID: "s1", Description: "one"}}
	cases := []struct {
		name        string
		actionID    string
		sessions    []session.Info
		cursor      int
		deletingIDs map[string]bool
	}{
		{"empty list", action.IDDelete, nil, 0, map[string]bool{}},
		{"cursor past end", action.IDDelete, sessions, 3, map[string]bool{}},
		{"target already deleting", action.IDDelete, sessions, 0, map[string]bool{"s1": true}},
		{"non-destructive action", action.IDRefresh, sessions, 0, map[string]bool{}},
		{"unknown action", "core:bogus", sessions, 0, map[string]bool{}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{sessions: tt.sessions, cursor: tt.cursor, deletingIDs: tt.deletingIDs}
			if req, ok := m.confirmRequestForAction(tt.actionID); ok {
				t.Errorf("confirmRequestForAction = %+v, want no request", req)
			}
		})
	}
}

// TestDispatchAction_DestructiveActionsAreSafeWhenUnwired pins the degraded
// path the palette can hit on an unwired Model: IDKill / IDDelete must reach
// handleDestructiveAction and stop at the tmuxClient=nil guard rather than
// panicking or surfacing an error. The mapping itself is covered by
// TestConfirmRequestForAction.
func TestDispatchAction_DestructiveActionsAreSafeWhenUnwired(t *testing.T) {
	for _, id := range []string{action.IDKill, action.IDDelete} {
		t.Run(id, func(t *testing.T) {
			m := Model{
				sessions:    []session.Info{{ID: "s1", Description: "one"}},
				cursor:      0,
				deletingIDs: map[string]bool{},
			}
			next, cmd := m.dispatchAction(id)
			if cmd != nil {
				t.Errorf("expected nil Cmd (the popup is opened synchronously), got %T", cmd)
			}
			nm, ok := next.(Model)
			if !ok {
				t.Fatalf("expected Model, got %T", next)
			}
			if nm.err != nil {
				t.Errorf("expected no err, got %v", nm.err)
			}
			if len(nm.deletingIDs) != 0 {
				t.Errorf("opening a confirmation must not delete anything, got %v", nm.deletingIDs)
			}
		})
	}
}

// TestDispatchAction_RefreshReturnsCmd asserts IDRefresh routes to handleRefresh
// by observing the non-nil fetchSessions Cmd it returns.
func TestDispatchAction_RefreshReturnsCmd(t *testing.T) {
	m := Model{}
	_, cmd := m.dispatchAction(action.IDRefresh)
	if cmd == nil {
		t.Fatal("expected non-nil Cmd for IDRefresh (fetchSessions)")
	}
}

// --- dispatchConfirmResult ---

// TestDispatchConfirmResult_Routing pins the mode/result table, and in
// particular the half of it that must do nothing: dispatchConfirmResult is fed
// from tmux env that can go stale, and every acting branch destroys a session.
// Cases are separated by the Cmd (issued vs not) plus the state each acting
// branch leaves behind; which flags reach daemon.Client.Delete is pinned by
// TestConfirmFlagsOnWire below.
func TestDispatchConfirmResult_Routing(t *testing.T) {
	cases := []struct {
		name            string
		mode            string
		targetID        string
		result          string
		wantCmd         bool
		wantDeleting    bool
		wantProcessing  string
		wantPendingKill string
	}{
		{name: "kill yes", mode: ConfirmModeKill, targetID: "s1", result: ConfirmResultYes,
			wantCmd: true, wantProcessing: "Stopping...", wantPendingKill: "s1"},
		{name: "delete yes", mode: ConfirmModeDelete, targetID: "s1", result: ConfirmResultYes,
			wantCmd: true, wantDeleting: true},
		{name: "delete_worktree yes (session only)", mode: ConfirmModeDeleteWorktree, targetID: "s1", result: ConfirmResultYes,
			wantCmd: true, wantDeleting: true},
		{name: "delete_worktree worktree", mode: ConfirmModeDeleteWorktree, targetID: "s1", result: ConfirmResultWorktree,
			wantCmd: true, wantDeleting: true},
		{name: "force yes", mode: ConfirmModeDeleteWorktreeForce, targetID: "s1", result: ConfirmResultForceYes,
			wantCmd: true, wantDeleting: true},
		{name: "force no falls back to session only", mode: ConfirmModeDeleteWorktreeForce, targetID: "s1", result: ConfirmResultForceNo,
			wantCmd: true, wantDeleting: true},

		// Everything below must leave the session alone.
		{name: "kill no", mode: ConfirmModeKill, targetID: "s1", result: ConfirmResultNo},
		{name: "delete no", mode: ConfirmModeDelete, targetID: "s1", result: ConfirmResultNo},
		{name: "delete_worktree no", mode: ConfirmModeDeleteWorktree, targetID: "s1", result: ConfirmResultNo},
		{name: "worktree answer to a plain delete prompt", mode: ConfirmModeDelete, targetID: "s1", result: ConfirmResultWorktree},
		{name: "force answer to a plain delete prompt", mode: ConfirmModeDelete, targetID: "s1", result: ConfirmResultForceYes},
		{name: "plain yes to a force prompt", mode: ConfirmModeDeleteWorktreeForce, targetID: "s1", result: ConfirmResultYes},
		{name: "unknown mode", mode: "delete_everything", targetID: "s1", result: ConfirmResultYes},
		{name: "unknown result", mode: ConfirmModeDelete, targetID: "s1", result: "sure"},
		{name: "empty target", mode: ConfirmModeDelete, targetID: "", result: ConfirmResultYes},
		{name: "all empty", mode: "", targetID: "", result: ""},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				sessions:    []session.Info{{ID: "s1", Description: "one", IsWorktree: true}},
				cursor:      0,
				deletingIDs: map[string]bool{},
				height:      100,
			}
			next, cmd := m.dispatchConfirmResult(tt.mode, tt.targetID, tt.result)
			nm := next.(Model)
			if (cmd != nil) != tt.wantCmd {
				t.Errorf("Cmd issued = %v, want %v", cmd != nil, tt.wantCmd)
			}
			if nm.deletingIDs["s1"] != tt.wantDeleting {
				t.Errorf("deletingIDs[s1] = %v, want %v", nm.deletingIDs["s1"], tt.wantDeleting)
			}
			if nm.processingMsg != tt.wantProcessing {
				t.Errorf("processingMsg = %q, want %q", nm.processingMsg, tt.wantProcessing)
			}
			if nm.pendingKillID != tt.wantPendingKill {
				t.Errorf("pendingKillID = %q, want %q", nm.pendingKillID, tt.wantPendingKill)
			}
		})
	}
}

// fakeDaemon is a minimal daemon speaking the real protocol over a real Unix
// socket, so a tea.Cmd built by the Model can be run for its wire effect.
// Modelled on internal/daemon's fakeServerWith; it lives here because the
// distinction this pins — which booleans reach daemon.Client.Delete — is not
// observable in the Model's own state.
type fakeDaemon struct {
	mu       sync.Mutex
	requests []daemon.Request
}

func startFakeDaemon(t *testing.T) (*fakeDaemon, *daemon.Client) {
	t.Helper()
	sock := testutil.SocketPath(t, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	d := &fakeDaemon{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			d.serve(conn)
		}
	}()
	return d, daemon.NewClient(sock)
}

func (d *fakeDaemon) serve(conn net.Conn) {
	defer conn.Close()
	var req daemon.Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	d.mu.Lock()
	d.requests = append(d.requests, req)
	d.mu.Unlock()

	resp := daemon.Response{ProtocolVersion: daemon.ProtocolVersion, Success: true}
	switch req.Action {
	case "list":
		resp.Data = json.RawMessage("[]")
	case "attention-seen":
		// The client decodes this one into a session.Info, so an empty Data
		// would fail the call rather than exercise it.
		resp.Data = json.RawMessage("{}")
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

// first returns the request that opened the exchange — the kill or delete.
// (Both Cmds follow up with a "list" to refresh the model.)
func (d *fakeDaemon) first(t *testing.T) daemon.Request {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) == 0 {
		t.Fatal("no request reached the daemon")
	}
	return d.requests[0]
}

// TestConfirmFlagsOnWire pins what each approved answer actually asks the
// daemon to destroy. removeWorktree and force are invisible in Model state —
// every delete branch looks identical from the outside — yet force is the flag
// that discards uncommitted work, so the only place the distinction survives
// is the request on the socket.
func TestConfirmFlagsOnWire(t *testing.T) {
	cases := []struct {
		name         string
		mode         string
		result       string
		wantAction   string
		wantWorktree bool
		wantForce    bool
	}{
		{"kill", ConfirmModeKill, ConfirmResultYes, "kill", false, false},
		{"delete", ConfirmModeDelete, ConfirmResultYes, "delete", false, false},
		{"worktree prompt, session only", ConfirmModeDeleteWorktree, ConfirmResultYes, "delete", false, false},
		{"worktree prompt, with worktree", ConfirmModeDeleteWorktree, ConfirmResultWorktree, "delete", true, false},
		{"force prompt, forced", ConfirmModeDeleteWorktreeForce, ConfirmResultForceYes, "delete", true, true},
		{"force prompt, declined", ConfirmModeDeleteWorktreeForce, ConfirmResultForceNo, "delete", false, false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			fake, client := startFakeDaemon(t)
			m := Model{
				client:      client,
				sessions:    []session.Info{{ID: "s1", Description: "one", IsWorktree: true}},
				deletingIDs: map[string]bool{},
				height:      100,
			}

			_, cmd := m.dispatchConfirmResult(tt.mode, "s1", tt.result)
			if cmd == nil {
				t.Fatalf("%s/%s issued no Cmd", tt.mode, tt.result)
			}
			cmd()

			got := fake.first(t)
			if got.Action != tt.wantAction {
				t.Fatalf("action = %q, want %q", got.Action, tt.wantAction)
			}
			if got.Action == "kill" {
				var kr daemon.IDRequest
				if err := json.Unmarshal(got.Data, &kr); err != nil {
					t.Fatalf("decode kill request: %v", err)
				}
				if kr.ID != "s1" {
					t.Errorf("kill ID = %q, want s1", kr.ID)
				}
				return
			}
			var dr daemon.DeleteRequest
			if err := json.Unmarshal(got.Data, &dr); err != nil {
				t.Fatalf("decode delete request: %v", err)
			}
			if dr.ID != "s1" {
				t.Errorf("delete ID = %q, want s1", dr.ID)
			}
			if dr.RemoveWorktree != tt.wantWorktree {
				t.Errorf("remove_worktree = %v, want %v", dr.RemoveWorktree, tt.wantWorktree)
			}
			if dr.ForceRemoveWorktree != tt.wantForce {
				t.Errorf("force_remove_worktree = %v, want %v", dr.ForceRemoveWorktree, tt.wantForce)
			}
		})
	}
}

// TestHandleEnvTick_ConfirmAnswerReachesDaemon walks the whole parent half of
// the handshake in one go: an answer sitting in the tmux env is drained,
// routed, and turned into the real delete on the daemon socket.
//
// The unit tests around it each pin one link (consumeEnvRequests reads the
// keys, dispatchConfirmResult picks the branch, TestConfirmFlagsOnWire pins the
// flags) but none of them pins that the links are connected: with the confirm
// block deleted from the tick, every one of them still passes and the feature
// is simply off. This test is the one that notices.
func TestHandleEnvTick_ConfirmAnswerReachesDaemon(t *testing.T) {
	fake, client := startFakeDaemon(t)
	env := map[string]string{
		EnvConfirmResult:     ConfirmResultWorktree,
		EnvConfirmMode:       ConfirmModeDeleteWorktree,
		EnvConfirmTargetID:   "s1",
		EnvConfirmTargetDesc: "one",
		// An unresolvable focus request rides along on the same tick, which is
		// what pins the confirm block's *position*: it makes the fast path below
		// it take its early return (moveCursorToSession fails on an ID the list
		// does not have). Reorder the two and the approved delete never reaches
		// the wire — the tick returns the fetch instead. Without this key the
		// fast path always succeeds and the ordering is unguarded.
		"JIN_FOCUS_SESSION": "not-in-list",
	}
	var unset []string
	m := Model{
		client:      client,
		sessions:    []session.Info{{ID: "s1", Description: "one", IsWorktree: true}},
		deletingIDs: map[string]bool{},
		height:      100,
	}

	next, cmd := m.handleEnvTick(env, func(key string) {
		unset = append(unset, key)
		delete(env, key)
	})
	if cmd == nil {
		t.Fatal("an approved delete waiting in the env produced no Cmd")
	}
	if !next.(Model).deletingIDs["s1"] {
		t.Error("deletingIDs[s1] = false, want true (the list greys the target out while the delete runs)")
	}

	// The tick re-arms itself alongside the work it dispatched, so the Cmd is
	// a Batch. Running its members is the only way to tell them apart; one of
	// them is envTickCmd's 250ms timer, which is why this test is not instant.
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Cmd returned %T, want tea.BatchMsg (the re-armed tick plus the delete)", msg)
	}
	for _, c := range batch {
		c()
	}

	got := fake.first(t)
	if got.Action != "delete" {
		t.Fatalf("action on the wire = %q, want %q", got.Action, "delete")
	}
	var dr daemon.DeleteRequest
	if err := json.Unmarshal(got.Data, &dr); err != nil {
		t.Fatalf("decode delete request: %v", err)
	}
	if dr.ID != "s1" || !dr.RemoveWorktree || dr.ForceRemoveWorktree {
		t.Errorf("delete request = %+v, want id=s1 remove_worktree=true force=false", dr)
	}
	// The answer and its prompt must leave the tmux env on this same tick, or
	// the next TUI process replays the approval against whatever it finds.
	assertConsumedSet(t, unset, "JIN_FOCUS_SESSION", EnvConfirmResult, EnvConfirmMode, EnvConfirmTargetID, EnvConfirmTargetDesc)
}

// --- display-pane hand-off around delete ---
//
// These pin the state machine that drives the pane off a session being
// deleted. The tmux-side half (resetting the pane label) needs a live server
// and lives in model_tmux_e2e_test.go; see docs/gotchas.md for why both
// halves are needed.

// TestDeleteSession_MovesDisplayOffTarget pins that the pane stops claiming a
// session at the moment the delete is issued, not a poll after it lands.
func TestDeleteSession_MovesDisplayOffTarget(t *testing.T) {
	tests := []struct {
		name     string
		sessions []session.Info
		shown    string
		want     string
	}{
		{
			name:     "deleting the only session drops the pane to the placeholder",
			sessions: []session.Info{{ID: "s1", Description: "one"}},
			shown:    "s1",
			want:     placeholderSessionID,
		},
		{
			name: "deleting the shown session hands the pane to the survivor",
			sessions: []session.Info{
				{ID: "s1", Description: "one"},
				{ID: "s2", Description: "two", Status: session.StatusIdle, TmuxWindowName: "jin-s2"},
			},
			shown: "s1",
			// switchToSession is a no-op without an outer tmux client, so the
			// observable part here is only that the pane stopped claiming s1.
			want: "",
		},
		{
			name: "deleting a session the pane is not showing leaves it alone",
			sessions: []session.Info{
				{ID: "s1", Description: "one"},
				{ID: "s2", Description: "two", Status: session.StatusIdle, TmuxWindowName: "jin-s2"},
			},
			shown: "s2",
			want:  "s2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, client := startFakeDaemon(t)
			m := Model{
				client:           client,
				sessions:         tt.sessions,
				cursor:           0,
				deletingIDs:      map[string]bool{},
				height:           100,
				currentSessionID: tt.shown,
			}
			next, cmd := m.deleteSession("s1", false, false)
			if cmd == nil {
				t.Fatal("deleteSession issued no Cmd")
			}
			if got := next.(Model).currentSessionID; got != tt.want {
				t.Errorf("currentSessionID = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestShowCursorSession_PlaceholderWhenCursorHasNoTarget covers the two ways
// the cursor can point at nothing attachable. The second one is the reason the
// deleting check cannot be dropped: without it the pane re-attaches to the
// session the daemon is in the middle of tearing down.
func TestShowCursorSession_PlaceholderWhenCursorHasNoTarget(t *testing.T) {
	tests := []struct {
		name        string
		sessions    []session.Info
		deletingIDs map[string]bool
	}{
		{
			name:        "empty list",
			sessions:    nil,
			deletingIDs: map[string]bool{},
		},
		{
			name:        "the only session is being deleted",
			sessions:    []session.Info{{ID: "s1", Description: "one"}},
			deletingIDs: map[string]bool{"s1": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				sessions:         tt.sessions,
				cursor:           0,
				deletingIDs:      tt.deletingIDs,
				height:           100,
				currentSessionID: "s1",
			}
			m.showCursorSession(false)
			if m.currentSessionID != placeholderSessionID {
				t.Errorf("currentSessionID = %q, want %q", m.currentSessionID, placeholderSessionID)
			}
		})
	}
}

// TestDisplaysLiveSession pins the predicate the sessionsMsg tail uses to
// decide whether the pane needs re-pointing. The placeholder case is the one
// that keeps a re-attach possible: treat the sentinel as "live" and a session
// created after the list went empty would never reach the pane.
func TestDisplaysLiveSession(t *testing.T) {
	sessions := []session.Info{{ID: "s1"}, {ID: "s2"}}
	tests := []struct {
		name  string
		shown string
		want  bool
	}{
		{"showing a session still in the list", "s1", true},
		{"showing a session that vanished", "gone", false},
		{"showing the placeholder", placeholderSessionID, false},
		{"nothing decided yet", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{sessions: sessions, currentSessionID: tt.shown}
			if got := m.displaysLiveSession(); got != tt.want {
				t.Errorf("displaysLiveSession() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSessionsMsg_LastDeleteSettlesOnPlaceholder walks the tail of the flow:
// the record is gone from the list, so the grey-out clears and the pane must
// end on the placeholder rather than still naming the session that just left.
func TestSessionsMsg_LastDeleteSettlesOnPlaceholder(t *testing.T) {
	m := Model{
		sessions:         []session.Info{{ID: "s1", Description: "one"}},
		cursor:           0,
		deletingIDs:      map[string]bool{"s1": true},
		height:           100,
		currentSessionID: "s1",
	}
	next, _ := m.updateListMode(sessionsMsg(nil))
	nm := next.(Model)
	if nm.currentSessionID != placeholderSessionID {
		t.Errorf("currentSessionID = %q, want %q", nm.currentSessionID, placeholderSessionID)
	}
	if len(nm.deletingIDs) != 0 {
		t.Errorf("deletingIDs = %v, want empty (the record is gone)", nm.deletingIDs)
	}
}

// TestSessionsMsg_KillArmSurvivesStaleList is the regression this arm exists
// for. The session poll runs on its own clock, so a List() that started before
// the daemon saw the Kill delivers a snapshot where the target is still
// running. Spending the reswitch on that snapshot re-points the pane at the
// session being killed, and the sessionsMsg carrying the real outcome then
// finds nothing to act on: the killed record stays in the list, so
// displaysLiveSession() reports the pane as settled.
func TestSessionsMsg_KillArmSurvivesStaleList(t *testing.T) {
	running := []session.Info{
		{ID: "s1", Description: "one", Status: session.StatusRunning, TmuxWindowName: "sess-s1"},
	}
	m := Model{
		sessions:         running,
		cursor:           0,
		deletingIDs:      map[string]bool{},
		height:           100,
		currentSessionID: "s1",
		pendingKillID:    "s1",
	}

	next, _ := m.updateListMode(sessionsMsg(running))
	stale := next.(Model)
	if stale.pendingKillID != "s1" {
		t.Fatalf("pendingKillID = %q, want %q — a snapshot that predates the kill spent the reswitch",
			stale.pendingKillID, "s1")
	}
	if stale.currentSessionID != "s1" {
		t.Errorf("currentSessionID = %q, want %q — the pane was re-pointed off a stale snapshot",
			stale.currentSessionID, "s1")
	}

	// The kill's own List(): the record is still listed, now stopped.
	next, _ = stale.updateListMode(sessionsMsg([]session.Info{
		{ID: "s1", Description: "one", Status: session.StatusStopped},
	}))
	fresh := next.(Model)
	if fresh.pendingKillID != "" {
		t.Errorf("pendingKillID = %q, want empty — the list confirmed the kill", fresh.pendingKillID)
	}
	if fresh.currentSessionID == "s1" {
		t.Error("the pane still claims the killed session; its attach is dead underneath")
	}
}

// TestSessionsMsg_KillArmSurvivesFocusFastPath covers the other way the arm
// could be spent without the re-point it was armed for: the focus fast path
// returns from the middle of the sessionsMsg branch, so anything resolved
// above it is silently dropped on a list that arrives while a create is still
// looking for its new session.
func TestSessionsMsg_KillArmSurvivesFocusFastPath(t *testing.T) {
	m := Model{
		sessions:         []session.Info{{ID: "s1", Description: "one", Status: session.StatusRunning}},
		cursor:           0,
		deletingIDs:      map[string]bool{},
		height:           100,
		currentSessionID: "s1",
		pendingKillID:    "s1",
		focusSessionID:   "s2", // created, not in the list yet
	}
	// The same snapshot both times: what changes is that focus has given up by
	// the second one, so it is the arm that decides whether the pane moves.
	stopped := sessionsMsg([]session.Info{{ID: "s1", Description: "one", Status: session.StatusStopped}})

	next, _ := m.updateListMode(stopped)
	focused := next.(Model)
	if focused.currentSessionID != "s1" {
		t.Fatalf("currentSessionID = %q, want %q — the focus path re-pointed the pane",
			focused.currentSessionID, "s1")
	}
	if focused.pendingKillID != "s1" {
		t.Fatalf("pendingKillID = %q, want %q — the arm was spent on a list that returned early",
			focused.pendingKillID, "s1")
	}

	// Focus gave up, so the next list is free to settle the pane.
	next, _ = focused.updateListMode(stopped)
	settled := next.(Model)
	if settled.pendingKillID != "" {
		t.Errorf("pendingKillID = %q, want empty", settled.pendingKillID)
	}
	if settled.currentSessionID == "s1" {
		t.Error("the pane still claims the killed session; its attach is dead underneath")
	}
}

// TestSessionsMsg_KillOfHiddenSessionKeepsAttach pins how far force reaches.
// A kill only makes the pane *look* settled when the pane is showing the
// session that was killed; forcing on any other kill tears down a live attach
// the user is watching and rebuilds it as a visible flash.
func TestSessionsMsg_KillOfHiddenSessionKeepsAttach(t *testing.T) {
	m := Model{
		sessions: []session.Info{
			{ID: "s1", Description: "one", Status: session.StatusRunning},
			{ID: "s2", Description: "two", Status: session.StatusRunning, TmuxWindowName: "sess-s2"},
		},
		cursor:           1, // parked on the session the pane shows
		deletingIDs:      map[string]bool{},
		height:           100,
		currentSessionID: "s2",
		pendingKillID:    "s1",
	}
	next, _ := m.updateListMode(sessionsMsg([]session.Info{
		{ID: "s1", Description: "one", Status: session.StatusStopped},
		{ID: "s2", Description: "two", Status: session.StatusRunning, TmuxWindowName: "sess-s2"},
	}))
	nm := next.(Model)
	if nm.pendingKillID != "" {
		t.Errorf("pendingKillID = %q, want empty — the kill landed", nm.pendingKillID)
	}
	if nm.currentSessionID != "s2" {
		t.Errorf("currentSessionID = %q, want %q — s2's live attach was torn down over a kill of s1",
			nm.currentSessionID, "s2")
	}
}

// TestSessionsMsg_OverlappingKillsStillFreeThePane pins why force keys off the
// pane rather than off the arm. One arm slot keeps only the newest kill, so a
// second kill can confirm while the pane is still parked on the first one's
// victim — and "was this the newest kill's target?" answers no there, leaving
// the pane on a dead attach that nothing else will ever re-point.
func TestSessionsMsg_OverlappingKillsStillFreeThePane(t *testing.T) {
	both := []session.Info{
		{ID: "s1", Description: "one", Status: session.StatusStopped},
		{ID: "s2", Description: "two", Status: session.StatusStopped},
	}
	m := Model{
		sessions:         both,
		cursor:           0, // back on the first victim, which the pane still shows
		deletingIDs:      map[string]bool{},
		height:           100,
		currentSessionID: "s1",
		pendingKillID:    "s2", // the second kill overwrote the first one's arm
	}
	next, _ := m.updateListMode(sessionsMsg(both))
	nm := next.(Model)
	if nm.pendingKillID != "" {
		t.Errorf("pendingKillID = %q, want empty — the kill landed", nm.pendingKillID)
	}
	if nm.currentSessionID == "s1" {
		t.Error("the pane still claims s1, whose attach died with the kill that lost the arm slot")
	}
}

// TestSessionsMsg_KillArmClearsWhenRecordVanishes keeps the arm from
// outliving its kill in the one case where the target never reports a stopped
// status: the record left the list entirely (deleted from elsewhere).
func TestSessionsMsg_KillArmClearsWhenRecordVanishes(t *testing.T) {
	m := Model{
		sessions:         []session.Info{{ID: "s1", Description: "one", Status: session.StatusRunning}},
		cursor:           0,
		deletingIDs:      map[string]bool{},
		height:           100,
		currentSessionID: "s1",
		pendingKillID:    "s1",
	}
	next, _ := m.updateListMode(sessionsMsg(nil))
	nm := next.(Model)
	if nm.pendingKillID != "" {
		t.Errorf("pendingKillID = %q, want empty — the record is gone, nothing will ever confirm it", nm.pendingKillID)
	}
	if nm.currentSessionID != placeholderSessionID {
		t.Errorf("currentSessionID = %q, want %q", nm.currentSessionID, placeholderSessionID)
	}
}

// TestSessionsMsg_KillArmSurvivesListError covers the other order the daemon
// can answer in: the kill's own List() failed, so the outcome arrives on a
// later poll instead. The error must not disarm the pending reswitch.
func TestSessionsMsg_KillArmSurvivesListError(t *testing.T) {
	m := Model{
		sessions:         []session.Info{{ID: "s1", Description: "one", Status: session.StatusRunning}},
		cursor:           0,
		deletingIDs:      map[string]bool{},
		height:           100,
		currentSessionID: "s1",
		pendingKillID:    "s1",
	}
	next, _ := m.updateListMode(errMsg(errors.New("list failed")))
	errored := next.(Model)
	if errored.pendingKillID != "s1" {
		t.Fatalf("pendingKillID = %q, want %q — an unrelated error disarmed the reswitch",
			errored.pendingKillID, "s1")
	}

	next, _ = errored.updateListMode(sessionsMsg([]session.Info{
		{ID: "s1", Description: "one", Status: session.StatusStopped},
	}))
	if got := next.(Model).pendingKillID; got != "" {
		t.Errorf("pendingKillID = %q, want empty — the next poll confirmed the kill", got)
	}
}

// TestSessionsMsg_PlaceholderReattachesWhenSessionAppears guards the recovery
// direction: the sentinel must not be sticky, or the pane stays blank forever
// after the list has been emptied once.
func TestSessionsMsg_PlaceholderReattachesWhenSessionAppears(t *testing.T) {
	m := Model{
		cursor:           0,
		deletingIDs:      map[string]bool{},
		height:           100,
		currentSessionID: placeholderSessionID,
	}
	next, _ := m.updateListMode(sessionsMsg([]session.Info{
		{ID: "s1", Description: "one", Status: session.StatusIdle, TmuxWindowName: "jin-s1"},
	}))
	if got := next.(Model).currentSessionID; got == placeholderSessionID {
		t.Error("currentSessionID stayed on the placeholder; a new session never reaches the pane")
	}
}

func TestCurrentCursorSessionID_Cursor(t *testing.T) {
	m := Model{
		sessions: []session.Info{
			{ID: "s1", Description: "one"},
			{ID: "s2", Description: "two"},
			{ID: "s3", Description: "three"},
		},
		cursor:      1,
		deletingIDs: map[string]bool{},
	}
	if got := m.currentCursorSessionID(); got != "s2" {
		t.Errorf("cursor=1 → %q, want %q", got, "s2")
	}
}

func TestCurrentCursorSessionID_Deleting(t *testing.T) {
	m := Model{
		sessions: []session.Info{
			{ID: "s1", Description: "one"},
			{ID: "s2", Description: "two"},
		},
		cursor:      1,
		deletingIDs: map[string]bool{"s2": true},
	}
	if got := m.currentCursorSessionID(); got != "" {
		t.Errorf("cursor on deleting session → %q, want empty", got)
	}
}

func TestCurrentCursorSessionID_EmptyList(t *testing.T) {
	m := Model{
		sessions:    nil,
		cursor:      0,
		deletingIDs: map[string]bool{},
	}
	if got := m.currentCursorSessionID(); got != "" {
		t.Errorf("empty list → %q, want empty", got)
	}
}

// TestWriteCursorEnv_UpdatesTmux is a degraded guard test: with tmuxClient=nil
// (legacy mode / tests), writeCursorEnv must be a no-op. Real SetEnvironment
// wiring is covered only by manual verification, not by this suite.
func TestWriteCursorEnv_UpdatesTmux(t *testing.T) {
	m := Model{
		sessions: []session.Info{
			{ID: "s1", Description: "one"},
		},
		cursor:      0,
		deletingIDs: map[string]bool{},
	}
	// Must not panic with a nil client.
	m.writeCursorEnv()
}

// Pin the tick intervals so a stray edit that lengthens envTickInterval (which
// would re-introduce the popup pickup lag this split was built to remove) or
// shortens sessionTickInterval (which would raise daemon-refetch churn) fails
// loudly instead of drifting silently.
func TestEnvTickInterval(t *testing.T) {
	if envTickInterval != 250*time.Millisecond {
		t.Errorf("envTickInterval = %v, want 250ms", envTickInterval)
	}
}

// --- confirm handshake env contract ---

// fakeEnvConsumer stands in for the envTick consume closure over a fixed env
// map: it returns each key's value once, records the read, and drops the key
// so a second read comes back empty — the same shape as the real closure,
// which unsets the key on tmux.
type fakeEnvConsumer struct {
	env      map[string]string
	consumed []string
}

func (f *fakeEnvConsumer) consume(key string) string {
	v := f.env[key]
	if v != "" {
		f.consumed = append(f.consumed, key)
		delete(f.env, key)
	}
	return v
}

func TestConsumeEnvRequests_FocusSession(t *testing.T) {
	f := &fakeEnvConsumer{env: map[string]string{"JIN_FOCUS_SESSION": "sess-xyz"}}

	req := consumeEnvRequests(f.consume)

	if req.focusSessionID != "sess-xyz" {
		t.Errorf("focusSessionID = %q, want %q", req.focusSessionID, "sess-xyz")
	}
	if req.answer.result != "" {
		t.Errorf("answer.result = %q, want empty (no confirm result in env)", req.answer.result)
	}
	if len(f.consumed) != 1 || f.consumed[0] != "JIN_FOCUS_SESSION" {
		t.Errorf("consumed = %v, want [JIN_FOCUS_SESSION]", f.consumed)
	}
}

func TestConsumeEnvRequests_EmptyEnvIsNoOp(t *testing.T) {
	f := &fakeEnvConsumer{env: map[string]string{}}

	req := consumeEnvRequests(f.consume)

	if req != (envRequests{}) {
		t.Errorf("consumeEnvRequests on an empty env = %+v, want zero value", req)
	}
	if len(f.consumed) != 0 {
		t.Errorf("consumed = %v, want none", f.consumed)
	}
}

// TestConsumeEnvRequests_ConfirmAnswer pins that an answered popup comes back
// whole and that all four JIN_CONFIRM_* keys leave the tmux env on the same
// tick. Anything left behind either re-applies on a later tick or mislabels
// the next prompt, and every result here destroys a session.
func TestConsumeEnvRequests_ConfirmAnswer(t *testing.T) {
	f := &fakeEnvConsumer{env: map[string]string{
		EnvConfirmResult:     ConfirmResultWorktree,
		EnvConfirmMode:       ConfirmModeDeleteWorktree,
		EnvConfirmTargetID:   "s1",
		EnvConfirmTargetDesc: "one",
	}}

	req := consumeEnvRequests(f.consume)

	want := confirmAnswer{mode: ConfirmModeDeleteWorktree, targetID: "s1", result: ConfirmResultWorktree}
	if req.answer != want {
		t.Errorf("answer = %+v, want %+v", req.answer, want)
	}
	assertConsumedSet(t, f.consumed, EnvConfirmResult, EnvConfirmMode, EnvConfirmTargetID, EnvConfirmTargetDesc)
}

// TestConsumeEnvRequests_FocusAndConfirmSameTick is the ordering invariant:
// the confirm answer is drained on the same pass as the focus IDs, before the
// caller's focus handling gets a chance to return early from the tick. Moving
// the confirm read after that early return would strand an approved
// destructive request in a tmux server that outlives this TUI process, and the
// next TUI would replay it.
func TestConsumeEnvRequests_FocusAndConfirmSameTick(t *testing.T) {
	f := &fakeEnvConsumer{env: map[string]string{
		"JIN_FOCUS_SESSION":  "sess-xyz",
		EnvConfirmResult:     ConfirmResultYes,
		EnvConfirmMode:       ConfirmModeDelete,
		EnvConfirmTargetID:   "s1",
		EnvConfirmTargetDesc: "one",
	}}

	req := consumeEnvRequests(f.consume)

	if req.focusSessionID != "sess-xyz" {
		t.Errorf("focusSessionID = %q, want %q", req.focusSessionID, "sess-xyz")
	}
	if req.answer.result != ConfirmResultYes {
		t.Errorf("answer.result = %q, want %q — the confirm answer must not wait for a later tick",
			req.answer.result, ConfirmResultYes)
	}
	assertConsumedSet(t, f.consumed,
		"JIN_FOCUS_SESSION", EnvConfirmResult, EnvConfirmMode, EnvConfirmTargetID, EnvConfirmTargetDesc)
	if len(f.env) != 0 {
		t.Errorf("keys left in env = %v, want none", f.env)
	}
}

// TestConsumeEnvRequests_DismissedPopup covers Ctrl+C: the popup writes no
// result, so there is nothing to act on and the prompt keys stay put for the
// next writeConfirmRequest (or the startup clear) to wipe.
func TestConsumeEnvRequests_DismissedPopup(t *testing.T) {
	f := &fakeEnvConsumer{env: map[string]string{
		EnvConfirmMode:       ConfirmModeDelete,
		EnvConfirmTargetID:   "s1",
		EnvConfirmTargetDesc: "one",
	}}

	req := consumeEnvRequests(f.consume)

	if req.answer.result != "" {
		t.Errorf("answer.result = %q, want empty — a dismissed popup approves nothing", req.answer.result)
	}
	if len(f.consumed) != 0 {
		t.Errorf("consumed = %v, want none", f.consumed)
	}
}

func assertConsumedSet(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("consumed = %v, want exactly %v", got, want)
	}
	for _, key := range want {
		if !slices.Contains(got, key) {
			t.Errorf("consumed = %v, missing %q", got, key)
		}
	}
}

// recordingEnv captures the outer-tmux env writes of the confirm handshake in
// order, standing in for *tmux.Client. It also applies them to env, so a test
// can ask what an interrupted handshake left behind on the tmux server rather
// than only what it attempted; failSet / failUnset name a key whose
// SetEnvironment / UnsetEnvironment fails, standing in for a tmux command that
// errors mid-handshake. Both halves need a failure injector: the write is
// fail-closed only if a failed *clear* also stops it, since a clear that gave
// up early is exactly how a stale key survives into the next request.
type recordingEnv struct {
	sessions  []string
	ops       []envOp
	env       map[string]string
	failSet   string
	failUnset string
}

type envOp struct {
	name  string
	value string
	unset bool
}

func (r *recordingEnv) SetEnvironment(session, name, value string) error {
	r.sessions = append(r.sessions, session)
	r.ops = append(r.ops, envOp{name: name, value: value})
	if name == r.failSet {
		return fmt.Errorf("set-environment %s: refused", name)
	}
	if r.env == nil {
		r.env = map[string]string{}
	}
	r.env[name] = value
	return nil
}

func (r *recordingEnv) UnsetEnvironment(session, name string) error {
	r.sessions = append(r.sessions, session)
	r.ops = append(r.ops, envOp{name: name, unset: true})
	if name == r.failUnset {
		return fmt.Errorf("set-environment -u %s: refused", name)
	}
	delete(r.env, name)
	return nil
}

// TestWriteConfirmRequest_ClearsEverythingFirst is the pairing guarantee: the
// whole handshake must be gone before any of this prompt's values land. An
// unconsumed answer would otherwise pair with the target being written here,
// and a dismissed prompt's leftovers (Ctrl+C writes no result, so its mode and
// target stay put by design) would survive a partial write and pair with it.
func TestWriteConfirmRequest_ClearsEverythingFirst(t *testing.T) {
	r := &recordingEnv{}

	if err := writeConfirmRequest(r, confirmRequest{mode: ConfirmModeKill, targetID: "s1", targetDesc: "one"}); err != nil {
		t.Fatalf("writeConfirmRequest: %v", err)
	}

	var cleared []string
	for _, op := range r.ops {
		if !op.unset {
			break
		}
		cleared = append(cleared, op.name)
	}
	for _, key := range []string{EnvConfirmResult, EnvConfirmMode, EnvConfirmTargetID, EnvConfirmTargetDesc} {
		if !slices.Contains(cleared, key) {
			t.Errorf("%s was not unset before the first value landed (leading unsets: %v)", key, cleared)
		}
	}
	for _, s := range r.sessions {
		if s != tmux.SessionName {
			t.Errorf("wrote to tmux session %q, want %q", s, tmux.SessionName)
		}
	}
}

// TestWriteConfirmRequest_Values pins each field to its own key. A swap here
// shows the user one session's name while dispatchConfirmResult deletes
// another, and both prompt and answer look internally consistent.
func TestWriteConfirmRequest_Values(t *testing.T) {
	r := &recordingEnv{}

	if err := writeConfirmRequest(r, confirmRequest{
		mode:       ConfirmModeDeleteWorktree,
		targetID:   "3f9c-id",
		targetDesc: "my-session",
	}); err != nil {
		t.Fatalf("writeConfirmRequest: %v", err)
	}

	want := map[string]string{
		EnvConfirmMode:       ConfirmModeDeleteWorktree,
		EnvConfirmTargetID:   "3f9c-id",
		EnvConfirmTargetDesc: "my-session",
	}
	if !maps.Equal(r.env, want) {
		t.Errorf("env after write = %v, want %v", r.env, want)
	}
}

// TestWriteConfirmRequest_PartialWriteLeavesNothingStale is the fail-closed
// property, driven through the exact sequence that made it necessary: a prompt
// for session A was dismissed with Ctrl+C, so A's mode and target are still in
// the tmux env, and the write for session B fails halfway. Without the leading
// clear (or with the errors discarded) the env ends up naming B while carrying
// A's ID, and the popup asks about a session the user can see while the answer
// destroys one they cannot.
//
// The env must come out empty, not merely free of A: the error is what stops
// openConfirmPopup from showing the popup, so whatever landed before the
// failure is a request nobody will ever answer — and one a later tick, or a
// hand-run `jin confirm-popup`, could still pick up.
func TestWriteConfirmRequest_PartialWriteLeavesNothingStale(t *testing.T) {
	for _, failKey := range []string{EnvConfirmMode, EnvConfirmTargetID, EnvConfirmTargetDesc} {
		t.Run("fails on "+failKey, func(t *testing.T) {
			r := &recordingEnv{
				env: map[string]string{
					EnvConfirmMode:       ConfirmModeDeleteWorktree,
					EnvConfirmTargetID:   "session-A",
					EnvConfirmTargetDesc: "A",
				},
				failSet: failKey,
			}

			err := writeConfirmRequest(r, confirmRequest{
				mode:       ConfirmModeDelete,
				targetID:   "session-B",
				targetDesc: "B",
			})

			if err == nil {
				t.Fatal("writeConfirmRequest = nil, want the failed SetEnvironment propagated")
			}
			if len(r.env) != 0 {
				t.Errorf("env after a failed write = %v, want empty — neither A's prompt nor B's fragments may survive", r.env)
			}
		})
	}
}

// TestWriteConfirmRequest_FailedClearWritesNothing covers the other way the
// handshake can be interrupted: the wipe itself fails. The clear is what turns
// a partial write into empty keys rather than a previous prompt's, so once it
// has failed there is no safe value left to write — writing the new mode on top
// of an unclearable old target is exactly the splice
// TestWriteConfirmRequest_PartialWriteLeavesNothingStale exists to prevent.
// Every key is still attempted, since each one left set is a usable fragment.
//
// The returned error is also what keeps openConfirmPopup from opening a popup
// over a half-cleared env: a prompt this process could not publish in full must
// not be shown.
func TestWriteConfirmRequest_FailedClearWritesNothing(t *testing.T) {
	r := &recordingEnv{
		env: map[string]string{
			EnvConfirmMode:       ConfirmModeDeleteWorktree,
			EnvConfirmTargetID:   "session-A",
			EnvConfirmTargetDesc: "A",
		},
		failUnset: EnvConfirmTargetID,
	}

	err := writeConfirmRequest(r, confirmRequest{
		mode:       ConfirmModeDelete,
		targetID:   "session-B",
		targetDesc: "B",
	})

	if err == nil {
		t.Fatal("writeConfirmRequest = nil, want the failed UnsetEnvironment propagated")
	}
	for _, op := range r.ops {
		if !op.unset {
			t.Errorf("wrote %s=%q after a failed clear, want no writes at all", op.name, op.value)
		}
	}
	unset := map[string]bool{}
	for _, op := range r.ops {
		if op.unset {
			unset[op.name] = true
		}
	}
	for _, key := range []string{EnvConfirmResult, EnvConfirmMode, EnvConfirmTargetID, EnvConfirmTargetDesc} {
		if !unset[key] {
			t.Errorf("%s was never attempted — a clear that stops at the first failure leaves fragments", key)
		}
	}
}

// TestClearConfirmEnv_UnsetsEverything guards the wipe: a key it forgets is a
// prompt or an approval that outlives whatever it was written for — the outer
// tmux server outlives the TUI process, so a survivor can be paired with a
// later request or replayed by the next run.
func TestClearConfirmEnv_UnsetsEverything(t *testing.T) {
	r := &recordingEnv{}

	if err := clearConfirmEnv(r, confirmEnvKeys); err != nil {
		t.Fatalf("clearConfirmEnv: %v", err)
	}

	unset := map[string]bool{}
	for _, op := range r.ops {
		if !op.unset {
			t.Errorf("clearConfirmEnv set %s=%q, want unsets only", op.name, op.value)
			continue
		}
		unset[op.name] = true
	}
	for _, key := range []string{EnvConfirmResult, EnvConfirmMode, EnvConfirmTargetID, EnvConfirmTargetDesc} {
		if !unset[key] {
			t.Errorf("%s was not unset", key)
		}
	}
}

// TestStaleConfirmKeys is the startup wipe's key selection: whatever the
// snapshot holds gets unset, and nothing else is touched. The narrowing is only
// safe because an absent key and an unset one are the same state — so what this
// pins is that every present key is returned, including a lone leftover.
func TestStaleConfirmKeys(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"clean startup", map[string]string{"JIN_CURRENT_SESSION": "s1"}, nil},
		{"nil env", nil, nil},
		{
			// A prompt dismissed with Ctrl+C in a previous run: no result, but
			// enough left behind for a hand-run popup to ask about it.
			name: "dismissed prompt",
			env: map[string]string{
				EnvConfirmMode:       ConfirmModeDelete,
				EnvConfirmTargetID:   "s1",
				EnvConfirmTargetDesc: "one",
			},
			want: []string{EnvConfirmMode, EnvConfirmTargetID, EnvConfirmTargetDesc},
		},
		{
			// The dangerous one: an approval the previous run never got to
			// consume, which this run's first envTick would carry out.
			name: "unconsumed answer",
			env: map[string]string{
				EnvConfirmResult:     ConfirmResultYes,
				EnvConfirmMode:       ConfirmModeKill,
				EnvConfirmTargetID:   "s1",
				EnvConfirmTargetDesc: "one",
			},
			want: confirmEnvKeys,
		},
		{"result alone", map[string]string{EnvConfirmResult: ConfirmResultYes}, []string{EnvConfirmResult}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := staleConfirmKeys(tt.env)
			if len(got) != len(tt.want) {
				t.Fatalf("staleConfirmKeys = %v, want %v", got, tt.want)
			}
			for _, key := range tt.want {
				if !slices.Contains(got, key) {
					t.Errorf("staleConfirmKeys = %v, missing %q", got, key)
				}
			}
		})
	}
}

// resolveFocusSession is the shared fast/slow path helper. These tests pin
// the three branches the envTick fast path and the sessionsMsg slow path
// both depend on: no-op when nothing is pending, cursor-align + clear on
// hit, and preserve focusSessionID on miss so the caller can retry (fast
// path) or explicitly give up (slow path).

func TestResolveFocusSession_EmptyID_ReturnsTrue(t *testing.T) {
	m := &Model{
		sessions: []session.Info{{ID: "a"}, {ID: "b"}},
		cursor:   1,
	}
	if !m.resolveFocusSession() {
		t.Errorf("resolveFocusSession() = false, want true (nothing pending)")
	}
	// Cursor must not move on the no-op path — the helper is called from
	// envTick every 250ms and any drift would silently scroll the list.
	if m.cursor != 1 {
		t.Errorf("cursor = %d, want 1 (unchanged)", m.cursor)
	}
}

func TestResolveFocusSession_TargetInSessions_Switches(t *testing.T) {
	m := &Model{
		sessions:         []session.Info{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		focusSessionID:   "b",
		cursor:           0,
		currentSessionID: "a",
	}
	if !m.resolveFocusSession() {
		t.Fatalf("resolveFocusSession() = false, want true (target present)")
	}
	if m.cursor != 1 {
		t.Errorf("cursor = %d, want 1 (aligned to target index)", m.cursor)
	}
	if m.focusSessionID != "" {
		t.Errorf("focusSessionID = %q, want \"\" (cleared on hit)", m.focusSessionID)
	}
	// tmuxClient is nil in this test, so switchToSession is a no-op and
	// currentSessionID stays as the forced-reset value. Pin the reset so a
	// future refactor cannot silently drop it.
	if m.currentSessionID != "" {
		t.Errorf("currentSessionID = %q, want \"\" (forced reset before switch)", m.currentSessionID)
	}
}

func TestResolveFocusSession_TargetMissing_ReturnsFalse(t *testing.T) {
	m := &Model{
		sessions: []session.Info{
			{ID: "a"},
			{ID: "b"},
		},
		focusSessionID:   "ghost",
		cursor:           1,
		currentSessionID: "b",
	}
	if m.resolveFocusSession() {
		t.Errorf("resolveFocusSession() = true, want false (target absent)")
	}
	if m.focusSessionID != "ghost" {
		t.Errorf("focusSessionID = %q, want \"ghost\" (retained for retry)", m.focusSessionID)
	}
	if m.cursor != 1 {
		t.Errorf("cursor = %d, want 1 (unchanged on miss)", m.cursor)
	}
	if m.currentSessionID != "b" {
		t.Errorf("currentSessionID = %q, want \"b\" (unchanged on miss)", m.currentSessionID)
	}
}

func TestSessionTickInterval(t *testing.T) {
	if sessionTickInterval != 2*time.Second {
		t.Errorf("sessionTickInterval = %v, want 2s", sessionTickInterval)
	}
}

func TestEnvTickCmd_NonNil(t *testing.T) {
	if envTickCmd() == nil {
		t.Fatal("envTickCmd returned nil")
	}
}

func TestSessionTickCmd_NonNil(t *testing.T) {
	if sessionTickCmd() == nil {
		t.Fatal("sessionTickCmd returned nil")
	}
}

// --- openPopup / popupDisplayOptions ---

// TestOpenPopup_LooksUpSizeByName verifies that each core popup name
// resolves its width/height via configMgr.GetPopupSize (matching
// config.DefaultPopupSizes for a Manager with no user overrides) and that
// the subcmd is built as "<name>-popup" with underscores hyphenated to
// match the registered cobra subcommand (e.g. "session-filter-popup").
func TestOpenPopup_LooksUpSizeByName(t *testing.T) {
	configMgr, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	m := Model{configMgr: configMgr, deletingIDs: map[string]bool{}}

	defaults := config.DefaultPopupSizes()
	tests := []struct {
		name       string
		wantSubcmd string
	}{
		{"create", "create-popup"},
		{"session_filter", "session-filter-popup"},
		{"help", "help-popup"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := m.popupDisplayOptions(tt.name, " Title ")

			def := defaults[tt.name]
			wantWidth := fmt.Sprintf("%d%%", def.Width)
			wantHeight := fmt.Sprintf("%d%%", def.Height)
			if opts.Width != wantWidth {
				t.Errorf("Width = %q, want %q", opts.Width, wantWidth)
			}
			if opts.Height != wantHeight {
				t.Errorf("Height = %q, want %q", opts.Height, wantHeight)
			}
			if !strings.HasSuffix(opts.Cmd, " "+tt.wantSubcmd) {
				t.Errorf("Cmd = %q, want suffix %q", opts.Cmd, tt.wantSubcmd)
			}
			if opts.Title != " Title " {
				t.Errorf("Title = %q, want %q", opts.Title, " Title ")
			}
		})
	}
}

// TestOpenPopup_NoConfigMgr_NoOp verifies the configMgr=nil safeguard: an
// unwired Model (popup opener and configMgr both nil — the legacy/test path)
// must not panic when opening a popup, since popupDisplayOptions dereferences
// configMgr.GetPopupSize.
func TestOpenPopup_NoConfigMgr_NoOp(t *testing.T) {
	m := Model{deletingIDs: map[string]bool{}}
	m.openPopup("create", " New Session ")
}

// fakePopupOpener records what openPopup actually asked tmux for. errs[i] is
// returned from the i-th call, so a test can make tmux refuse the first spawn
// the way it refuses a cell-sized popup on a client too small to hold it.
type fakePopupOpener struct {
	calls []tmux.DisplayPopupOptions
	errs  []error
}

func (f *fakePopupOpener) DisplayPopup(o tmux.DisplayPopupOptions) error {
	f.calls = append(f.calls, o)
	if len(f.calls) <= len(f.errs) {
		return f.errs[len(f.calls)-1]
	}
	return nil
}

// popupTestModel is a Model wired for the popup path only: a recorder in place
// of the outer tmux client, real config defaults, and a distinguishable
// identity.
func popupTestModel(t *testing.T, errs ...error) (Model, *fakePopupOpener) {
	t.Helper()
	configMgr, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	fp := &fakePopupOpener{errs: errs}
	return Model{
		configMgr: configMgr,
		popups:    fp,
		// Non-empty on purpose: a popup must name no session, and with this
		// zero the assertion that it does could not fail. The TUI has a
		// displayed session for most of its life.
		currentSessionID: "22222222-2222-2222-2222-222222222222",
		deletingIDs:      map[string]bool{},
		identity: jinenv.Identity{
			SocketPath: "/tmp/from-jin-ui.sock",
			BinPath:    "/tmp/jin-bin",
			Debug:      true,
		},
	}, fp
}

// TestOpenPopup_SpawnsWithTheGivenIdentity is the assertion that the popup
// reaches the daemon `jin ui` picked rather than the one the outer tmux server
// names. It asserts on the spawn, not on popupDisplayOptions: the options
// builder had a test while openPopup could be emptied out entirely without one
// failing.
func TestOpenPopup_SpawnsWithTheGivenIdentity(t *testing.T) {
	m, fp := popupTestModel(t)
	m.openPopup(config.PopupCreate, " New Session ")

	if len(fp.calls) != 1 {
		t.Fatalf("DisplayPopup called %d times, want 1", len(fp.calls))
	}
	want := []string{
		"JIN_SOCKET=/tmp/from-jin-ui.sock",
		"JIN_BIN=/tmp/jin-bin",
		"JIN_DEBUG=1",
		// Empty rather than absent: a popup belongs to no session, and saying
		// so is what clears a stale id the outer tmux server may hold.
		"JIN_SESSION_ID=",
	}
	for _, kv := range want {
		if !slices.Contains(fp.calls[0].Env, kv) {
			t.Errorf("popup Env = %q, want it to contain %q", fp.calls[0].Env, kv)
		}
	}
}

// TestOpenPopup_EveryCorePopupCarriesTheIdentity keeps the guarantee at the
// catalog level: a popup added later must not be the one that quietly opens
// without an identity. PopupPluginDefault is a resolver tier rather than a
// popup, and is excluded the same way PopupSubcmd excludes it.
func TestOpenPopup_EveryCorePopupCarriesTheIdentity(t *testing.T) {
	for _, name := range config.PopupNames() {
		if config.PopupSubcmd(name) == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			m, fp := popupTestModel(t)
			m.openPopup(name, " Title ")
			if len(fp.calls) != 1 {
				t.Fatalf("DisplayPopup called %d times, want 1", len(fp.calls))
			}
			if !slices.Contains(fp.calls[0].Env, "JIN_SOCKET=/tmp/from-jin-ui.sock") {
				t.Errorf("popup %q Env = %q, want the identity's socket", name, fp.calls[0].Env)
			}
		})
	}
}

// TestOpenPopup_RetriesAtPercentWhenTheCellSizedSpawnIsRefused covers the
// branch that keeps a destructive keypress from becoming a no-op on a client
// too small for the confirm dialog. tmux refuses an absolute size larger than
// the client outright, and the refusal is invisible from the TUI, so the retry
// is the only thing that puts the dialog on screen.
func TestOpenPopup_RetriesAtPercentWhenTheCellSizedSpawnIsRefused(t *testing.T) {
	m, fp := popupTestModel(t, errors.New("width too large"))
	m.openPopup(config.PopupConfirm, " Confirm ")

	if len(fp.calls) != 2 {
		t.Fatalf("DisplayPopup called %d times, want 2 (spawn + percent retry)", len(fp.calls))
	}
	cells := config.DefaultPopupSizes()[config.PopupConfirm]
	if got, want := fp.calls[0].Width, fmt.Sprintf("%d", cells.Width); got != want {
		t.Errorf("first spawn Width = %q, want the cell size %q", got, want)
	}
	wantW, wantH, ok := m.configMgr.PopupFallbackPercent(config.PopupConfirm)
	if !ok {
		t.Fatal("PopupFallbackPercent(confirm) ok = false, want true")
	}
	if fp.calls[1].Width != wantW || fp.calls[1].Height != wantH {
		t.Errorf("retry size = (%q, %q), want (%q, %q)",
			fp.calls[1].Width, fp.calls[1].Height, wantW, wantH)
	}
	// The retry is the same popup, so it must still name its daemon.
	if !slices.Contains(fp.calls[1].Env, "JIN_SOCKET=/tmp/from-jin-ui.sock") {
		t.Errorf("retry Env = %q, want the identity's socket", fp.calls[1].Env)
	}
}

// TestOpenPopup_DoesNotRetryAPercentSizedPopup is the other half: a popup that
// was already sized as a share of the client cannot be refused for being too
// big, so a second identical spawn could only fail the same way.
func TestOpenPopup_DoesNotRetryAPercentSizedPopup(t *testing.T) {
	m, fp := popupTestModel(t, errors.New("popup failed"))
	m.openPopup(config.PopupCreate, " New Session ")

	if len(fp.calls) != 1 {
		t.Fatalf("DisplayPopup called %d times, want 1 (no retry)", len(fp.calls))
	}
}

// TestOpenPopup_NoOpenerIsNoOp pins the guard that keeps a Model built by
// NewModel (no tmux) from calling into a nil opener.
func TestOpenPopup_NoOpenerIsNoOp(t *testing.T) {
	configMgr, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	m := Model{configMgr: configMgr, deletingIDs: map[string]bool{}}
	m.openPopup(config.PopupCreate, " New Session ")
}

// --- adoptAttachedSession / pollAttachedSessionCmd ---
//
// These run with tmuxClient=nil (the degraded-guard style used across this
// file): the tmux side-effects (SetEnvironment, @session_name) are skipped, so
// the tests observe only the in-memory state adoption. Real tmux wiring and the
// end-to-end choose-tree follow are covered only by manual verification, not
// by this suite.

// TestAdoptAttachedSession covers the in-memory adoption paths: the happy
// path, cursor tracking of the display index (the black-box-reachable half
// of the cursor-tracking case), the steady-state no-op that keeps the poll
// from thrashing the cursor every tick, unknown names, empty attach names
// (client dead / poll race), and a late msg after leaving local attach. The
// cursor-tracking case's other half (an adopted session hidden by a filter:
// ID adopted, cursor kept) is not black-box reachable while
// getDisplaySessions() returns m.sessions unfiltered; moveCursorToSession's
// guard covers it structurally.
func TestAdoptAttachedSession(t *testing.T) {
	sessions := []session.Info{
		{ID: "s1", TmuxWindowName: "jin-s1", Description: "one"},
		{ID: "s2", TmuxWindowName: "jin-s2", Description: "two"},
		{ID: "s3", TmuxWindowName: "jin-s3", Description: "three"},
	}
	cases := []struct {
		name        string
		attached    string
		localAttach bool
		startID     string
		startCursor int
		wantID      string
		wantCursor  int
	}{
		{"follows another session", "jin-s2", true, "s1", 0, "s2", 1},
		{"cursor tracks display index", "jin-s3", true, "s1", 0, "s3", 2},
		{"already in sync", "jin-s2", true, "s2", 1, "s2", 1},
		{"unknown name", "jin-unknown", true, "s1", 0, "s1", 0},
		{"empty attach name", "", true, "s1", 0, "s1", 0},
		{"not local attach", "jin-s2", false, "s1", 0, "s1", 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				sessions:           sessions,
				currentSessionID:   tt.startID,
				cursor:             tt.startCursor,
				displayLocalAttach: tt.localAttach,
			}
			m.adoptAttachedSession(tt.attached)
			if m.currentSessionID != tt.wantID || m.cursor != tt.wantCursor {
				t.Errorf("got id=%q cursor=%d, want id=%q cursor=%d",
					m.currentSessionID, m.cursor, tt.wantID, tt.wantCursor)
			}
		})
	}
}

// TestPollAttachedSessionCmd_Guards verifies that the poll Cmd is only issued
// when the display pane is locally attached with both tmux clients wired and a
// known pane ID. Building the Cmd runs no tmux command (that happens only when
// the closure fires), so the all-satisfied case can assert non-nil with
// zero-value clients; only the actual tmux round-trip is manual.
func TestPollAttachedSessionCmd_Guards(t *testing.T) {
	cases := []struct {
		name        string
		localAttach bool
		paneID      string
		outer       *tmux.Client
		inner       *tmux.Client
		wantNil     bool
	}{
		{"not local attach", false, "%1", &tmux.Client{}, &tmux.Client{}, true},
		{"empty pane id", true, "", &tmux.Client{}, &tmux.Client{}, true},
		{"nil outer client", true, "%1", nil, &tmux.Client{}, true},
		{"nil inner client", true, "%1", &tmux.Client{}, nil, true},
		{"neither", false, "", nil, nil, true},
		{"all satisfied", true, "%1", &tmux.Client{}, &tmux.Client{}, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				displayLocalAttach: tt.localAttach,
				displayPaneID:      tt.paneID,
				tmuxClient:         tt.outer,
				innerTmuxClient:    tt.inner,
				deletingIDs:        map[string]bool{},
			}
			c := m.pollAttachedSessionCmd()
			if tt.wantNil && c != nil {
				t.Errorf("expected nil Cmd, got %T", c)
			}
			if !tt.wantNil && c == nil {
				t.Error("expected non-nil Cmd, got nil")
			}
		})
	}
}

// TestBuildInnerAttachCmd_UsesResolvedSocket pins the display pane's attach
// command to the *resolved* inner socket. This regressed once: the command was
// built from the tmux.SocketName constant, so a run that redirected every
// Client with JIN_TMUX_SOCKET still sent the pane to the real "jin" server the
// moment a session was displayed.
func TestBuildInnerAttachCmd_UsesResolvedSocket(t *testing.T) {
	t.Run("env value reaches the attach command", func(t *testing.T) {
		t.Setenv("JIN_TMUX_SOCKET", "jin-test-abcd1234")
		got := buildInnerAttachCmd(tmux.DefaultSocketName(), "sess-x")
		if !strings.Contains(got, "-L jin-test-abcd1234 ") {
			t.Errorf("attach command %q does not target the overridden socket", got)
		}
		if strings.Contains(got, "-L "+tmux.SocketName+" ") {
			t.Errorf("attach command %q still targets the built-in default socket", got)
		}
	})

	t.Run("unset falls back to the built-in default", func(t *testing.T) {
		t.Setenv("JIN_TMUX_SOCKET", "")
		got := buildInnerAttachCmd(tmux.DefaultSocketName(), "sess-x")
		if !strings.Contains(got, "-L "+tmux.SocketName+" ") {
			t.Errorf("attach command %q does not target the default socket %q", got, tmux.SocketName)
		}
	})

	t.Run("keeps the nesting guard and the tail keepalive", func(t *testing.T) {
		got := buildInnerAttachCmd("sock", "sess-x")
		want := "env -u TMUX tmux -L sock attach -t sess-x; tail -f /dev/null"
		if got != want {
			t.Errorf("buildInnerAttachCmd() = %q, want %q", got, want)
		}
	})
}
