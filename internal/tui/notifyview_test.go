package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/takaaki-s/jind-ai/internal/notify"
)

// --- entryTypeLabel ---

func TestEntryTypeLabel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "permission type",
			input: "permission",
			want:  "?  Permission   ",
		},
		{
			name:  "task_complete type",
			input: "task_complete",
			want:  "⚡ Task Complete ",
		},
		{
			name:  "unknown type uses sprintf formatting",
			input: "custom_event",
			want:  fmt.Sprintf("   %-14s", "custom_event"),
		},
		{
			name:  "empty type",
			input: "",
			want:  fmt.Sprintf("   %-14s", ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := entryTypeLabel(tt.input)
			if got != tt.want {
				t.Errorf("entryTypeLabel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- NewNotifyModel ---

func TestNewNotifyModel(t *testing.T) {
	t.Run("with entries", func(t *testing.T) {
		entries := []notify.Entry{
			{
				SessionID:   "sess-1",
				SessionName: "Project A",
				Type:        "permission",
				Message:     "Needs approval",
				Timestamp:   time.Now(),
			},
			{
				SessionID:   "sess-2",
				SessionName: "Project B",
				Type:        "task_complete",
				Message:     "Done",
				Timestamp:   time.Now(),
			},
		}
		m := NewNotifyModel(entries)

		if len(m.entries) != 2 {
			t.Fatalf("NewNotifyModel() entries = %d, want 2", len(m.entries))
		}
		if m.cursor != 0 {
			t.Errorf("NewNotifyModel() cursor = %d, want 0", m.cursor)
		}
		if m.selected != "" {
			t.Errorf("NewNotifyModel() selected = %q, want empty", m.selected)
		}
		if m.width != 0 {
			t.Errorf("NewNotifyModel() width = %d, want 0", m.width)
		}
		if m.height != 0 {
			t.Errorf("NewNotifyModel() height = %d, want 0", m.height)
		}
		if m.scrollTop != 0 {
			t.Errorf("NewNotifyModel() scrollTop = %d, want 0", m.scrollTop)
		}
	})

	t.Run("with empty entries", func(t *testing.T) {
		m := NewNotifyModel(nil)

		if m.entries != nil {
			t.Errorf("NewNotifyModel(nil) entries should be nil, got %v", m.entries)
		}
		if m.cursor != 0 {
			t.Errorf("NewNotifyModel(nil) cursor = %d, want 0", m.cursor)
		}
	})
}

// --- NotifyModel.Selected ---

func TestNotifyModel_Selected(t *testing.T) {
	t.Run("initially empty", func(t *testing.T) {
		m := NewNotifyModel(nil)
		if got := m.Selected(); got != "" {
			t.Errorf("Selected() = %q, want empty string", got)
		}
	})

	t.Run("returns session ID when set", func(t *testing.T) {
		m := NotifyModel{selected: "sess-123"}
		if got := m.Selected(); got != "sess-123" {
			t.Errorf("Selected() = %q, want %q", got, "sess-123")
		}
	})
}

// --- NotifyModel.visibleLines ---

func TestNotifyModel_VisibleLines(t *testing.T) {
	tests := []struct {
		name   string
		height int
		want   int
	}{
		{
			name:   "normal height 30",
			height: 30,
			want:   26, // 30 - 4
		},
		{
			name:   "small height 10",
			height: 10,
			want:   6, // 10 - 4
		},
		{
			name:   "minimum height 5 gives 1",
			height: 5,
			want:   1, // 5 - 4 = 1
		},
		{
			name:   "height 4 gives 1 (clamped)",
			height: 4,
			want:   1, // 4 - 4 = 0, clamped to 1
		},
		{
			name:   "height 0 gives 1 (clamped)",
			height: 0,
			want:   1, // 0 - 4 = -4, clamped to 1
		},
		{
			name:   "height 3 gives 1 (clamped)",
			height: 3,
			want:   1, // 3 - 4 = -1, clamped to 1
		},
		{
			name:   "height 20",
			height: 20,
			want:   16, // 20 - 4
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NotifyModel{height: tt.height}
			got := m.visibleLines()
			if got != tt.want {
				t.Errorf("visibleLines() with height=%d = %d, want %d", tt.height, got, tt.want)
			}
		})
	}
}
