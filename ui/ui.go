// Package ui is hv's fullscreen session browser.
//
// It is a view over the same packages the CLI uses - watch indexes the logs,
// desktop overlays the app's registry, selection filters, launch reopens - and
// it owns no session logic of its own. Anything the browser can do, a flag can
// do too, because a TUI that is the only way to reach a feature cannot be
// scripted, piped, or run over ssh in a hurry.
package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/hv/apply"
	"github.com/satoricorp/hv/desktop"
	"github.com/satoricorp/hv/launch"
	"github.com/satoricorp/hv/parse"
	"github.com/satoricorp/hv/selection"
	"github.com/satoricorp/hv/session"
	"github.com/satoricorp/hv/watch"
)

// Outcome is what the browser was asked to do once it exits. The TUI never
// plans or merges itself: it collects an intent and hands it back, so the
// planning path stays identical whether a person clicked or typed flags.
type Outcome struct {
	// Merge holds the sessions whose plan should be printed on exit, empty if
	// none. The browser plans on screen; this is only for carrying the same
	// plan out to a normal terminal where it can be scrolled back to or piped.
	Merge []session.Session
}

// Options configure a browser run.
type Options struct {
	// MaxAge bounds how far back the log sweep reads.
	MaxAge time.Duration
	// RepoRoot is the repository hv was started in, if any. It is not a
	// filter - the browser shows every session by default - it is what the
	// scope key toggles to when someone wants to narrow to where they are.
	RepoRoot string
	// ScopeToRepo starts narrowed to RepoRoot instead of showing everything.
	ScopeToRepo bool
}

type mode int

const (
	modeBrowse mode = iota
	modeFilter
)

// screen is which view is on top. The browser picks sessions; the plan screen
// shows what demux made of them, which is the part that answers "what did it
// actually do".
type screen int

const (
	screenSessions screen = iota
	screenPlanning
	screenPlan
	screenApplied
)

type model struct {
	watcher  *watch.Watcher
	all      []session.Session
	rows     []session.Session
	chosen   map[string]bool
	cursor   int
	offset   int
	width    int
	height   int
	mode     mode
	screen   screen
	events   []capture.SessionEvent
	plans    []planGroup
	plan     planCursor
	dryRun   bool
	applied  apply.Entry
	filter   string
	archived bool
	repoOnly bool
	repoRoot string
	short    map[string]string
	status   string
	err      error
	outcome  Outcome
	now      int64
}

type refreshedMsg struct {
	sessions []session.Session
	events   []capture.SessionEvent
	err      error
}

type plannedMsg struct {
	groups []planGroup
	err    error
}

type tickMsg time.Time

// Run opens the browser and returns what the person asked for.
func Run(ctx context.Context, options Options) (Outcome, error) {
	watcher := watch.New(nil, 0)
	watcher.MaxAge = options.MaxAge

	initial := model{
		watcher:  watcher,
		chosen:   map[string]bool{},
		repoRoot: options.RepoRoot,
		repoOnly: options.ScopeToRepo && options.RepoRoot != "",
		now:      time.Now().UnixMilli(),
		status:   "reading session logs…",
	}
	program := tea.NewProgram(initial, tea.WithAltScreen(), tea.WithContext(ctx))
	final, err := program.Run()
	if err != nil {
		return Outcome{}, err
	}
	done, _ := final.(model)
	return done.outcome, done.err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(refresh(m.watcher), tick())
}

// refresh sweeps the logs off the UI thread; a first sweep over a year of
// transcripts takes seconds, and blocking the render loop for it would look
// like a hang.
func refresh(watcher *watch.Watcher) tea.Cmd {
	return func() tea.Msg {
		events, err := watcher.Scan()
		if err != nil {
			return refreshedMsg{err: err}
		}
		records, err := desktop.Load()
		if err != nil {
			// The registry is an enhancement; losing it costs titles, not
			// sessions.
			return refreshedMsg{sessions: watcher.Sessions(), events: events}
		}
		return refreshedMsg{sessions: desktop.Apply(watcher.Sessions(), records), events: events}
	}
}

