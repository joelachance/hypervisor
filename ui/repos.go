package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/satoricorp/hv/internal/text"
	"github.com/satoricorp/hv/selection"
	"github.com/satoricorp/hv/session"
)

// scopeKey names what r will do next rather than what is on, so the footer
// reads as an action.
func (m model) scopeKey() string {
	return styleKey.Render("r") + styleDim.Render(" repo")
}

// repoChoices is what the picker offers: every repository, then each one with
// a session someone is still working in. The empty string is "every repo".
//
// A repository whose sessions have all gone quiet is finished work, and
// listing it makes the picker longer without making it more useful. When
// nothing at all is active the full list is offered instead, because an empty
// picker answers nothing.
func (m model) repoChoices() []string {
	now := time.Now().UnixMilli()
	seen := map[string]struct{}{}
	active, all := []string{}, []string{}
	for _, row := range m.all {
		root := strings.TrimSpace(row.RepoRoot)
		if root == "" {
			continue
		}
		if _, ok := seen[root]; !ok {
			seen[root] = struct{}{}
			all = append(all, root)
		}
		if row.State(now) == session.StateActive && !containsString(active, root) {
			active = append(active, root)
		}
	}
	repos := active
	if len(repos) == 0 {
		repos = all
	}
	sort.Strings(repos)
	return append([]string{""}, repos...)
}

// repoPickerNote says which list is on screen, so a missing repository is
// explained rather than surprising.
func (m model) repoPickerNote() string {
	now := time.Now().UnixMilli()
	for _, row := range m.all {
		if row.State(now) == session.StateActive {
			return "repos with active sessions"
		}
	}
	return "no active sessions - showing every repo"
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// repoIndex is where the current pick sits in the choices.
func (m model) repoIndex() int {
	for index, repo := range m.repoChoices() {
		if repo == m.repoPick {
			return index
		}
	}
	return 0
}

// updateRepoPicker drives the list of repositories. The choice applies as the
// cursor moves, so the sessions below are the answer to "what would this show
// me" rather than something to find out after committing to it.
func (m model) updateRepoPicker(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	choices := m.repoChoices()
	switch key.String() {
	case "r", "down", "j", "tab":
		m.repoCursor = (m.repoCursor + 1) % len(choices)
	case "R", "up", "k", "shift+tab":
		m.repoCursor = (m.repoCursor - 1 + len(choices)) % len(choices)
	case "enter":
		m.mode = modeBrowse
		return m, nil
	case "esc":
		m.mode = modeBrowse
		m.repoPick = m.repoBefore
		m.selectRepo()
		// selectRepo clears the -repo launch scope because a pick replaces
		// it; a cancel restores it instead.
		m.repoOnly = m.repoOnlyBefore
		m.applyFilter()
		return m, nil
	case "q", "ctrl+c":
		return m, tea.Quit
	default:
		return m, nil
	}
	m.repoPick = choices[m.repoCursor]
	m.selectRepo()
	return m, nil
}

func (m *model) selectRepo() {
	m.repoOnly = false
	m.cursor, m.offset = 0, 0
	m.applyFilter()
}

// repoPanel is the picker itself, above the sessions it is filtering.
func (m model) repoPanel() string {
	choices := m.repoChoices()
	rows := make([]string, 0, len(choices)+1)
	rows = append(rows, styleDim.Render("  "+m.repoPickerNote()))
	for index, repo := range choices {
		label := "every repo"
		style := styleDim
		if repo != "" {
			label = repoName(sessionAt(repo))
			style = repoStyle(label)
		}
		total, live := m.countIn(repo)
		cursor := "  "
		if index == m.repoCursor {
			cursor = styleCursor.Render("❯ ")
		}
		counts := fmt.Sprintf("%d session%s", total, text.Plural("s", total))
		if live > 0 {
			counts += fmt.Sprintf(", %d active", live)
		}
		line := fmt.Sprintf("%s%s %s", cursor,
			style.Render(fmt.Sprintf("%-16s", text.Truncate(label, 16))),
			styleDim.Render(counts))
		if index == m.repoCursor {
			line = styleHighlight.Width(m.width).Render(line)
		}
		rows = append(rows, line)
	}
	// Bottom border only: the header already drew a line above this.
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, false, true, false).
		BorderForeground(colorDim).Width(m.width).
		Render(strings.Join(rows, "\n"))
}

// countIn is how many sessions the picker would show for a choice, under the
// filters already in force - an archived-only repository should not advertise
// sessions the list will not show.
func (m model) countIn(repo string) (total, active int) {
	now := time.Now().UnixMilli()
	rows := selection.Apply(m.all, selection.Filter{IncludeArchived: m.archived, TouchedUnder: repo})
	for _, row := range rows {
		if row.State(now) == session.StateActive {
			active++
		}
	}
	return len(rows), active
}

// plural is the suffix for a count.
func plural(suffix string, count int) string {
	if count == 1 {
		return ""
	}
	return suffix
}

// sessionAt is a stand-in session carrying a repository path, so the panel can
// name and colour a repo with the same code the rows use.
func sessionAt(root string) session.Session { return session.Session{RepoRoot: root} }
