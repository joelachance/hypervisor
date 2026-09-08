package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/satoricorp/hv/internal/text"
	"github.com/satoricorp/hv/parse"
	"github.com/satoricorp/hv/session"
)

func (m model) updateBrowse(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit

	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "pgup":
		m.moveCursor(-m.listHeight())
	case "pgdown":
		m.moveCursor(m.listHeight())
	case "home", "g":
		m.cursor, m.offset = 0, 0
	case "end", "G":
		m.cursor = len(m.rows) - 1
		m.clampCursor()

	case " ":
		// Mark in place. Advancing steals the cursor from under someone
		// marking two rows they are comparing.
		if current, ok := m.current(); ok {
			m.chosen[current.ID] = !m.chosen[current.ID]
		}

	case "enter", "shift+enter", "d":
		chosen := m.selected()
		if len(chosen) == 0 {
			m.status = "nothing selected - space marks a session, enter merges"
			return m, nil
		}
		// Shift+Enter plans without the option to commit. Not every terminal
		// can send it, so d does the same thing.
		m.dryRun = key.String() != "enter"
		m.outcome = Outcome{Merge: chosen}
		m.screen = screenPlanning
		return m, planCmd(chosen, m.events)

	case "o":
		return m.openCurrent()

	case "a":
		m.archived = !m.archived
		m.applyFilter()
	case "r", "R":
		// Open the repository picker rather than cycling blind: with a list on
		// screen you can see what you are choosing between.
		repos := m.repoChoices()
		if len(repos) <= 1 {
			m.status = "no session recorded a repository"
			break
		}
		m.mode = modeRepo
		m.repoBefore = m.repoPick
		m.repoOnlyBefore = m.repoOnly
		m.repoCursor = m.repoIndex()
	case "M":
		// Merge everything on screen, whatever the marks say.
		m.chosen = map[string]bool{}
		for _, row := range m.rows {
			m.chosen[row.ID] = true
		}
		if len(m.rows) == 0 {
			m.status = "nothing to merge"
			return m, nil
		}
		m.dryRun = false
		m.outcome = Outcome{Merge: m.selected()}
		m.screen = screenPlanning
		return m, planCmd(m.outcome.Merge, m.events)
	case "p", "v":
		if current, ok := m.current(); ok {
			m.screen = screenPeek
			m.peek = peekState{session: current}
			return m, peekCmd(current)
		}
	case "/":
		m.mode = modeFilter
		m.startEntry("/", m.filter)
	case "ctrl+r":
		m.status = "rescanning…"
		return m, refresh(m.watcher)
	}
	return m, nil
}

func (m model) header() string {
	active := 0
	for _, row := range m.rows {
		if row.State(m.now) == session.StateActive {
			active++
		}
	}
	scope := "every repo"
	switch {
	case m.repoPick != "":
		scope = shortPath(m.repoPick) + " only"
	case m.repoOnly && m.repoRoot != "":
		scope = shortPath(m.repoRoot) + " only"
	}
	left := lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Render("hv")
	counts := styleCount.Render(fmt.Sprintf(
		"  %d sessions  ·  %d active  ·  %s%s", len(m.rows), active, scope, archivedNote(m.archived)))
	// Say which store this is whenever it is not the real one. Two stores that
	// look alike but hold different sessions is a confusing way to lose time.
	if !parse.IsDefaultHome() {
		counts += styleKey.Render("  ·  store " + shortPath(parse.Home()))
	}
	return styleTitleBar.Width(m.width).Render(left + counts)
}

func archivedNote(archived bool) string {
	if archived {
		return "  ·  including archived"
	}
	return ""
}

