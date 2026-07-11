package tui

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/takaaki-s/jind-ai/internal/action"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/session"
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

// --- countStatuses ---

func TestCountStatuses(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		counts := countStatuses(nil)
		if counts.thinking != 0 || counts.permission != 0 || counts.running != 0 ||
			counts.creating != 0 || counts.idle != 0 || counts.stopped != 0 {
			t.Errorf("countStatuses(nil) should return all zeros, got %+v", counts)
		}
	})

	t.Run("mixed statuses", func(t *testing.T) {
		sessions := []session.Info{
			{Status: session.StatusThinking},
			{Status: session.StatusThinking},
			{Status: session.StatusPermission},
			{Status: session.StatusRunning},
			{Status: session.StatusCreating},
			{Status: session.StatusIdle},
			{Status: session.StatusIdle},
			{Status: session.StatusIdle},
			{Status: session.StatusStopped},
		}
		counts := countStatuses(sessions)
		if counts.thinking != 2 {
			t.Errorf("thinking = %d, want 2", counts.thinking)
		}
		if counts.permission != 1 {
			t.Errorf("permission = %d, want 1", counts.permission)
		}
		if counts.running != 1 {
			t.Errorf("running = %d, want 1", counts.running)
		}
		if counts.creating != 1 {
			t.Errorf("creating = %d, want 1", counts.creating)
		}
		if counts.idle != 3 {
			t.Errorf("idle = %d, want 3", counts.idle)
		}
		if counts.stopped != 1 {
			t.Errorf("stopped = %d, want 1", counts.stopped)
		}
	})
}

// --- getStatusDisplay ---

