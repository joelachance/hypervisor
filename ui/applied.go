package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/satoricorp/hv/apply"
	"github.com/satoricorp/hv/recall"
	"github.com/satoricorp/hv/resolve"
	"github.com/satoricorp/hv/selection"
)

type appliedMsg struct {
	entry apply.Entry
	err   error
}

type undoneMsg struct {
	entry apply.Entry
	err   error
}

// applyCmd writes one repository's plan. Only the repository on screen is
// written: applying every repository at once from a screen showing one of them
// would commit work the reader has not looked at.
func applyCmd(group planGroup, branch string) tea.Cmd {
	return func() tea.Msg {
		titles := map[string]string{}
		why := map[string]string{}
		outcomes := map[string]string{}
		for _, current := range group.Sessions {
			titles[current.ID] = current.Label()
			// The transcript is read here rather than in the render loop, and
			// it is what puts a Why paragraph in the commit message.
			items, err := recall.Read(current, 0)
			if err != nil {
				continue
			}
			summary := recall.Summarize(current, items)
			if summary.Intent != "" {
				why[current.ID] = summary.Intent
			}
			if summary.Outcome != "" {
				outcomes[current.ID] = summary.Outcome
			}
		}
		// Attribution has to come along, or a revision demux left unattributed
		// commits with no Why paragraph - the browser's merges were briefer
		// than the same merge from the command line.
		attribution := map[string][]string{}
		for _, revision := range group.Result.Packet.Proposal.Revisions {
			if ids := revisionSessionIDs(revision, group.Result.Attributed.Links); len(ids) > 0 {
				attribution[revision.ID] = ids
			}
		}
		options := apply.Options{
			SessionIDs:       selection.IDs(group.Sessions),
			Trailers:         titles,
			Why:              why,
			Outcomes:         outcomes,
			RevisionSessions: attribution,
			Branch:           branch,
		}
		// A patch that collides with what is already on the branch is settled
		// by whichever harness is installed, so enter finishes the merge
		// rather than stopping at "does not apply".
		if harness, ok := resolve.Detect(); ok {
			options.Resolver = resolve.Adapter{Harness: harness}
		}
		entry, err := apply.Apply(context.Background(), group.Root, group.Result.Packet, options)
		return appliedMsg{entry: entry, err: err}
	}
}

func undoCmd(entry apply.Entry) tea.Cmd {
	return func() tea.Msg {
		undone, err := apply.Undo(context.Background(), entry)
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
			styleDim.Render(short(commit.SHA)),
			styleBadgeCLI.Render(fmt.Sprintf("%-24s", truncate(commit.Branch, 24))),
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

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
