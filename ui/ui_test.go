package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

func TestSpaceMarksSessionsForMerge(t *testing.T) {
	m := NewForTest(fixture())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = press(m, " ", " ") // mark row 1, advance, mark row 2
	after, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	outcome := after.(model).outcome
	if len(outcome.Merge) != 2 {
		t.Fatalf("merge = %d sessions, want both marked", len(outcome.Merge))
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