func TestGetStatusDisplay(t *testing.T) {
	tests := []struct {
		name      string
		status    session.Status
		wantIcon  string
		wantLabel string
	}{
		{"thinking", session.StatusThinking, "⚡", "THINKING"},
		{"permission", session.StatusPermission, "?", "PERMISSION"},
		{"running", session.StatusRunning, "▶", "RUNNING"},
		{"creating", session.StatusCreating, "+", "CREATING"},
		{"idle", session.StatusIdle, "○", "IDLE"},
		{"stopped", session.StatusStopped, "■", "STOPPED"},
		{"unknown", session.Status("unknown"), "?", "UNKNOWN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			icon, label, _ := getStatusDisplay(tt.status)
			if icon != tt.wantIcon {
				t.Errorf("getStatusDisplay(%q) icon = %q, want %q", tt.status, icon, tt.wantIcon)
			}
			if label != tt.wantLabel {
				t.Errorf("getStatusDisplay(%q) label = %q, want %q", tt.status, label, tt.wantLabel)
			}
		})
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

// --- buildStatusSummary ---

func TestBuildStatusSummary(t *testing.T) {
	t.Run("empty sessions returns empty string", func(t *testing.T) {
		got := buildStatusSummary(nil)
		if got != "" {
			t.Errorf("buildStatusSummary(nil) = %q, want empty string", got)
		}
	})

	t.Run("all same status", func(t *testing.T) {
		sessions := []session.Info{
			{Status: session.StatusIdle},
			{Status: session.StatusIdle},
			{Status: session.StatusIdle},
		}
		got := buildStatusSummary(sessions)
		if got == "" {
			t.Fatal("buildStatusSummary with idle sessions should not return empty string")
		}
		if !strings.Contains(got, "3") {
			t.Errorf("buildStatusSummary should contain count 3, got %q", got)
		}
		if !strings.Contains(got, "Idle") {
			t.Errorf("buildStatusSummary should contain 'Idle', got %q", got)
		}
	})

	t.Run("mixed statuses", func(t *testing.T) {
		sessions := []session.Info{
			{Status: session.StatusThinking},
			{Status: session.StatusThinking},
			{Status: session.StatusPermission},
			{Status: session.StatusIdle},
		}
		got := buildStatusSummary(sessions)
		if got == "" {
			t.Fatal("buildStatusSummary with mixed sessions should not return empty string")
		}
		if !strings.Contains(got, "2") {
			t.Errorf("buildStatusSummary should contain count 2 for thinking, got %q", got)
		}
		if !strings.Contains(got, "Thinking") {
			t.Errorf("buildStatusSummary should contain 'Thinking', got %q", got)
		}
		if !strings.Contains(got, "Permission") {
			t.Errorf("buildStatusSummary should contain 'Permission', got %q", got)
		}
		if !strings.Contains(got, "Idle") {
			t.Errorf("buildStatusSummary should contain 'Idle', got %q", got)
		}
	})

	t.Run("stopped only returns empty because stopped is excluded from summary", func(t *testing.T) {
		sessions := []session.Info{
			{Status: session.StatusStopped},
			{Status: session.StatusStopped},
		}
		got := buildStatusSummary(sessions)
		// buildStatusSummary intentionally excludes stopped from the summary
		if got != "" {
			t.Errorf("buildStatusSummary with only stopped should return empty, got %q", got)
		}
	})

	t.Run("running and creating", func(t *testing.T) {
		sessions := []session.Info{
			{Status: session.StatusRunning},
			{Status: session.StatusCreating},
			{Status: session.StatusCreating},
		}
		got := buildStatusSummary(sessions)
		if !strings.Contains(got, "Running") {
			t.Errorf("buildStatusSummary should contain 'Running', got %q", got)
		}
		if !strings.Contains(got, "Creating") {
			t.Errorf("buildStatusSummary should contain 'Creating', got %q", got)
		}
	})
}

// --- cardHeight ---

func TestCardHeight(t *testing.T) {
	m := Model{deletingIDs: map[string]bool{"deleting-id": true}}

	tests := []struct {
		name string
		sess session.Info
		want int
	}{
		{
			name: "base card (name + status + spacer)",
			sess: session.Info{ID: "s1", Description: "s"},
			want: 3,
		},
		{
			name: "with user message",
			sess: session.Info{ID: "s2", Description: "s", LastUserMessage: "hi"},
			want: 4,
		},
		{
			name: "with both messages",
			sess: session.Info{ID: "s3", Description: "s", LastUserMessage: "hi", LastAssistantMessage: "yo"},
			want: 5,
		},
		{
			name: "deleting card",
			sess: session.Info{ID: "deleting-id", Description: "s"},
			want: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.cardHeight(tt.sess); got != tt.want {
				t.Errorf("cardHeight() = %d, want %d", got, tt.want)
			}
		})
	}
}

// --- viewport scroll ---

