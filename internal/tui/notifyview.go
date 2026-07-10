package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/takaaki-s/jind-ai/internal/notify"
)

// NotifyModel is a standalone Bubble Tea model for displaying notification history.
// Designed to run inside a tmux popup. Selecting an entry sets the session ID
// for the parent TUI to pick up via tmux environment variable.
type NotifyModel struct {
	entries   []notify.Entry
	cursor    int
	selected  string // selected session ID (empty = no selection)
	width     int
	height    int
	scrollTop int
}

// NewNotifyModel creates a new NotifyModel with the given entries.
func NewNotifyModel(entries []notify.Entry) NotifyModel {
	return NotifyModel{entries: entries}
}

// Selected returns the session ID selected by the user, or empty string.
func (m NotifyModel) Selected() string {
	return m.selected
}

func (m NotifyModel) Init() tea.Cmd {
	return nil
}

func (m NotifyModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				if m.cursor < m.scrollTop {
					m.scrollTop = m.cursor
				}
			}
		case "down", "j":
			if m.cursor < len(m.entries)-1 {
				m.cursor++
				visibleLines := m.visibleLines()
				if m.cursor >= m.scrollTop+visibleLines {
					m.scrollTop = m.cursor - visibleLines + 1
				}
			}
		case "enter":
			if len(m.entries) > 0 && m.cursor < len(m.entries) {
				m.selected = m.entries[m.cursor].SessionID
			}
			return m, tea.Quit
		case "esc", "q":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m NotifyModel) visibleLines() int {
	// Account for header (2 lines) and footer (2 lines)
	lines := m.height - 4
	lines = max(lines, 1)
	return lines
}

func (m NotifyModel) View() string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(primaryColor)
	cursorStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255")).Background(primaryColor)
	timeColStyle := lipgloss.NewStyle().Foreground(dimColor)
	typePermStyle := lipgloss.NewStyle().Foreground(warningColor).Bold(true)
	typeCompleteStyle := lipgloss.NewStyle().Foreground(successColor).Bold(true)
	nameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("255"))

	var b strings.Builder

	b.WriteString(titleStyle.Render("Notification History"))
	b.WriteString("\n\n")

	if len(m.entries) == 0 {
		b.WriteString(helpStyle.Render("  No notifications yet"))
		b.WriteString("\n\n")
		b.WriteString(helpStyle.Render("Press any key to close"))
		return b.String()
	}

	visibleLines := m.visibleLines()
	end := min(m.scrollTop+visibleLines, len(m.entries))

	for i := m.scrollTop; i < end; i++ {
		entry := m.entries[i]

		// Time column (fixed width)
		timeStr := fmt.Sprintf("%-10s", timeAgo(entry.Timestamp))

		// Type icon and label
		var typeStr string
		switch entry.Type {
		case "permission":
			typeStr = typePermStyle.Render("?  Permission   ")
		case "task_complete":
			typeStr = typeCompleteStyle.Render("⚡ Task Complete ")
		default:
			typeStr = fmt.Sprintf("   %-14s", entry.Type)
		}

		// Session name
		sessionName := entry.SessionName

		line := fmt.Sprintf("  %s %s %s",
			timeColStyle.Render(timeStr),
			typeStr,
			nameStyle.Render(sessionName),
		)

		if i == m.cursor {
			// Highlight the entire line for selected item
			line = cursorStyle.Render(fmt.Sprintf("▸ %s %s %s",
				timeStr,
				entryTypeLabel(entry.Type),
				sessionName,
			))
		}

		b.WriteString(line)
		b.WriteString("\n")
	}

	// Scroll indicator
	if len(m.entries) > visibleLines {
		b.WriteString(fmt.Sprintf("\n%s",
			helpStyle.Render(fmt.Sprintf("  %d/%d", m.cursor+1, len(m.entries)))))
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑↓ navigate  Enter attach  Esc close"))

	return b.String()
}

func entryTypeLabel(t string) string {
	switch t {
	case "permission":
		return "?  Permission   "
	case "task_complete":
		return "⚡ Task Complete "
	default:
		return fmt.Sprintf("   %-14s", t)
	}
}