func (m model) list() string {
	if len(m.rows) == 0 {
		return lipgloss.NewStyle().Height(m.listHeight()).Padding(1, 2).
			Render(m.emptyReason())
	}
	height := m.listHeight()
	lines := make([]string, 0, height)
	for index := m.offset; index < len(m.rows) && len(lines) < height; index++ {
		lines = append(lines, m.row(m.rows[index], index == m.cursor))
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// emptyReason says which filter emptied the list, because "no sessions" reads
// as "hv found nothing" when the truth is usually "nothing edited this repo".
func (m model) emptyReason() string {
	switch {
	case len(m.all) == 0:
		return styleDim.Render("no session logs found under " + shortPath(parse.Home()))
	case strings.TrimSpace(m.filter) != "":
		return styleDim.Render(fmt.Sprintf("nothing matches %q  ·  ", m.filter)) +
			styleKey.Render("/") + styleDim.Render(" to edit the filter")
	case m.repoOnly && m.repoRoot != "":
		return styleDim.Render(fmt.Sprintf("none of the %d sessions edited ", len(m.all))) +
			styleValue.Render(shortPath(m.repoRoot)) + "\n\n" +
			styleKey.Render("r") + styleDim.Render(" shows sessions from every repository")
	default:
		return styleDim.Render(fmt.Sprintf("no sessions in the last sweep  ·  %d known", len(m.all)))
	}
}

func (m model) row(current session.Session, highlighted bool) string {
	cursor := "  "
	if highlighted {
		cursor = styleCursor.Render("❯ ")
	}
	mark := "  "
	if m.chosen[current.ID] {
		mark = styleSelected.Render("● ")
	}

	dot := styleStaleDot.Render("○")
	if current.State(m.now) == session.StateActive {
		dot = styleActiveDot.Render("●")
	}

	titleStyle := styleRow
	if current.State(m.now) != session.StateActive {
		titleStyle = styleRowDim
	}
	if highlighted {
		titleStyle = titleStyle.Bold(true)
	}

	// The title gets whatever the terminal has left after the fixed columns,
	// so a narrow window loses the title's tail rather than wrapping the row.
	// cursor, mark, dot, repo, id, age, and the PR badge each take a fixed
	// column; the title takes what is left.
	fixed := 2 + 2 + 2 + 15 + 14 + 9 + 16
	titleWidth := m.width - fixed
	if titleWidth < 12 {
		titleWidth = 12
	}

	line := fmt.Sprintf("%s%s%s %s %s %s %s %s",
		cursor, mark, dot,
		// The repo is a column now that the browser spans all of them - a
		// title alone does not say where the work is - and it carries a colour
		// of its own so a repo can be picked out without reading the word.
		repoStyle(repoName(current)).Render(fmt.Sprintf("%-14s", text.Truncate(repoName(current), 14))),
		titleStyle.Width(titleWidth).Render(text.Truncate(current.Label(), titleWidth)),
		styleDim.Render(fmt.Sprintf("%13s", m.label(current))),
		styleDim.Render(fmt.Sprintf("%8s", humanAge(m.now-current.LastActiveAt))),
		prBadge(current))
	if highlighted {
		return styleHighlight.Width(m.width).Render(line)
	}
	return line
}

func prBadge(current session.Session) string {
	if len(current.PRs) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, pr := range current.PRs {
		counts[strings.ToUpper(pr.State)]++
	}
	parts := []string{}
	if count := counts["MERGED"]; count > 0 {
		parts = append(parts, stylePRMerged.Render(fmt.Sprintf("%d merged", count)))
	}
	if count := counts["OPEN"]; count > 0 {
		parts = append(parts, stylePROpen.Render(fmt.Sprintf("%d open", count)))
	}
	if count := counts["CLOSED"]; count > 0 {
		parts = append(parts, stylePRClosed.Render(fmt.Sprintf("%d closed", count)))
	}
	return strings.Join(parts, " ")
}

func (m model) detail() string {
	current, ok := m.current()
	if !ok {
		return styleDetail.Width(m.width).Height(detailHeight).Render("")
	}
	field := func(label, value string) string {
		if strings.TrimSpace(value) == "" {
			return ""
		}
		return styleLabel.Render(label+" ") + styleValue.Render(value)
	}
	titleSource := ""
	if current.TitleSource != "" {
		titleSource = styleDim.Render(" (" + current.TitleSource + "-named)")
	}
	lines := []string{
		lipgloss.NewStyle().Bold(true).Foreground(colorText).Render(current.Label()) + titleSource,
		field("id", current.ID),
		strings.TrimSpace(strings.Join([]string{
			field("repo", shortPath(current.RepoRoot)),
			field("branch", current.Branch),
		}, "   ")),
		strings.TrimSpace(strings.Join([]string{
			field("edits", fmt.Sprintf("%d in %d files", current.EditCount, len(current.Files))),
			field("model", current.Model),
			prLinks(current),
		}, "   ")),
	}
	if m.status != "" {
		lines = append(lines, styleDim.Render(m.status))
	}
	return styleDetail.Width(m.width).Height(detailHeight).Render(strings.Join(lines, "\n"))
}

func prLinks(current session.Session) string {
	if len(current.PRs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(current.PRs))
	for _, pr := range current.PRs {
		style := stylePROpen
		switch strings.ToUpper(pr.State) {
		case "MERGED":
			style = stylePRMerged
		case "CLOSED":
			style = stylePRClosed
		}
		parts = append(parts, style.Render(fmt.Sprintf("#%d %s", pr.Number, strings.ToLower(pr.State))))
	}
	return styleLabel.Render("prs ") + strings.Join(parts, ", ")
}

func (m model) footer() string {
	if m.mode == modeRepo {
		key := func(k, label string) string { return styleKey.Render(k) + styleDim.Render(" "+label) }
		return styleFooter.Width(m.width).Render(strings.Join([]string{
			key("r/↑↓", "choose"), key("enter", "keep"), key("esc", "cancel"),
		}, styleDim.Render("  ·  ")))
	}
	if m.mode == modeFilter {
		return styleFilter.Width(m.width).Render(m.entry.View())
	}
	key := func(k, label string) string { return styleKey.Render(k) + styleDim.Render(" "+label) }
	// Ordered by how often they are needed: a narrow terminal loses the tail
	// rather than wrapping the footer onto a second line, which would eat a
	// session row.
	keys := []string{
		key("↑↓", "move"),
		key("space", "mark"),
		key("enter", "merge"),
		key("⇧enter/d", "dry run"),
		key("p", "peek"),
		key("M", "merge all"),
		key("o", "open"),
		key("/", "filter"),
		m.scopeKey(),
		key("a", "archived"),
		key("q", "quit"),
	}
	separator := styleDim.Render("  ·  ")
	line := strings.Join(keys, separator)
	for len(keys) > 1 && lipgloss.Width(line)+2 > m.width {
		keys = keys[:len(keys)-1]
		line = strings.Join(keys, separator)
	}
	return styleFooter.Width(m.width).Render(line)
}
