package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/satoricorp/hv/internal/text"
	"github.com/satoricorp/hv/journal"
	"github.com/satoricorp/hv/merge"
	"github.com/satoricorp/hv/resolve"
)

type appliedMsg struct {
	entry journal.Entry
	err   error
}

type undoneMsg struct {
	entry journal.Entry
	err   error
}

// applyCmd writes one repository's plan. Only the repository on screen is
// written: applying every repository at once from a screen showing one of them
// would commit work the reader has not looked at.
// applyCmd commits one repository's plan through the shared merge path, so
// the browser and the command line cannot drift apart again.
func applyCmd(group planGroup, branch string) tea.Cmd {
	return func() tea.Msg {
		request := merge.Request{
			RepoRoot: group.Root,
			Sessions: group.Sessions,
			Branch:   branch,
		}
		// A patch that collides with what is already on the branch is settled
		// by whichever harness is installed, so enter finishes the merge
		// rather than stopping at "does not apply".
		if harness, ok := resolve.Detect(); ok {
			request.Resolver = resolve.Adapter{Harness: harness}
		}
		result, err := merge.CommitPlanned(context.Background(), request, group.Result)
		return appliedMsg{entry: result.Entry, err: err}
	}
}

func undoCmd(entry journal.Entry) tea.Cmd {
	return func() tea.Msg {
		undone, err := journal.Undo(context.Background(), entry)
		return undoneMsg{entry: undone, err: err}
	}
}

func (m model) updateApplied(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q", "ctrl+c":
		m.outcome = Outcome{}
		return m, tea.Quit
	case "u":
		if m.applied.Undone() {
			m.status = "already undone"
			return m, nil
		}
		m.status = "undoing…"
		return m, undoCmd(m.applied)
	case "esc", "b", "left", "h":
		m.screen = screenPlan
		m.status = ""
		return m, nil
	}
	return m, nil
}

// appliedView reports what was written, and how to take it back. Undo is on
// the same screen as the result rather than a command to remember later.
func (m model) appliedView() string {
	entry := m.applied
	state := styleActiveDot.Render("committed")
	if entry.Undone() {
		state = stylePRClosed.Render("undone")
	}
	header := styleTitleBar.Width(m.width).Render(
		lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Render("applied") +
			styleValue.Render("  "+shortPath(entry.Repo)) +
			styleCount.Render(fmt.Sprintf("  ·  %d commits  ·  %d branches  ·  ", len(entry.Commits), len(entry.Branches))) +
			state)

	lines := []string{}
	for _, commit := range entry.Commits {
		lines = append(lines, fmt.Sprintf("  %s  %s  %s",
			styleDim.Render(text.Short(commit.SHA)),
			styleBadgeCLI.Render(fmt.Sprintf("%-24s", text.Truncate(commit.Branch, 24))),
			styleRow.Render(commit.Subject)))
	}
	lines = append(lines, "")
	for _, branch := range entry.Branches {
		verb := "extended"
		if branch.Created() {
			verb = "created"
		}
		if entry.Undone() {
			verb = "reset"
			if branch.Created() {
				verb = "deleted"
			}
		}
		lines = append(lines, fmt.Sprintf("  %s %s",
			styleDim.Render(fmt.Sprintf("%-9s", verb)), styleValue.Render(branch.Name)))
	}
	lines = append(lines, "")
	for _, path := range entry.Resolved {
		lines = append(lines, styleKey.Render("  settled ")+styleValue.Render(path)+
			styleDim.Render(" — it conflicted with the branch"))
	}
	lines = append(lines, styleDim.Render("  the working tree is untouched; every change is still there"))
	if m.status != "" {
		lines = append(lines, styleDim.Render("  "+m.status))
	}

	body := lipgloss.NewStyle().Height(m.height-3).Padding(1, 1).Render(strings.Join(lines, "\n"))

	key := func(k, label string) string { return styleKey.Render(k) + styleDim.Render(" "+label) }
	keys := []string{key("esc", "back to plan"), key("q", "quit")}
	if !entry.Undone() {
		keys = append([]string{key("u", "undo")}, keys...)
	}
	footer := styleFooter.Width(m.width).Render(strings.Join(keys, styleDim.Render("  ·  ")))
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}
