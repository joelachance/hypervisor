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

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/hv/desktop"
	"github.com/satoricorp/hv/journal"
	"github.com/satoricorp/hv/launch"
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
	modeBranch
	modeRepo
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
	screenPeek
)

type model struct {
	watcher    *watch.Watcher
	all        []session.Session
	rows       []session.Session
	chosen     map[string]bool
	cursor     int
	offset     int
	width      int
	height     int
	mode       mode
	screen     screen
	events     []capture.SessionEvent
	plans      []planGroup
	plan       planCursor
	dryRun     bool
	target     string
	// entry edits whichever text field the mode says is open - the filter or
	// the branch target - so both get the same paste and readline keys.
	entry textinput.Model
	applied    journal.Entry
	filter     string
	archived   bool
	repoOnly   bool
	repoRoot   string
	repoPick   string
	repoCursor int
	repoBefore string
	// repoOnlyBefore remembers a -repo launch scope while the picker is open,
	// so cancelling out of it restores that scope instead of widening to
	// every repository.
	repoOnlyBefore bool
	peek           peekState
	short          map[string]string
	status         string
	err            error
	outcome        Outcome
	now            int64
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

	case peekedMsg:
		if msg.sessionID != m.peek.session.ID {
			return m, nil
		}
		m.peek.items, m.peek.summary, m.peek.err = msg.items, msg.summary, msg.err
		m.peek.offset = 0
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
		case m.mode == modeRepo:
			return m.updateRepoPicker(msg)
		case m.mode == modeBranch:
			return m.updateBranch(msg)
		case m.mode == modeFilter:
			return m.updateFilter(msg)
		case m.screen == screenPlan:
			return m.updatePlan(msg)
		case m.screen == screenApplied:
			return m.updateApplied(msg)
		case m.screen == screenPeek:
			return m.updatePeek(msg)
		case m.screen == screenPlanning:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		default:
			return m.updateBrowse(msg)
		}
	}
	// Whatever the entry's own commands produce - a clipboard read for
	// ctrl+v, mostly - comes back as a message only the entry understands.
	if m.mode == modeFilter || m.mode == modeBranch {
		return m.forwardToEntry(message)
	}
	return m, nil
}

// startEntry opens the shared text field over an existing value, cursor at
// the end. The cursor is static because nothing else in the program ticks,
// and a blink loop is not worth plumbing messages for.
func (m *model) startEntry(prompt, value string) {
	entry := textinput.New()
	entry.Prompt = prompt
	entry.SetValue(value)
	entry.Cursor.SetMode(cursor.CursorStatic)
	entry.Focus()
	m.entry = entry
}

// forwardToEntry hands a message to the text field and mirrors its value into
// whichever string the open mode edits, filtering live as the filter changes.
func (m model) forwardToEntry(message tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.entry, cmd = m.entry.Update(message)
	switch m.mode {
	case modeFilter:
		if m.filter != m.entry.Value() {
			m.filter = m.entry.Value()
			m.applyFilter()
		}
	case modeBranch:
		m.target = m.entry.Value()
	}
	return m, cmd
}

func (m model) updateFilter(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyEnter, tea.KeyEscape:
		m.mode = modeBrowse
		if key.Type == tea.KeyEscape {
			m.filter = ""
			m.applyFilter()
		}
		return m, nil
	}
	return m.forwardToEntry(key)
}

// updateBranch reads the branch to commit to. Typing it here is the difference
// between commits parked on a name demux invented and commits where the person
// actually wants them.
func (m model) updateBranch(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyEnter:
		m.mode = modeBrowse
		if strings.TrimSpace(m.target) == "" {
			m.status = "no target - each revision goes to the branch demux proposed"
		} else {
			m.status = "committing to " + m.target
		}
		return m, nil
	case tea.KeyEscape:
		m.mode = modeBrowse
		m.target = ""
		m.status = "target cleared"
		return m, nil
	}
	return m.forwardToEntry(key)
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
	switch {
	case m.repoPick != "":
		filter.TouchedUnder = m.repoPick
	case m.repoOnly && m.repoRoot != "":
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
	if m.mode == modeRepo {
		// The picker takes its rows from the list, plus its note and border.
		height -= len(m.repoChoices()) + 2
	}
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
	case screenPeek:
		return m.peekView()
	}
	sections := []string{m.header()}
	if m.mode == modeRepo {
		sections = append(sections, m.repoPanel())
	}
	sections = append(sections, m.list(), m.detail(), m.footer())
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
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
