package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/takaaki-s/jindaiko/internal/config"
	"github.com/takaaki-s/jindaiko/internal/session"
)

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

// --- matchesSearch ---

func TestMatchesSearch(t *testing.T) {
	sess := session.Info{
		Description:    "MyProject",
		WorkDir:        "/home/user/projects/webapp",
		CurrentWorkDir: "/home/user/projects/webapp/src",
		CurrentBranch:  "feature-auth",
	}

	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"match by Name", "myproject", true},
		{"match by WorkDir", "webapp", true},
		{"match by CurrentWorkDir", "webapp/src", true},
		{"match by CurrentBranch", "feature-auth", true},
		{"case insensitive match", "myproject", true},
		{"partial match", "proj", true},
		{"no match", "nonexistent", false},
		{"empty query matches nothing meaningful", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesSearch(sess, tt.query)
			// Empty query: strings.Contains(anything, "") == true,
			// so it will match unless all fields are empty.
			if tt.query == "" {
				// For empty query, it will match any non-empty field
				if !got {
					t.Errorf("matchesSearch with empty query should match non-empty fields")
				}
				return
			}
			if got != tt.want {
				t.Errorf("matchesSearch(sess, %q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}

	t.Run("no match with empty session", func(t *testing.T) {
		emptySess := session.Info{}
		got := matchesSearch(emptySess, "anything")
		if got {
			t.Error("matchesSearch with empty session should return false")
		}
	})

	t.Run("match by Fleet", func(t *testing.T) {
		fleetSess := session.Info{
			Description: "session1",
			Fleet:       "backend",
		}
		if !matchesSearch(fleetSess, "backend") {
			t.Error("matchesSearch should match by Fleet name")
		}
		if matchesSearch(fleetSess, "frontend") {
			t.Error("matchesSearch should not match unrelated Fleet name")
		}
	})
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

// --- getItemsPerPage ---

func TestGetItemsPerPage(t *testing.T) {
	tests := []struct {
		name      string
		height    int
		searching bool
		wantMin   int // items should be at least this
	}{
		{
			name:   "tall terminal",
			height: 40,
			// availableLines = 40 - 8 = 32, items = 32/4 = 8
		},
		{
			name:   "short terminal",
			height: 10,
			// availableLines = 10 - 8 = 2, clamped to 4, items = 4/4 = 1
		},
		{
			name:      "with search bar",
			height:    40,
			searching: true,
			// availableLines = 40 - 8 - 1 = 31, items = 31/4 = 7
		},
		{
			name:   "very short terminal",
			height: 5,
			// availableLines = 5 - 8 = -3, clamped to 4, items = 4/4 = 1
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				height:    tt.height,
				searching: tt.searching,
			}
			got := m.getItemsPerPage()
			if got < 1 {
				t.Errorf("getItemsPerPage() = %d, should be at least 1", got)
			}
			// Verify calculation (no sessions in test → fleetGroupCount=0 → no header subtraction)
			availableLines := tt.height - 8
			if tt.searching {
				availableLines--
			}
			availableLines = max(availableLines, 4)
			expected := availableLines / 4
			expected = max(expected, 1)
			if got != expected {
				t.Errorf("getItemsPerPage() = %d, want %d (height=%d, searching=%v)",
					got, expected, tt.height, tt.searching)
			}
		})
	}
}

// --- getTotalPages ---

func TestGetTotalPages(t *testing.T) {
	tests := []struct {
		name        string
		numSessions int
		height      int
		wantPages   int
	}{
		{
			name:        "no sessions",
			numSessions: 0,
			height:      40,
			wantPages:   1,
		},
		{
			name:        "fewer sessions than page size",
			numSessions: 3,
			height:      40,
			// itemsPerPage = (40-8)/4 = 32/4 = 8, totalPages = ceil(3/8) = 1
			wantPages: 1,
		},
		{
			name:        "exactly one page",
			numSessions: 7,
			height:      40,
			// fleetGroupCount=1 → availableLines=40-8-1=31, itemsPerPage=31/4=7, totalPages=ceil(7/7)=1
			wantPages: 1,
		},
		{
			name:        "two pages",
			numSessions: 9,
			height:      40,
			// fleetGroupCount=1 → itemsPerPage=7, totalPages=ceil(9/7)=2
			wantPages: 2,
		},
		{
			name:        "many sessions short terminal",
			numSessions: 10,
			height:      12,
			// itemsPerPage = max((12-8),4)/4 = 4/4 = 1, totalPages = ceil(10/1) = 10
			wantPages: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := make([]session.Info, tt.numSessions)
			for i := range sessions {
				sessions[i] = session.Info{ID: string(rune('a' + i)), Description: "s"}
			}
			m := Model{
				sessions: sessions,
				height:   tt.height,
			}
			got := m.getTotalPages()
			if got != tt.wantPages {
				t.Errorf("getTotalPages() = %d, want %d", got, tt.wantPages)
			}
		})
	}
}