// tick re-renders on a slow beat so "2m ago" and the active dot stay true
// without the person pressing anything.
func tick() tea.Cmd {
	return tea.Tick(15*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case refreshedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.all = msg.sessions
		if len(msg.events) > 0 {
			m.events = msg.events
		}
		m.status = ""
		m.applyFilter()
		return m, nil

	case appliedMsg:
		if msg.err != nil {
			m.status = "could not commit: " + msg.err.Error()
			return m, nil
		}
		m.applied = msg.entry
		m.screen = screenApplied
		m.status = ""
		return m, nil

	case undoneMsg:
		if msg.err != nil {
			m.status = "could not undo: " + msg.err.Error()
			return m, nil
		}
		m.applied = msg.entry
		m.status = ""
		return m, nil

	case plannedMsg:
		if msg.err != nil {
			m.screen = screenSessions
			m.status = "could not plan: " + msg.err.Error()
			return m, nil
		}
		m.plans = msg.groups
		m.plan = planCursor{}
		m.screen = screenPlan
		return m, nil

	case tickMsg:
		m.now = time.Now().UnixMilli()
		return m, tea.Batch(refresh(m.watcher), tick())

	case launchedMsg:
		if msg.err != nil {
			m.status = "could not open: " + msg.err.Error()
		} else {
			m.status = "opened " + msg.detail
		}
		return m, nil

	case tea.KeyMsg:
		switch {
		case m.mode == modeFilter:
			return m.updateFilter(msg)
		case m.screen == screenPlan:
			return m.updatePlan(msg)
		case m.screen == screenApplied:
			return m.updateApplied(msg)
		case m.screen == screenPlanning:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		default:
			return m.updateBrowse(msg)
		}
	}
	return m, nil
}

func (m model) updateFilter(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyEnter, tea.KeyEscape:
		m.mode = modeBrowse
		if key.Type == tea.KeyEscape {
			m.filter = ""
			m.applyFilter()
		}
	case tea.KeyBackspace:
		if m.filter != "" {
			m.filter = m.filter[:len(m.filter)-1]
			m.applyFilter()
		}
	case tea.KeyRunes, tea.KeySpace:
		m.filter += string(key.Runes)
		if key.Type == tea.KeySpace {
			m.filter += " "
		}
		m.applyFilter()
	}
	return m, nil
}

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
		if current, ok := m.current(); ok {
			m.chosen[current.ID] = !m.chosen[current.ID]
			m.moveCursor(1)
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
	case "r":
		if m.repoRoot == "" {
			m.status = "not inside a repository, so there is nothing to narrow to"
			break
		}
		m.repoOnly = !m.repoOnly
		m.applyFilter()
	case "/":
		m.mode = modeFilter
	case "ctrl+r":
		m.status = "rescanning…"
		return m, refresh(m.watcher)
	}
	return m, nil
}

type launchedMsg struct {
	detail string
	err    error
}

// openCurrent hands the terminal to the session. An attach or a resume takes
// over this process, so the browser releases the screen first; a deep link
// hands off to another application and the browser stays up.
func (m model) openCurrent() (tea.Model, tea.Cmd) {
	current, ok := m.current()
	if !ok {
		return m, nil
	}
	target, err := launch.Plan(context.Background(), current)
	if err != nil {
		m.status = err.Error()
		return m, nil
	}
	command := exec.Command(target.Command[0], target.Command[1:]...)
	if target.Method == launch.MethodCLI {
		command.Dir = current.RepoRoot
	}
	if !target.Replace {
		return m, func() tea.Msg {
			err := command.Run()
			return launchedMsg{detail: current.Label() + " in the app", err: err}
		}
	}
	return m, tea.ExecProcess(command, func(err error) tea.Msg {
		return launchedMsg{detail: current.Label(), err: err}
	})
}

func (m *model) applyFilter() {
	filter := selection.Filter{IncludeArchived: m.archived}
	if m.repoOnly && m.repoRoot != "" {
		filter.TouchedUnder = m.repoRoot
	}
	rows := selection.Apply(m.all, filter)
	if needle := strings.ToLower(strings.TrimSpace(m.filter)); needle != "" {
		matched := rows[:0]
		for _, row := range rows {
			haystack := strings.ToLower(row.Label() + " " + row.ID + " " + row.RepoRoot + " " + row.Branch)
			if strings.Contains(haystack, needle) {
				matched = append(matched, row)
			}
		}
		rows = matched
	}
	sort.SliceStable(rows, func(left, right int) bool {
		return rows[left].LastActiveAt > rows[right].LastActiveAt
	})
	m.rows = rows
	m.short = session.Abbreviate(rows, 8)
	m.clampCursor()
}

func (m *model) moveCursor(delta int) {
	m.cursor += delta
	m.clampCursor()
}

func (m *model) clampCursor() {
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	height := m.listHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if height > 0 && m.cursor >= m.offset+height {
		m.offset = m.cursor - height + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m model) current() (session.Session, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return session.Session{}, false
	}
	return m.rows[m.cursor], true
}

// selected returns the marked sessions, falling back to the highlighted one -
// pressing enter on a session you are looking at should merge that session,
// not complain that you forgot to mark it.
func (m model) selected() []session.Session {
	chosen := []session.Session{}
	for _, row := range m.rows {
		if m.chosen[row.ID] {
			chosen = append(chosen, row)
		}
	}
	if len(chosen) == 0 {
		if current, ok := m.current(); ok {
			chosen = append(chosen, current)
		}
	}
	return chosen
}