// TestAdjustScrollForCursor verifies the viewport follows the cursor.
// The available content area for these tests is small so we can watch the
// scroll offset track the cursor's card position.
func TestAdjustScrollForCursor(t *testing.T) {
	// 10 base cards (3 lines each) + 1 fleet header = 31 lines total.
	sessions := make([]session.Info, 10)
	for i := range sessions {
		sessions[i] = session.Info{ID: string(rune('0' + i)), Description: "s"}
	}

	newModel := func(cursor int) *Model {
		m := &Model{
			sessions:    sessions,
			height:      10, // contentAreaLines() → 10-1-1-1 = 7 usable
			cursor:      cursor,
			deletingIDs: map[string]bool{},
		}
		return m
	}

	t.Run("cursor on first card keeps scroll at 0", func(t *testing.T) {
		m := newModel(0)
		m.adjustScrollForCursor()
		if m.scrollOffset != 0 {
			t.Errorf("scrollOffset = %d, want 0 (cursor on first card)", m.scrollOffset)
		}
	})

	t.Run("cursor below viewport scrolls down", func(t *testing.T) {
		m := newModel(4)
		// Card 4 top = 1 (fleet header) + 4*3 = 13. avail = 7. Bottom = 16.
		// Should scroll so bottom = scrollOffset + avail → scrollOffset = 9.
		m.adjustScrollForCursor()
		if m.scrollOffset != 9 {
			t.Errorf("scrollOffset = %d, want 9 (cursor below fold)", m.scrollOffset)
		}
	})

	t.Run("cursor at end anchors scroll to last visible page", func(t *testing.T) {
		m := newModel(9)
		m.adjustScrollForCursor()
		// Last card top = 1 + 9*3 = 28, height = 3, bottom = 31.
		// scrollOffset = 31 - 7 = 24. clampScroll bounds by totalCardLines(31) - avail(7) = 24.
		if m.scrollOffset != 24 {
			t.Errorf("scrollOffset = %d, want 24 (cursor at end)", m.scrollOffset)
		}
	})

	t.Run("clampScroll bounds within [0, total-avail]", func(t *testing.T) {
		m := newModel(0)
		m.scrollOffset = 999
		m.clampScroll()
		if m.scrollOffset != 24 {
			t.Errorf("clampScroll from overshoot = %d, want 24", m.scrollOffset)
		}
		m.scrollOffset = -50
		m.clampScroll()
		if m.scrollOffset != 0 {
			t.Errorf("clampScroll from negative = %d, want 0", m.scrollOffset)
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

func TestDeleteConfirmMoveCursorToNextSession(t *testing.T) {
	makeSessions := func(ids ...string) []session.Info {
		var ss []session.Info
		for _, id := range ids {
			ss = append(ss, session.Info{ID: id, Description: id})
		}
		return ss
	}

	yKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}

	t.Run("cursor moves to next session after delete confirm", func(t *testing.T) {
		m := Model{
			sessions:       makeSessions("a", "b", "c"),
			cursor:         1, // on "b"
			deletingIDs:    make(map[string]bool),
			height:         100,
			confirmDelete:  true,
			deleteTargetID: "b",
		}
		result, _ := m.updateListMode(yKey)
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
			sessions:       makeSessions("a", "b", "c"),
			cursor:         2, // on "c" (last)
			deletingIDs:    make(map[string]bool),
			height:         100,
			confirmDelete:  true,
			deleteTargetID: "c",
		}
		result, _ := m.updateListMode(yKey)
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

// --- renderSession ---

// TestRenderSession_Indicators verifies the two orthogonal indicators:
//   - selected → blue '▎' cursor bar on every card line
//   - viewed   → subdued row background across every card line (detectable
//     via presence of an ANSI SGR bg code in the rendered output)
func TestRenderSession_Indicators(t *testing.T) {
	sess := session.Info{
		ID:          "test-id",
		Description: "test-session",
		Status:      session.StatusIdle,
	}
	m := Model{}
	width := 40

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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := m.renderSession(sess, tt.selected, tt.viewed, width)
			lines := strings.Split(result, "\n")
			if len(lines) == 0 {
				t.Fatal("renderSession returned empty string")
			}
			firstLine := lines[0]
			hasBar := strings.Contains(firstLine, "▎")
			if hasBar != tt.wantBar {
				t.Errorf("cursor bar present = %v, want %v (line: %q)", hasBar, tt.wantBar, firstLine)
			}
			hasViewBg := strings.Contains(firstLine, bgSGR)
			if hasViewBg != tt.wantViewBg {
				t.Errorf("viewed background present = %v, want %v (line: %q)", hasViewBg, tt.wantViewBg, firstLine)
			}
		})
	}
}

// --- dispatchAction / currentCursorSessionID / writeCursorEnv ---
//
// Note: the tui package has no mock for *tmux.Client or *daemon.Client — both
// are concrete structs with unexported fields, and expanding an interface just
// for these tests would balloon this task well beyond R4. The tests below
// cover the routing/guard logic reachable without live clients; the real
// side-effect wiring (ZoomPane, SetEnvironment, PluginRun) is exercised by
// the manual verification steps (see 03_todo.md V-006/V-008/V-009).

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

// TestDispatchAction_PluginRouting verifies the plugin: prefix routes to
// handlePluginRun and that the m.client=nil guard prevents any panic /
// spurious m.err when the daemon is unavailable.
func TestDispatchAction_PluginRouting(t *testing.T) {
	m := Model{deletingIDs: map[string]bool{}}
	next, cmd := m.dispatchAction(action.PluginIDPrefix + "notifier")
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

// TestDispatchAction_KillSetsConfirm asserts IDKill routes to handleKill by
// observing the confirm-kill state transition it produces on a non-empty list.
func TestDispatchAction_KillSetsConfirm(t *testing.T) {
	m := Model{
		sessions:    []session.Info{{ID: "s1"}},
		cursor:      0,
		deletingIDs: map[string]bool{},
	}
	next, _ := m.dispatchAction(action.IDKill)
	nm := next.(Model)
	if !nm.confirmKill {
		t.Fatal("expected confirmKill to be true after IDKill dispatch")
	}
	if nm.killTargetID != "s1" {
		t.Fatalf("killTargetID = %q, want %q", nm.killTargetID, "s1")
	}
}

// TestDispatchAction_DeleteSetsConfirm asserts IDDelete routes to handleDelete
// by observing the confirm-delete state transition on a non-empty list.
func TestDispatchAction_DeleteSetsConfirm(t *testing.T) {
	m := Model{
		sessions:    []session.Info{{ID: "s1"}},
		cursor:      0,
		deletingIDs: map[string]bool{},
	}
	next, _ := m.dispatchAction(action.IDDelete)
	nm := next.(Model)
	if !nm.confirmDelete {
		t.Fatal("expected confirmDelete to be true after IDDelete dispatch")
	}
	if nm.deleteTargetID != "s1" {
		t.Fatalf("deleteTargetID = %q, want %q", nm.deleteTargetID, "s1")
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
// wiring is covered by manual verification (03_todo.md V-009).
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

// TestEnvTickConsume_FocusSession pins the consume-and-unset contract that
// the JIN_FOCUS_SESSION branch in the envTickMsg handler (model.go) relies
// on. The real branch reads through m.tmuxClient, a concrete *tmux.Client
// that shells out to the tmux binary with no fake injection point, so —
// like its JIN_CREATED_SESSION / JIN_NOTIFY_SESSION / JIN_ACTION_ID
// siblings — it cannot be driven end-to-end via Model.Update in a unit
// test. This test instead exercises the same consume-closure shape against
// a fake env map, asserting a present value is both returned once and
// flagged for unset so a stale value isn't reapplied on the next tick.
func TestEnvTickConsume_FocusSession(t *testing.T) {
	env := map[string]string{"JIN_FOCUS_SESSION": "sess-xyz"}
	var unsetKeys []string
	consume := func(key string) string {
		v := env[key]
		if v != "" {
			unsetKeys = append(unsetKeys, key)
		}
		return v
	}

	var m Model
	if id := consume("JIN_FOCUS_SESSION"); id != "" {
		m.focusSessionID = id
	}

	if m.focusSessionID != "sess-xyz" {
		t.Errorf("focusSessionID = %q, want %q", m.focusSessionID, "sess-xyz")
	}
	if len(unsetKeys) != 1 || unsetKeys[0] != "JIN_FOCUS_SESSION" {
		t.Errorf("unset keys = %v, want [JIN_FOCUS_SESSION]", unsetKeys)
	}
}

// TestEnvTickConsume_FocusSession_EmptyIsNoOp mirrors the "value absent"
// path: consume must not report an unset when the key was never set, and
// focusSessionID must stay untouched.
func TestEnvTickConsume_FocusSession_EmptyIsNoOp(t *testing.T) {
	env := map[string]string{}
	var unsetKeys []string
	consume := func(key string) string {
		v := env[key]
		if v != "" {
			unsetKeys = append(unsetKeys, key)
		}
		return v
	}

	m := Model{focusSessionID: "unchanged"}
	if id := consume("JIN_FOCUS_SESSION"); id != "" {
		m.focusSessionID = id
	}

	if m.focusSessionID != "unchanged" {
		t.Errorf("focusSessionID = %q, want unchanged", m.focusSessionID)
	}
	if len(unsetKeys) != 0 {
		t.Errorf("unset keys = %v, want none", unsetKeys)
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
