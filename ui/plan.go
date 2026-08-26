package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/demux/matcher"
	hvplan "github.com/satoricorp/hv/plan"
	"github.com/satoricorp/hv/session"
)

// planGroup is one repository's plan. A selection can span repositories and a
// plan cannot, so merging three sessions in two repositories produces two.
type planGroup struct {
	Root     string
	Result   hvplan.Result
	Sessions []session.Session
}

// planCursor is where the reader is inside the plan: which repository, which
// revision, how far down its diff, and which pane the keys drive.
type planCursor struct {
	group    int
	revision int
	offset   int
	onDiff   bool
}

// planCmd plans each repository off the render loop.
func planCmd(sessions []session.Session, events []capture.SessionEvent) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		order, groups, problems := hvplan.GroupByRepo(ctx, sessions)
		out := []planGroup{}
		for _, root := range order {
			result, err := hvplan.Run(ctx, hvplan.Request{
				RepoRoot: root,
				Sessions: groups[root],
				Events:   events,
			})
			if err != nil {
				// One repository with nothing to plan must not lose the rest.
				problems = append(problems, fmt.Errorf("%s: %w", root, err))
				continue
			}
			out = append(out, planGroup{Root: root, Result: result, Sessions: groups[root]})
		}
		if len(out) == 0 {
			if len(problems) > 0 {
				return plannedMsg{err: problems[0]}
			}
			return plannedMsg{err: fmt.Errorf("nothing to plan")}
		}
		return plannedMsg{groups: out}
	}
}

func (m model) updatePlan(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	group, ok := m.currentGroup()
	if !ok {
		m.screen = screenSessions
		return m, nil
	}
	revisions := group.Result.Packet.Proposal.Revisions

	switch key.String() {
	case "q", "ctrl+c":
		// Quitting from the plan screen leaves nothing behind; enter is how
		// you keep it.
		m.outcome = Outcome{}
		return m, tea.Quit

	case "esc", "left", "h":
		// Back to the sessions with the marks intact, so another look at the
		// same plan costs one keystroke. Leaving the plan screen also clears
		// the outcome: quitting from the browser should print nothing.
		m.screen = screenSessions
		m.outcome = Outcome{}
		m.status = "back to sessions - enter plans the same selection again"
		return m, nil

	case "tab", "right", "l":
		m.plan.onDiff = !m.plan.onDiff
	case "up", "k":
		m.movePlan(-1, revisions)
	case "down", "j":
		m.movePlan(1, revisions)
	case "pgup":
		m.movePlan(-m.diffHeight(), revisions)
	case "pgdown", " ":
		m.movePlan(m.diffHeight(), revisions)
	case "home", "g":
		if m.plan.onDiff {
			m.plan.offset = 0
		} else {
			m.plan.revision, m.plan.offset = 0, 0
		}
	case "[":
		m.switchGroup(-1)
	case "]":
		m.switchGroup(1)
	case "enter", "a", "c":
		if m.dryRun {
			m.status = "dry run - press esc, then enter on the sessions to commit"
			return m, nil
		}
		// Commit this repository's plan. The reader is looking at it, which
		// is the only screen from which committing is an informed act - and
		// leaving without committing is q, one key away.
		m.status = "committing…"
		return m, applyCmd(group, m.target)

	case "b":
		// Name the branch these commits land on.
		m.mode = modeBranch
		return m, nil

	case "p":
		// Leave with this plan on stdout, where it can be read at leisure,
		// scrolled back to, or piped somewhere.
		return m, tea.Quit
	}
	return m, nil
}

func (m *model) movePlan(delta int, revisions []demux.RevisionProposal) {
	if m.plan.onDiff {
		m.plan.offset += delta
		if m.plan.offset < 0 {
			m.plan.offset = 0
		}
		if limit := len(m.diffLines()) - m.diffHeight(); m.plan.offset > limit {
			m.plan.offset = maxInt(limit, 0)
		}
		return
	}
	m.plan.revision += delta
	if m.plan.revision < 0 {
		m.plan.revision = 0
	}
	if m.plan.revision >= len(revisions) {
		m.plan.revision = maxInt(len(revisions)-1, 0)
	}
	// A new revision starts at the top of its own diff, not where the last
	// one was scrolled to.
	m.plan.offset = 0
}

func (m *model) switchGroup(delta int) {
	if len(m.plans) < 2 {
		return
	}
	m.plan.group = (m.plan.group + delta + len(m.plans)) % len(m.plans)
	m.plan = planCursor{group: m.plan.group}
}

func (m model) currentGroup() (planGroup, bool) {
	if m.plan.group < 0 || m.plan.group >= len(m.plans) {
		return planGroup{}, false
	}
	return m.plans[m.plan.group], true
}

