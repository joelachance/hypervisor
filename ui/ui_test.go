package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/satoricorp/hv/session"
)

func fixture() []session.Session {
	now := time.Now().UnixMilli()
	return []session.Session{
		{ID: "aaaaaaaa-1111", Title: "benchmarks", TitleSource: "user", Origin: session.OriginApp,
			LastActiveAt: now - 60_000, EditCount: 12, Files: []string{"a.go"}, RepoRoot: "/repo",
			PRs: []session.PullRequest{{Number: 58, State: "MERGED"}, {Number: 60, State: "OPEN"}}},
		{ID: "bbbbbbbb-2222", Title: "How demux works", Origin: session.OriginCLI,
			LastActiveAt: now - 5*60*60*1000, EditCount: 3, RepoRoot: "/repo"},
		{ID: "cccccccc-3333", Title: "archived work", Origin: session.OriginApp,
			LastActiveAt: now - 60_000, Archived: true},
	}
}

func render(t *testing.T, m tea.Model) string {
	t.Helper()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	return m.View()
}

func press(m tea.Model, keys ...string) tea.Model {
	for _, key := range keys {
		switch key {
		case " ":
			m, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
		case "enter":
			m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		default:
			m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		}
	}
	return m
}

func TestViewShowsTitlesStateAndPRs(t *testing.T) {
	view := render(t, NewForTest(fixture()))
	for _, want := range []string{"benchmarks", "How demux works", "1 merged", "1 open", "aaaaaaaa"} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q", want)
		}
	}
	if strings.Contains(view, "archived work") {
		t.Error("archived sessions must be hidden until asked for")
	}
	if !strings.Contains(view, "2 sessions") {
		t.Errorf("header should count the visible rows, got:\n%s", firstLine(view))
	}
}

func TestArchivedKeyRevealsArchivedSessions(t *testing.T) {
	m := NewForTest(fixture())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	if view := press(m, "a").View(); !strings.Contains(view, "archived work") {
		t.Fatal("pressing a did not reveal archived sessions")
	}
}

func TestFilterNarrowsRows(t *testing.T) {
	m := NewForTest(fixture())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	view := press(m, "/", "b", "e", "n").View()
	if !strings.Contains(view, "benchmarks") || strings.Contains(view, "How demux works") {
		t.Fatalf("filter did not narrow the list:\n%s", view)
	}
}

// Enter on a session you are looking at merges that session; forgetting to
// mark it first is not a mistake worth an error message.
func TestEnterFallsBackToTheHighlightedSession(t *testing.T) {
	m := NewForTest(fixture())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	after, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	outcome := after.(model).outcome
	if len(outcome.Merge) != 1 || outcome.Merge[0].ID != "aaaaaaaa-1111" {
		t.Fatalf("merge = %+v, want the highlighted session", outcome.Merge)
	}
}

// Space marks in place. Advancing would steal the cursor from under someone
// marking two rows they are comparing.
func TestSpaceMarksWithoutMovingTheCursor(t *testing.T) {
	m := NewForTest(fixture())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	marked := press(m, " ")
	if marked.(model).cursor != 0 {
		t.Fatalf("cursor = %d, want it to stay put", marked.(model).cursor)
	}
	// Two rows take a deliberate move between the marks.
	both := press(marked, "j", " ")
	after, _ := both.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if merged := after.(model).outcome.Merge; len(merged) != 2 {
		t.Fatalf("merge = %d sessions, want both marked", len(merged))
	}
}

// M merges everything on screen, whatever the marks say.
func TestMergeAllTakesEveryVisibleSession(t *testing.T) {
	m := NewForTest(fixture())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	after := press(m, "M").(model)
	if len(after.outcome.Merge) != 2 {
		t.Fatalf("merge = %d, want every visible session (the archived one is hidden)", len(after.outcome.Merge))
	}
	if after.screen != screenPlanning {
		t.Error("M did not start planning")
	}
}