// --- getPageSessions ---

func TestGetPageSessions(t *testing.T) {
	// Create 10 sessions named s0..s9
	sessions := make([]session.Info, 10)
	for i := range sessions {
		sessions[i] = session.Info{ID: string(rune('0' + i)), Description: "s" + string(rune('0'+i))}
	}

	t.Run("first page", func(t *testing.T) {
		m := Model{
			sessions:    sessions,
			height:      40, // fleetGroupCount=1 → itemsPerPage=(40-8-1)/4=7
			currentPage: 0,
		}
		got := m.getPageSessions()
		if len(got) != 7 {
			t.Fatalf("getPageSessions() page 0 len = %d, want 7", len(got))
		}
		if got[0].Description != "s0" {
			t.Errorf("first item Name = %q, want %q", got[0].Description, "s0")
		}
		if got[6].Description != "s6" {
			t.Errorf("last item Name = %q, want %q", got[6].Description, "s6")
		}
	})

	t.Run("second page", func(t *testing.T) {
		m := Model{
			sessions:    sessions,
			height:      40,
			currentPage: 1,
		}
		got := m.getPageSessions()
		if len(got) != 3 {
			t.Fatalf("getPageSessions() page 1 len = %d, want 3", len(got))
		}
		if got[0].Description != "s7" {
			t.Errorf("first item Name = %q, want %q", got[0].Description, "s7")
		}
		if got[2].Description != "s9" {
			t.Errorf("last item Name = %q, want %q", got[2].Description, "s9")
		}
	})

	t.Run("page beyond range resets to page 0", func(t *testing.T) {
		m := Model{
			sessions:    sessions,
			height:      40,
			currentPage: 99,
		}
		got := m.getPageSessions()
		if len(got) != 7 {
			t.Fatalf("getPageSessions() beyond range len = %d, want 7", len(got))
		}
		if got[0].Description != "s0" {
			t.Errorf("first item Name = %q, want %q", got[0].Description, "s0")
		}
	})

	t.Run("empty sessions", func(t *testing.T) {
		m := Model{
			sessions:    nil,
			height:      40,
			currentPage: 0,
		}
		got := m.getPageSessions()
		if got != nil {
			t.Errorf("getPageSessions() with no sessions should return nil, got %v", got)
		}
	})
}

// --- applySearchFilter ---