func (m model) currentRevision() (demux.RevisionProposal, bool) {
	group, ok := m.currentGroup()
	if !ok {
		return demux.RevisionProposal{}, false
	}
	revisions := group.Result.Packet.Proposal.Revisions
	if m.plan.revision < 0 || m.plan.revision >= len(revisions) {
		return demux.RevisionProposal{}, false
	}
	return revisions[m.plan.revision], true
}

// diffLines is the patch text of the selected revision, in hunk order.
func (m model) diffLines() []string {
	group, ok := m.currentGroup()
	if !ok {
		return nil
	}
	revision, ok := m.currentRevision()
	if !ok {
		return nil
	}
	patches := map[string]string{}
	for _, hunk := range group.Result.Packet.Proposal.Hunks {
		patches[hunk.ID] = hunk.Patch
	}
	ids := revision.HunkIDs
	if len(ids) == 0 {
		for _, hunk := range revision.Hunks {
			ids = append(ids, hunk.ID)
		}
	}
	lines := []string{}
	for _, id := range ids {
		patch, ok := patches[id]
		if !ok {
			continue
		}
		for _, line := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
			lines = append(lines, line)
		}
	}
	return lines
}

func (m model) planningView() string {
	body := lipgloss.NewStyle().Padding(2, 3).Render(
		styleKey.Render("planning…") + "\n\n" +
			styleDim.Render("reading the working tree and grouping hunks"))
	return lipgloss.JoinVertical(lipgloss.Left, m.planHeader(), body)
}

func (m model) planHeader() string {
	group, ok := m.currentGroup()
	if !ok {
		return styleTitleBar.Width(m.width).Render(
			lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Render("hv"))
	}
	proposal := group.Result.Packet.Proposal
	left := lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Render("plan")
	where := styleValue.Render("  " + shortPath(group.Root))
	counts := styleCount.Render(fmt.Sprintf("  ·  %d revisions  ·  %d hunks  ·  %s",
		len(proposal.Revisions), len(proposal.Hunks), group.Result.Packet.State))
	if len(m.plans) > 1 {
		counts += styleKey.Render(fmt.Sprintf("  ·  repo %d/%d", m.plan.group+1, len(m.plans)))
	}
	if m.dryRun {
		counts += styleCursor.Render("  ·  dry run")
	}
	if strings.TrimSpace(m.target) != "" {
		counts += styleKey.Render("  ·  → " + m.target)
	}
	return styleTitleBar.Width(m.width).Render(left + where + counts)
}

// planView is the whole point of the screen: what demux decided, and the exact
// lines behind each decision. Nothing here has been applied - these are
// proposed branches, and the working tree is untouched.
func (m model) planView() string {
	group, ok := m.currentGroup()
	if !ok {
		return m.planningView()
	}
	revisions := group.Result.Packet.Proposal.Revisions
	listHeight := planListHeight(len(revisions), m.height)

	rows := make([]string, 0, listHeight)
	for index, revision := range revisions {
		if len(rows) >= listHeight {
			break
		}
		rows = append(rows, m.planRow(group, revision, index))
	}
	for len(rows) < listHeight {
		rows = append(rows, "")
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		m.planHeader(),
		strings.Join(rows, "\n"),
		m.diffPane(),
		m.planFooter())
}

func (m model) planRow(group planGroup, revision demux.RevisionProposal, index int) string {
	selected := index == m.plan.revision
	cursor := "  "
	if selected {
		cursor = styleCursor.Render("❯ ")
	}
	name := styleRow
	if !selected {
		name = styleRowDim
	}
	route := strings.TrimSpace(revision.TargetStack)
	if route == "" {
		route = "(unrouted)"
	}
	files := strings.Join(revision.Files, ", ")
	credit := creditLine(revision, group)

	// id, route, and credit are fixed columns; the file list takes the rest.
	fixed := 2 + 4 + 26 + 24
	width := m.width - fixed
	if width < 12 {
		width = 12
	}
	line := fmt.Sprintf("%s%s %s %s %s",
		cursor,
		name.Bold(selected).Render(fmt.Sprintf("%-3s", revision.ID)),
		styleValue.Render(truncate(files, width)+strings.Repeat(" ", maxInt(width-len([]rune(truncate(files, width))), 0))),
		styleBadgeCLI.Render(fmt.Sprintf("%-24s", truncate("→ "+route, 24))),
		styleDim.Render(truncate(credit, 22)))
	if selected {
		return styleHighlight.Width(m.width).Render(line)
	}
	return line
}

// revisionSessionIDs is who wrote a revision, from hv's own hunk links rather
// than the proposal's provenance field - demux fills that only from exact and
// fuzzy links and leaves it empty more often than not, and a commit missing
// its Why paragraph over a bookkeeping gap helps nobody.
func revisionSessionIDs(revision demux.RevisionProposal, links []matcher.HunkLink) []string {
	bySession := map[string]string{}
	for _, link := range links {
		bySession[link.HunkID] = link.SessionID
	}
	ids := revision.HunkIDs
	if len(ids) == 0 {
		for _, hunk := range revision.Hunks {
			ids = append(ids, hunk.ID)
		}
	}
	seen := map[string]struct{}{}
	out := []string{}
	for _, id := range ids {
		sessionID := bySession[id]
		if sessionID == "" {
			continue
		}
		if _, ok := seen[sessionID]; ok {
			continue
		}
		seen[sessionID] = struct{}{}
		out = append(out, sessionID)
	}
	return out
}