// listHeight is the rows left after the chrome.
//
// Getting this wrong by one costs the header: the terminal scrolls to fit the
// extra line and the top of the frame is gone, which looks like hv rendered
// nothing useful.
func (m model) listHeight() int {
	height := m.height - chromeHeight
	if height < 1 {
		return 1
	}
	return height
}

const (
	// detailHeight is the panel's own rows; it also draws a top border.
	detailHeight = 6
	// chromeHeight is every row the list does not get: the header and its
	// border, the detail panel plus its border, and the footer.
	chromeHeight = 2 + detailHeight + 1 + 1
)

// defaultWidth and defaultHeight stand in until the terminal reports its size.
// Some terminals and multiplexers are slow to answer, and a browser that shows
// nothing until they do looks broken rather than patient.
const (
	defaultWidth  = 100
	defaultHeight = 24
)

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		m.width, m.height = defaultWidth, defaultHeight
	}
	if m.err != nil {
		return styleError.Render("hv: " + m.err.Error())
	}
	switch m.screen {
	case screenPlanning:
		return m.planningView()
	case screenPlan:
		return m.planView()
	case screenApplied:
		return m.appliedView()
	}
	sections := []string{m.header(), m.list(), m.detail(), m.footer()}
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

func (m model) header() string {
	active := 0
	for _, row := range m.rows {
		if row.State(m.now) == session.StateActive {
			active++
		}
	}
	scope := "every repo"
	if m.repoOnly && m.repoRoot != "" {
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

	// Where it runs when that is known, otherwise which tool it is - Codex and
	// Cursor record no entrypoint, and a blank column looks like missing data.
	origin := styleDim.Render(fmt.Sprintf("%-3s", toolBadge(current.Tool)))
	switch current.Origin {
	case session.OriginApp:
		origin = styleBadgeApp.Render("app")
	case session.OriginCLI:
		origin = styleBadgeCLI.Render("cli")
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
	// cursor, mark, dot, origin, repo, id, age, and the PR badge each take a
	// fixed column; the title takes what is left.
	fixed := 2 + 2 + 2 + 4 + 14 + 14 + 9 + 16
	titleWidth := m.width - fixed
	if titleWidth < 12 {
		titleWidth = 12
	}

	line := fmt.Sprintf("%s%s%s %s %s %s %s %s %s",
		cursor, mark, dot, origin,
		// The repo is a column now that the browser spans all of them: a
		// title alone does not say where the work is.
		styleDim.Render(fmt.Sprintf("%-13s", truncate(repoName(current), 13))),
		titleStyle.Width(titleWidth).Render(truncate(current.Label(), titleWidth)),
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
	if m.mode == modeFilter {
		return styleFilter.Width(m.width).Render("/" + m.filter + "█")
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

// scopeKey names what r will do next rather than what is on, so the footer
// reads as an action.
func (m model) scopeKey() string {
	if m.repoOnly {
		return styleKey.Render("r") + styleDim.Render(" every repo")
	}
	return styleKey.Render("r") + styleDim.Render(" this repo")
}

func humanAge(millis int64) string {
	age := time.Duration(millis) * time.Millisecond
	switch {
	case age < time.Minute:
		return "now"
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd", int(age.Hours()/24))
	}
}

// toolBadge is a three-letter tool name, cut rather than elided: an ellipsis
// in a three-character column costs a third of the word to say nothing.
func toolBadge(tool string) string {
	if len(tool) <= 3 {
		return tool
	}
	return tool[:3]
}

// repoName is the last path element of a session's directory, which is what
// people call the repository.
func repoName(current session.Session) string {
	root := strings.TrimSpace(current.RepoRoot)
	if root == "" {
		return ""
	}
	return filepath.Base(root)
}

func (m model) label(current session.Session) string {
	if short, ok := m.short[current.ID]; ok {
		return short
	}
	return current.ID
}

func shortPath(path string) string {
	if home := homeDir(); home != "" && strings.HasPrefix(path, home) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit < 2 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}

func homeDir() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return dir
}

// NewForTest builds a browser model over a fixed session list, for tests and
// for rendering a frame without a terminal.
func NewForTest(sessions []session.Session) tea.Model {
	built := model{chosen: map[string]bool{}, now: time.Now().UnixMilli(), all: sessions}
	built.applyFilter()
	return built
}

// NewForTestScoped builds a browser model already scoped to a repository, for
// exercising the empty state.
func NewForTestScoped(sessions []session.Session, repoRoot string) tea.Model {
	built := model{chosen: map[string]bool{}, now: time.Now().UnixMilli(), all: sessions,
		repoRoot: repoRoot, repoOnly: true}
	built.applyFilter()
	return built
}
