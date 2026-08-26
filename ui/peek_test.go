package ui

import (
	"fmt"
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
func TestPeekLeadsWithASummary(t *testing.T) {
	view := peeked().peekView()
	for _, want := range []string{"benchmarks", "Asked: “add a rate limiter”", "Reported:", "Edited"} {
		if !strings.Contains(view, want) {
			t.Errorf("peek is missing %q", want)
		}
	}
}

// Three sentences at most: a summary someone has to read carefully is not one.
func TestPeekSummaryIsAtMostThreeSentences(t *testing.T) {
	m := peeked()
	m.peek.session.Files = []string{"/repo/a.go", "/repo/b.go", "/repo/c.go", "/repo/d.go", "/repo/e.go"}
	sentences := m.peekSentences()
	if len(sentences) > 3 {
		t.Fatalf("summary has %d sentences: %q", len(sentences), sentences)
	}
	text := strings.Join(sentences, " ")
	if !strings.Contains(text, "and 2 more") {
		t.Errorf("summary = %q, want the file list capped rather than spilling", text)
	}
}

// The conversation, and only the conversation: a list of file reads is what
// the session row already shows.
func TestPeekShowsExchangesWithoutToolCalls(t *testing.T) {
	view := peeked().peekView()
	if !strings.Contains(view, "you") || !strings.Contains(view, "add a rate limiter") {
		t.Error("the exchange is missing what was asked")
	}
	if !strings.Contains(view, "added a token bucket") {
		t.Error("the exchange is missing what came back")
	}
	if strings.Contains(view, "edit   ") {
		t.Error("tool calls leaked into the conversation pane")
	}
}

// Only the last four rounds: enough to see where a session ended up, short
// enough to read at a glance.
func TestPeekShowsTheLastFourExchanges(t *testing.T) {
	m := peeked()
	m.height = 60
	m.peek.items = nil
	for round := 1; round <= 6; round++ {
		m.peek.items = append(m.peek.items,
			recall.Item{Kind: recall.KindUser, Text: fmt.Sprintf("question %d", round)},
			recall.Item{Kind: recall.KindTool, Tool: "edit", File: "/repo/a.go"},
			recall.Item{Kind: recall.KindAssistant, Text: fmt.Sprintf("answer %d", round)})
	}
	view := m.peekView()
	if strings.Contains(view, "question 2") {
		t.Error("an exchange older than the last four is shown")
	}
	for round := 3; round <= 6; round++ {
		if !strings.Contains(view, fmt.Sprintf("question %d", round)) {
			t.Errorf("round %d is missing", round)
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
	if !strings.Contains(m.peekView(), "No transcript and no edits") {
		t.Fatalf("peek should say when it has nothing:\n%s", m.peekView())
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
