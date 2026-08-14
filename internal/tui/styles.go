package tui

import (
	"github.com/charmbracelet/lipgloss"

	"omasend/internal/theme"
)

// Colours and styles, (re)built from the active Omarchy theme by applyTheme.
// Colours are truecolor hex from the theme; lipgloss downsamples as needed and
// honours NO_COLOR.
var (
	accent lipgloss.Color
	text   lipgloss.Color
	dim    lipgloss.Color
	muted  lipgloss.Color
	good   lipgloss.Color
	bad    lipgloss.Color

	titleBarStyle    lipgloss.Style
	tabActiveStyle   lipgloss.Style
	tabInactiveStyle lipgloss.Style
	frameStyle       lipgloss.Style
	cardStyle        lipgloss.Style
	footerStyle      lipgloss.Style
	titleStyle       lipgloss.Style
	headerStyle      lipgloss.Style
	labelStyle       lipgloss.Style
	valueStyle       lipgloss.Style
)

func init() { applyTheme(theme.Default()) }

// applyTheme rebuilds all styles from t. Called once at startup with the active
// Omarchy theme; init() seeds defaults for tests.
func applyTheme(t theme.Theme) {
	accent = lipgloss.Color(t.Accent)
	text = lipgloss.Color(t.Fg)
	bg := lipgloss.Color(t.Bg)
	dim = lipgloss.Color(t.Dim)
	muted = lipgloss.Color(t.Muted)
	good = lipgloss.Color(t.Good)
	bad = lipgloss.Color(t.Bad)

	// Title bar & active tab: theme background text on the accent (high contrast
	// regardless of whether the accent is light or dark).
	titleBarStyle = lipgloss.NewStyle().Bold(true).Foreground(bg).Background(accent)
	tabActiveStyle = lipgloss.NewStyle().Bold(true).Foreground(bg).Background(accent).Padding(0, 2)
	tabInactiveStyle = lipgloss.NewStyle().Foreground(dim).Padding(0, 2)

	frameStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Padding(0, 1)
	cardStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Padding(1, 2)

	footerStyle = lipgloss.NewStyle().Foreground(muted).Padding(0, 1)
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(accent)
	headerStyle = lipgloss.NewStyle().Foreground(dim)
	labelStyle = lipgloss.NewStyle().Foreground(dim).Width(14)
	valueStyle = lipgloss.NewStyle().Foreground(text)
}
