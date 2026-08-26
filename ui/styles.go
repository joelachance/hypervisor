package ui

import "github.com/charmbracelet/lipgloss"

// The palette is adaptive: hv runs in whatever terminal the person already
// chose, so every color has a light and a dark reading rather than assuming a
// dark background.
var (
	colorActive = lipgloss.AdaptiveColor{Light: "#0F7B4F", Dark: "#3DD68C"}
	colorStale  = lipgloss.AdaptiveColor{Light: "#8A8A8A", Dark: "#6C6C6C"}
	colorApp    = lipgloss.AdaptiveColor{Light: "#7A3EA1", Dark: "#C48FE8"}
	colorCLI    = lipgloss.AdaptiveColor{Light: "#1F6FB2", Dark: "#7FC3F5"}
	colorText   = lipgloss.AdaptiveColor{Light: "#1A1A1A", Dark: "#EDEDED"}
	colorDim    = lipgloss.AdaptiveColor{Light: "#767676", Dark: "#8A8A8A"}
	colorAccent = lipgloss.AdaptiveColor{Light: "#B7410E", Dark: "#FF9E64"}
	colorMerged = lipgloss.AdaptiveColor{Light: "#6B3FA0", Dark: "#BB9AF7"}
	colorOpenPR = lipgloss.AdaptiveColor{Light: "#0F7B4F", Dark: "#3DD68C"}
	colorDanger = lipgloss.AdaptiveColor{Light: "#B3261E", Dark: "#FF7A7A"}
)

// repoPalette gives each repository a colour of its own, assigned by name so
// it is the same colour every run. Reading a list of forty sessions is mostly
// grouping them by repo, and colour does that faster than a word does.
var repoPalette = []lipgloss.AdaptiveColor{
	{Light: "#1F6FB2", Dark: "#7FC3F5"},
	{Light: "#7A3EA1", Dark: "#C48FE8"},
	{Light: "#0F7B4F", Dark: "#3DD68C"},
	{Light: "#B7410E", Dark: "#FF9E64"},
	{Light: "#A8891F", Dark: "#E0C060"},
	{Light: "#B3261E", Dark: "#FF7A7A"},
	{Light: "#00707A", Dark: "#5FD3DC"},
}

// repoStyle picks a repository's colour from its name, so the same repo always
// reads the same and two repos rarely collide.
func repoStyle(name string) lipgloss.Style {
	if name == "" {
		return lipgloss.NewStyle().Foreground(colorDim)
	}
	sum := 0
	for _, char := range name {
		sum = sum*31 + int(char)
	}
	if sum < 0 {
		sum = -sum
	}
	return lipgloss.NewStyle().Foreground(repoPalette[sum%len(repoPalette)])
}

var (
	styleTitleBar = lipgloss.NewStyle().Bold(true).Foreground(colorText).
			Border(lipgloss.NormalBorder(), false, false, true, false).
			BorderForeground(colorDim).Padding(0, 1)

	styleCount = lipgloss.NewStyle().Foreground(colorDim)

	styleCursor    = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleRow       = lipgloss.NewStyle().Foreground(colorText)
	styleRowDim    = lipgloss.NewStyle().Foreground(colorStale)
	styleSelected  = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleHighlight = lipgloss.NewStyle().Background(lipgloss.AdaptiveColor{Light: "#EDEDED", Dark: "#2A2A2A"})

	styleActiveDot = lipgloss.NewStyle().Foreground(colorActive)
	styleStaleDot  = lipgloss.NewStyle().Foreground(colorStale)

	styleBadgeApp = lipgloss.NewStyle().Foreground(colorApp)
	styleBadgeCLI = lipgloss.NewStyle().Foreground(colorCLI)

	styleDim   = lipgloss.NewStyle().Foreground(colorDim)
	styleLabel = lipgloss.NewStyle().Foreground(colorDim)
	styleValue = lipgloss.NewStyle().Foreground(colorText)

	stylePRMerged = lipgloss.NewStyle().Foreground(colorMerged)
	stylePROpen   = lipgloss.NewStyle().Foreground(colorOpenPR)
	stylePRClosed = lipgloss.NewStyle().Foreground(colorDanger)

	styleDetail = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), true, false, false, false).
			BorderForeground(colorDim).Padding(0, 1)

	styleFooter = lipgloss.NewStyle().Foreground(colorDim).Padding(0, 1)
	styleKey    = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleError  = lipgloss.NewStyle().Foreground(colorDanger).Padding(0, 1)
	styleFilter = lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Padding(0, 1)
)