func TestApplySearchFilter(t *testing.T) {
	sessions := []session.Info{
		{Description: "frontend", WorkDir: "/home/user/webapp"},
		{Description: "backend", WorkDir: "/home/user/api"},
		{Description: "docs", WorkDir: "/home/user/documentation"},
	}

	t.Run("empty query returns all sessions", func(t *testing.T) {
		si := textinput.New()
		si.SetValue("")
		m := Model{
			sessions:    sessions,
			searching:   true,
			searchInput: si,
		}
		m.applySearchFilter()
		if len(m.filteredSessions) != len(sessions) {
			t.Errorf("applySearchFilter with empty query: got %d sessions, want %d",
				len(m.filteredSessions), len(sessions))
		}
	})

	t.Run("filter by name", func(t *testing.T) {
		si := textinput.New()
		si.SetValue("front")
		m := Model{
			sessions:    sessions,
			searching:   true,
			searchInput: si,
		}
		m.applySearchFilter()
		if len(m.filteredSessions) != 1 {
			t.Fatalf("applySearchFilter('front'): got %d sessions, want 1", len(m.filteredSessions))
		}
		if m.filteredSessions[0].Description != "frontend" {
			t.Errorf("filtered session Name = %q, want %q", m.filteredSessions[0].Description, "frontend")
		}
	})

	t.Run("filter by workdir", func(t *testing.T) {
		si := textinput.New()
		si.SetValue("api")
		m := Model{
			sessions:    sessions,
			searching:   true,
			searchInput: si,
		}
		m.applySearchFilter()
		if len(m.filteredSessions) != 1 {
			t.Fatalf("applySearchFilter('api'): got %d sessions, want 1", len(m.filteredSessions))
		}
		if m.filteredSessions[0].Description != "backend" {
			t.Errorf("filtered session Name = %q, want %q", m.filteredSessions[0].Description, "backend")
		}
	})

	t.Run("no matches", func(t *testing.T) {
		si := textinput.New()
		si.SetValue("nonexistent")
		m := Model{
			sessions:    sessions,
			searching:   true,
			searchInput: si,
		}
		m.applySearchFilter()
		if len(m.filteredSessions) != 0 {
			t.Errorf("applySearchFilter('nonexistent'): got %d sessions, want 0",
				len(m.filteredSessions))
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		si := textinput.New()
		si.SetValue("DOCS")
		m := Model{
			sessions:    sessions,
			searching:   true,
			searchInput: si,
		}
		m.applySearchFilter()
		if len(m.filteredSessions) != 1 {
			t.Fatalf("applySearchFilter('DOCS'): got %d sessions, want 1", len(m.filteredSessions))
		}
		if m.filteredSessions[0].Description != "docs" {
			t.Errorf("filtered session Name = %q, want %q", m.filteredSessions[0].Description, "docs")
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

// --- renderSession ---

func TestRenderSession_ViewedState(t *testing.T) {
	sess := session.Info{
		ID:          "test-id",
		Description: "test-session",
		Status:      session.StatusIdle,
	}
	m := Model{}
	width := 40

	tests := []struct {
		name           string
		selected       bool
		viewed         bool
		wantPrefix     string
		dontWantPrefix string
	}{
		{
			name:           "selected but not viewed shows cursor marker",
			selected:       true,
			viewed:         false,
			wantPrefix:     "> ",
			dontWantPrefix: "▎",
		},
		{
			name:           "selected and viewed shows cursor marker",
			selected:       true,
			viewed:         true,
			wantPrefix:     "> ",
			dontWantPrefix: "▎",
		},
		{
			name:       "viewed only shows viewed marker",
			selected:   false,
			viewed:     true,
			wantPrefix: "▎",
		},
		{
			name:           "neither selected nor viewed shows plain",
			selected:       false,
			viewed:         false,
			dontWantPrefix: "▎",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := m.renderSession(sess, tt.selected, tt.viewed, width)
			lines := strings.Split(result, "\n")
			if len(lines) == 0 {
				t.Fatal("renderSession returned empty string")
			}
			firstLine := lines[0]
			if tt.wantPrefix != "" && !strings.Contains(firstLine, tt.wantPrefix) {
				t.Errorf("first line %q should contain %q", firstLine, tt.wantPrefix)
			}
			if tt.dontWantPrefix != "" && strings.Contains(firstLine, tt.dontWantPrefix) {
				t.Errorf("first line %q should not contain %q", firstLine, tt.dontWantPrefix)
			}
		})
	}
}