// r opens a picker listing the repositories, so you can see what you are
// choosing between rather than cycling blind.
func TestRepoPickerListsRepositoriesAndFiltersAsYouMove(t *testing.T) {
	rows := fixture()
	rows[1].RepoRoot = "/other"
	m := NewForTest(rows)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	opened := press(m, "r").(model)
	if opened.mode != modeRepo {
		t.Fatal("r did not open the picker")
	}
	view := opened.View()
	for _, want := range []string{"every repo", "repo", "other", "sessions"} {
		if !strings.Contains(view, want) {
			t.Errorf("picker is missing %q", want)
		}
	}

	// Moving applies straight away: the sessions below are the answer to
	// "what would this show me".
	moved, _ := opened.updateRepoPicker(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	first := moved.(model)
	if first.repoPick == "" {
		t.Fatal("moving the cursor did not narrow to a repository")
	}
	if len(first.rows) != 1 {
		t.Fatalf("rows = %d, want only the sessions in %q", len(first.rows), first.repoPick)
	}

	// Arrows and j/k do the same as r.
	down, _ := first.updateRepoPicker(tea.KeyMsg{Type: tea.KeyDown})
	if down.(model).repoPick == first.repoPick {
		t.Error("down did not move the picker")
	}
	up, _ := down.(model).updateRepoPicker(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if up.(model).repoPick != first.repoPick {
		t.Error("k did not move back")
	}
}

// esc puts back whatever was showing before the picker opened.
func TestRepoPickerEscapeRestoresTheScope(t *testing.T) {
	rows := fixture()
	rows[1].RepoRoot = "/other"
	m := NewForTest(rows)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	opened := press(m, "r").(model)
	moved, _ := opened.updateRepoPicker(tea.KeyMsg{Type: tea.KeyDown})
	cancelled, _ := moved.(model).updateRepoPicker(tea.KeyMsg{Type: tea.KeyEscape})
	after := cancelled.(model)
	if after.mode != modeBrowse || after.repoPick != "" {
		t.Fatalf("after esc: mode=%v pick=%q, want the browser back at every repo", after.mode, after.repoPick)
	}
}

func TestRepoPickerFitsTheTerminal(t *testing.T) {
	rows := fixture()
	rows[1].RepoRoot = "/other"
	m := NewForTest(rows)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	opened := press(m, "r").(model)
	if lines := strings.Split(opened.View(), "\n"); len(lines) != 30 {
		t.Fatalf("with the picker open the frame is %d lines, want 30", len(lines))
	}
}

func TestDetailPanelDescribesTheHighlightedSession(t *testing.T) {
	view := render(t, NewForTest(fixture()))
	for _, want := range []string{"user-named", "aaaaaaaa-1111", "12 in 1 files", "#58 merged"} {
		if !strings.Contains(view, want) {
			t.Errorf("detail panel is missing %q", want)
		}
	}
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}

// The frame must fit the terminal exactly. One line too many makes the
// terminal scroll, the header disappears off the top, and every later repaint
// paints over the wrong rows - which reads as "hv showed me nothing".
func TestViewFitsTheTerminalExactly(t *testing.T) {
	for _, size := range []struct{ width, height int }{{100, 24}, {80, 30}, {120, 14}} {
		m := NewForTest(fixture())
		m, _ = m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		lines := strings.Split(m.View(), "\n")
		if len(lines) != size.height {
			t.Errorf("at %dx%d the frame is %d lines, want exactly %d",
				size.width, size.height, len(lines), size.height)
		}
		for index, line := range lines {
			if width := lipgloss.Width(line); width > size.width {
				t.Errorf("at %dx%d line %d is %d columns wide", size.width, size.height, index, width)
			}
		}
	}
}

// A terminal that has not reported its size yet must still render something.
func TestViewRendersBeforeTheSizeArrives(t *testing.T) {
	view := NewForTest(fixture()).View()
	if !strings.Contains(view, "benchmarks") {
		t.Fatalf("view before WindowSizeMsg showed no sessions:\n%s", view)
	}
}

// A repository whose sessions have all gone quiet is finished work; listing it
// makes the picker longer without making it more useful.
func TestRepoPickerHidesRepositoriesWithNothingActive(t *testing.T) {
	now := time.Now().UnixMilli()
	rows := []session.Session{
		{ID: "live", RepoRoot: "/busy", LastActiveAt: now, Files: []string{"/busy/a.go"}},
		{ID: "old", RepoRoot: "/quiet", LastActiveAt: now - 48*60*60*1000, Files: []string{"/quiet/b.go"}},
	}
	m := NewForTest(rows)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	opened := press(m, "r").(model)

	// The picker hides it; the session list below still shows every session,
	// which is the point of the two being separate.
	panel := opened.repoPanel()
	if !strings.Contains(panel, "busy") {
		t.Error("the repository with an active session is missing")
	}
	if strings.Contains(panel, "quiet") {
		t.Error("a repository with nothing active was listed")
	}
	if !strings.Contains(panel, "repos with active sessions") {
		t.Error("the picker does not say which list it is showing")
	}
}

// An empty picker answers nothing, so with nothing active it offers them all
// and says so.
func TestRepoPickerFallsBackWhenNothingIsActive(t *testing.T) {
	old := time.Now().UnixMilli() - 48*60*60*1000
	rows := []session.Session{
		{ID: "a", RepoRoot: "/one", LastActiveAt: old, Files: []string{"/one/a.go"}},
		{ID: "b", RepoRoot: "/two", LastActiveAt: old, Files: []string{"/two/b.go"}},
	}
	m := NewForTest(rows)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	panel := press(m, "r").(model).repoPanel()
	if !strings.Contains(panel, "one") || !strings.Contains(panel, "two") {
		t.Error("with nothing active the picker should offer every repository")
	}
	if !strings.Contains(panel, "no active sessions") {
		t.Error("the picker does not explain why it is showing everything")
	}
}
