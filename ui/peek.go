package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/satoricorp/hv/recall"
	"github.com/satoricorp/hv/session"
)

// peekState is one session's transcript, read on demand.
//
// A row in the list says a session touched three files in a repo, which is
// enough to tell two sessions apart and not enough to decide whether to merge
// one. Peeking answers the actual question - what was this asked to do, and
// what did it do - without leaving the browser.
type peekState struct {
	session session.Session
	summary recall.Summary
	items   []recall.Item
	offset  int
	loading bool
	err     error
}

type peekedMsg struct {
	sessionID string
	summary   recall.Summary
	items     []recall.Item
	err       error
}

// peekCmd reads the transcript off the render loop: a long session is a large
// file, and the list should not freeze while it is parsed.
func peekCmd(current session.Session) tea.Cmd {
	return func() tea.Msg {
		items, err := recall.Read(current, 0)
		if err != nil {
			return peekedMsg{sessionID: current.ID, err: err}
		}
		return peekedMsg{
			sessionID: current.ID,
			summary:   recall.Summarize(current, items),
			items:     items,
		}
	}
}

func (m model) updatePeek(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc", "p", "v", "left", "h":
		m.screen = screenSessions
		return m, nil
	case "up", "k":
		m.peek.offset = maxInt(m.peek.offset-1, 0)
	case "down", "j":
		m.peek.offset = minInt(m.peek.offset+1, maxInt(len(m.peek.items)-m.peekHeight(), 0))
	case "pgup":
		m.peek.offset = maxInt(m.peek.offset-m.peekHeight(), 0)
	case "pgdown", " ":
		m.peek.offset = minInt(m.peek.offset+m.peekHeight(), maxInt(len(m.peek.items)-m.peekHeight(), 0))
	case "enter":
		// Straight from looking at it to merging it.
		m.screen = screenPlanning
		m.dryRun = false
		m.outcome = Outcome{Merge: []session.Session{m.peek.session}}
		return m, planCmd(m.outcome.Merge, m.events)
	case "o":
		m.screen = screenSessions
		return m.openCurrent()
	}
	return m, nil
}

// peekFacts is the summary block above the transcript. It is a method so the
// body can be sized against its real height rather than a guess, which is the
// difference between a frame that fits the terminal and one that scrolls it.
func (m model) peekFacts() []string {
	current := m.peek.session
	facts := []string{}
	add := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			facts = append(facts, styleLabel.Render(label+" ")+styleValue.Render(value))
		}
	}
	add("asked", m.peek.summary.Intent)
	add("said", m.peek.summary.Outcome)
	add("branch", current.Branch)
	if len(current.Files) > 0 {
		add("files", truncate(strings.Join(shortPaths(current.Files), ", "), maxInt(m.width-10, 20)))
	}
	if m.peek.err != nil {
		facts = append(facts, styleError.Render("could not read the transcript: "+m.peek.err.Error()))
	}
	if len(facts) == 0 {
		facts = append(facts, styleDim.Render("no transcript recorded for this session"))
	}
	return facts
}

// peekHeight is what the transcript pane gets: the terminal, less the header
// and its border, the summary block and its padding, the pane's own border,
// and the footer.
func (m model) peekHeight() int {
	return maxInt(m.height-2-(len(m.peekFacts())+2)-1-1, 1)
}

func (m model) peekView() string {
	current := m.peek.session
	header := styleTitleBar.Width(m.width).Render(
		lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Render(truncate(current.Label(), 40)) +
			styleCount.Render(fmt.Sprintf("  ·  %s  ·  %s  ·  %d edits in %d files",
				current.Tool, shortPath(current.RepoRoot), current.EditCount, len(current.Files))))

	summary := lipgloss.NewStyle().Padding(1, 1).Render(strings.Join(m.peekFacts(), "\n"))
	body := m.peekBody()

	key := func(k, label string) string { return styleKey.Render(k) + styleDim.Render(" "+label) }
	footer := styleFooter.Width(m.width).Render(strings.Join([]string{
		key("↑↓", "scroll"),
		key("enter", "merge this"),
		key("o", "open"),
		key("esc", "back"),
		key("q", "quit"),
	}, styleDim.Render("  ·  ")))

	return lipgloss.JoinVertical(lipgloss.Left, header, summary, body, footer)
}

// peekBody is the transcript itself: what was asked, what was answered, and
// every file the session touched, in order.
func (m model) peekBody() string {
	height := m.peekHeight()
	lines := make([]string, 0, height)
	for index := m.peek.offset; index < len(m.peek.items) && len(lines) < height; index++ {
		lines = append(lines, m.peekLine(m.peek.items[index]))
	}
	if len(lines) == 0 {
		// No conversation was recorded - Cursor keeps none, and a session can
		// be all tool calls. Say what is known rather than showing an empty
		// pane: the edits are a story of a kind.
		lines = append(lines, m.peekWithoutTranscript()...)
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return styleDetail.Width(m.width).Height(height).Render(strings.Join(lines, "\n"))
}

// peekWithoutTranscript summarizes a session from what hv has when the
// conversation is missing.
func (m model) peekWithoutTranscript() []string {
	current := m.peek.session
	if len(current.Files) == 0 {
		return []string{styleDim.Render("  no conversation and no edits recorded for this session")}
	}
	out := []string{styleDim.Render(fmt.Sprintf(
		"  no conversation recorded; %d edits across %d files:", current.EditCount, len(current.Files)))}
	for index, file := range current.Files {
		if index >= m.peekHeight()-1 {
			out = append(out, styleDim.Render(fmt.Sprintf("  … and %d more", len(current.Files)-index)))
			break
		}
		out = append(out, styleActiveDot.Render("  edit   ")+styleDim.Render(shortPath(file)))
	}
	return out
}

func (m model) peekLine(item recall.Item) string {
	width := maxInt(m.width-12, 20)
	switch item.Kind {
	case recall.KindUser:
		return styleKey.Render(" asked  ") + styleRow.Render(truncate(item.Text, width))
	case recall.KindAssistant:
		return styleBadgeCLI.Render(" said   ") + styleRow.Render(truncate(item.Text, width))
	default:
		verb := item.Tool
		if verb == "" {
			verb = "tool"
		}
		style := styleDim
		if verb == "edit" {
			style = styleActiveDot
		}
		return style.Render(fmt.Sprintf(" %-6s ", verb)) +
			styleDim.Render(truncate(shortPath(item.File), width))
	}
}

func shortPaths(files []string) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, shortPath(file))
	}
	return out
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