// creditLine names those sessions for the plan row.
func creditLine(revision demux.RevisionProposal, group planGroup) string {
	names := []string{}
	for _, id := range revisionSessionIDs(revision, group.Result.Attributed.Links) {
		names = append(names, sessionTitle(id, group.Sessions))
	}
	return strings.Join(names, ", ")
}

func sessionTitle(id string, sessions []session.Session) string {
	for _, current := range sessions {
		if current.ID == id {
			return current.Label()
		}
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (m model) diffPane() string {
	lines := m.diffLines()
	height := m.diffHeight()
	visible := make([]string, 0, height)
	for index := m.plan.offset; index < len(lines) && len(visible) < height; index++ {
		visible = append(visible, diffLine(lines[index], m.width-2))
	}
	if len(visible) == 0 {
		visible = append(visible, styleDim.Render("no patch text for this revision"))
	}
	for len(visible) < height {
		visible = append(visible, "")
	}
	border := styleDetail
	if m.plan.onDiff {
		border = border.BorderForeground(colorAccent)
	}
	return border.Width(m.width).Height(height).Render(strings.Join(visible, "\n"))
}

// diffLine colors a patch line by what it does, which is the fastest way to
// read one.
func diffLine(line string, width int) string {
	trimmed := truncate(line, maxInt(width, 8))
	switch {
	case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
		return styleDim.Render(trimmed)
	case strings.HasPrefix(line, "diff --git"), strings.HasPrefix(line, "new file"), strings.HasPrefix(line, "index "):
		return styleDim.Render(trimmed)
	case strings.HasPrefix(line, "@@"):
		return stylePRMerged.Render(trimmed)
	case strings.HasPrefix(line, "+"):
		return stylePROpen.Render(trimmed)
	case strings.HasPrefix(line, "-"):
		return stylePRClosed.Render(trimmed)
	default:
		return styleRow.Render(trimmed)
	}
}

func (m model) planFooter() string {
	if m.mode == modeBranch {
		return styleFilter.Width(m.width).Render("commit to branch: " + m.target + "█")
	}
	key := func(k, label string) string { return styleKey.Render(k) + styleDim.Render(" "+label) }
	pane := "diff"
	if m.plan.onDiff {
		pane = "revisions"
	}
	commit := key("enter", "commit")
	if m.dryRun {
		commit = styleDim.Render("dry run — nothing will be committed")
	}
	keys := []string{
		key("↑↓", "move"),
		key("tab", pane),
		commit,
		key("b", "branch"),
		key("esc", "back"),
		key("p", "print"),
	}
	if len(m.plans) > 1 {
		keys = append(keys, key("[ ]", "repo"))
	}
	keys = append(keys, key("q", "quit"))

	// Nothing on this screen has touched the repository, and a plan that looks
	// like a result invites the assumption that it was applied. The note keeps
	// its room first: a narrow terminal should lose a key, not the warning.
	note := styleDim.Render("  nothing applied yet")
	separator := styleDim.Render("  ·  ")
	line := strings.Join(keys, separator)
	for len(keys) > 1 && lipgloss.Width(line+note)+2 > m.width {
		keys = keys[:len(keys)-1]
		line = strings.Join(keys, separator)
	}
	return styleFooter.Width(m.width).Render(line + note)
}

// planListHeight gives the revision list what it needs up to a third of the
// screen, so a two-revision plan does not push the diff off the bottom.
func planListHeight(revisions, height int) int {
	limit := height / 3
	if limit < 2 {
		limit = 2
	}
	if revisions < limit {
		return maxInt(revisions, 1)
	}
	return limit
}

func (m model) diffHeight() int {
	group, _ := m.currentGroup()
	revisions := 0
	if group.Result.Packet.Proposal.Revisions != nil {
		revisions = len(group.Result.Packet.Proposal.Revisions)
	}
	height := m.height - 2 - planListHeight(revisions, m.height) - 1 - 1
	if height < 1 {
		return 1
	}
	return height
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

// PlanFrameForTest plans the given sessions and renders the plan screen once,
// for tests and for looking at a frame without a terminal.
func PlanFrameForTest(sessions []session.Session, events []capture.SessionEvent, width, height int) string {
	built := model{chosen: map[string]bool{}, width: width, height: height, events: events}
	message := planCmd(sessions, events)()
	planned, ok := message.(plannedMsg)
	if !ok || planned.err != nil {
		return fmt.Sprintf("plan failed: %v", planned.err)
	}
	built.plans = planned.groups
	built.screen = screenPlan
	return built.planView()
}
