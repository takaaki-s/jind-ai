package tui

import "github.com/charmbracelet/lipgloss"

var (
	// Colors - Tokyo Night inspired palette
	primaryColor   = lipgloss.Color("#7aa2f7") // Blue
	secondaryColor = lipgloss.Color("#565f89") // Gray
	successColor   = lipgloss.Color("#9ece6a") // Green
	warningColor   = lipgloss.Color("#ff9e64") // Orange
	errorColor     = lipgloss.Color("#f7768e") // Red
	dimColor       = lipgloss.Color("#414868") // Dark gray
	purpleColor    = lipgloss.Color("#bb9af7") // Purple (thinking)
	cyanColor      = lipgloss.Color("#7dcfff") // Cyan (running)
	foregroundColor = lipgloss.Color("#a9b1d6") // Light lavender (text)
	// Title style (for header inside box)
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(primaryColor)

	// Help style (outside box)
	helpStyle = lipgloss.NewStyle().
			Foreground(secondaryColor)

	// Item styles
	selectedItemStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("255")).
				Background(primaryColor)

	// Session name style
	sessionNameStyle = lipgloss.NewStyle().
				Bold(true)

	// Time style
	timeStyle = lipgloss.NewStyle().
			Foreground(dimColor)

	// Path last segment style (brighter than helpStyle for emphasis)
	pathLastSegStyle = lipgloss.NewStyle().
				Foreground(foregroundColor)

	// Status styles - Tokyo Night inspired
	thinkingStyle = lipgloss.NewStyle().
			Foreground(purpleColor).
			Bold(true)

	permissionStyle = lipgloss.NewStyle().
			Foreground(warningColor).
			Bold(true)

	runningStyle = lipgloss.NewStyle().
			Foreground(cyanColor).
			Bold(true)

	creatingStyle = lipgloss.NewStyle().
			Foreground(primaryColor)

	idleStyle = lipgloss.NewStyle().
			Foreground(successColor)

	stoppedStyle = lipgloss.NewStyle().
			Foreground(dimColor)

	// Box border style
	boxBorderColor = primaryColor
)

// createBoxStyle creates a box style with specified width and height.
// When focused is false, the border color is dimmed to indicate the pane is inactive.
func createBoxStyle(width, height int, focused bool) lipgloss.Style {
	borderColor := boxBorderColor
	if !focused {
		borderColor = dimColor
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(width).
		Height(height)
}
