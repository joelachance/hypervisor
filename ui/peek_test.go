package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/satoricorp/hv/recall"
	"github.com/satoricorp/hv/session"
)

func peeked() model {
	m := NewForTest(fixture()).(model)
	m.width, m.height = 100, 26
	m.screen = screenPeek
	m.peek = peekState{
		session: session.Session{ID: "aaaaaaaa-1111", Title: "benchmarks", Tool: "claude",
			RepoRoot: "/repo", EditCount: 3, Files: []string{"/repo/a.go", "/repo/b.go"}},
		summary: recall.Summary{Intent: "add a rate limiter", Outcome: "added a token bucket"},
		items: []recall.Item{
			{Kind: recall.KindUser, Text: "add a rate limiter"},
			{Kind: recall.KindTool, Tool: "edit", File: "/repo/a.go"},
			{Kind: recall.KindAssistant, Text: "added a token bucket"},
		},
	}
	return m
}

// A row says a session touched files in a repo. Peeking says what it was asked
// to do, which is what deciding whether to merge it actually needs.
func TestPeekShowsIntentOutcomeAndTranscript(t *testing.T) {
	view := peeked().peekView()
	for _, want := range []string{"benchmarks", "add a rate limiter", "added a token bucket", "a.go", "asked", "said"} {
		if !strings.Contains(view, want) {
			t.Errorf("peek is missing %q", want)
		}
	}
}

func TestPeekFitsTheTerminal(t *testing.T) {
	for _, size := range []struct{ width, height int }{{100, 26}, {80, 16}} {
		m := peeked()
		m.width, m.height = size.width, size.height
		if lines := strings.Split(m.peekView(), "\n"); len(lines) != size.height {
			t.Errorf("at %dx%d peek is %d lines, want %d", size.width, size.height, len(lines), size.height)
		}
	}
}

// Straight from looking at a session to merging it.
func TestPeekEnterMergesThatSession(t *testing.T) {
	after, _ := peeked().updatePeek(tea.KeyMsg{Type: tea.KeyEnter})
	outcome := after.(model).outcome
	if len(outcome.Merge) != 1 || outcome.Merge[0].ID != "aaaaaaaa-1111" {
		t.Fatalf("merge = %+v, want the session being looked at", outcome.Merge)
	}
	if after.(model).screen != screenPlanning {
		t.Error("enter did not start planning")
	}
}

func TestPeekEscapeGoesBack(t *testing.T) {
	after, _ := peeked().updatePeek(tea.KeyMsg{Type: tea.KeyEscape})
	if after.(model).screen != screenSessions {
		t.Fatal("esc did not return to the session list")
	}
}

func TestPeekIsHonestWhenThereIsNoTranscript(t *testing.T) {
	m := peeked()
	m.peek.summary = recall.Summary{}
	m.peek.items = nil
	m.peek.session.Files = nil
	m.peek.session.Branch = ""
	if !strings.Contains(m.peekView(), "no transcript recorded") {
		t.Fatal("peek should say when it has nothing rather than showing an empty pane")
	}
}

// Cursor records no conversation, and a session can be all tool calls. The
// edits are a story of a kind; an empty pane is not.
func TestPeekFallsBackToTheEditsWhenThereIsNoConversation(t *testing.T) {
	m := peeked()
	m.peek.items = nil
	m.peek.session.Files = []string{"/repo/a.go", "/repo/b.go"}
	m.peek.session.EditCount = 4
	view := m.peekView()
	if !strings.Contains(view, "no conversation recorded") {
		t.Error("peek did not explain why the transcript is empty")
	}
	if !strings.Contains(view, "a.go") || !strings.Contains(view, "b.go") {
		t.Error("peek did not fall back to the files the session edited")
	}
}
