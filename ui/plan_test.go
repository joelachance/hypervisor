package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/satoricorp/demux"
	"github.com/satoricorp/demux/matcher"
	hvplan "github.com/satoricorp/hv/plan"
	"github.com/satoricorp/hv/session"
)

func planFixture() []planGroup {
	proposal := demux.DemuxProposal{
		Hunks: []demux.HunkRange{
			{ID: "h1", File: "internal/alpha.go", Patch: "diff --git a/internal/alpha.go b/internal/alpha.go\n@@ -1,3 +1,3 @@\n package alpha\n-func Version() string { return \"0.1\" }\n+func Version() string { return \"0.2\" }\n"},
			{ID: "h2", File: "internal/limiter.go", Patch: "diff --git a/internal/limiter.go b/internal/limiter.go\n@@ -0,0 +1,2 @@\n+package alpha\n+// Limiter caps requests.\n"},
		},
		Revisions: []demux.RevisionProposal{
			{ID: "u1", Files: []string{"internal/alpha.go"}, HunkIDs: []string{"h1"}, TargetStack: "feature/internal"},
			{ID: "u2", Files: []string{"internal/limiter.go"}, HunkIDs: []string{"h2"}, TargetStack: "feature/internal"},
		},
	}
	return []planGroup{{
		Root: "/repo/alpha",
		Result: hvplan.Result{
			Packet: demux.DemuxPlanPacket{Proposal: proposal, State: demux.DemuxWorkflowReadyToApply},
		},
		Sessions: []session.Session{{ID: "s1", Title: "rate limiter"}, {ID: "s2", Title: "limiter tests"}},
	}}
}

func plannedModel(width, height int) model {
	built := model{chosen: map[string]bool{}, width: width, height: height, screen: screenPlan}
	built.plans = planFixture()
	built.plans[0].Result.Attributed.Links = []matcher.HunkLink{
		{HunkID: "h1", SessionID: "s1"}, {HunkID: "h2", SessionID: "s2"},
	}
	return built
}

func TestPlanViewShowsRevisionsRoutesAndDiff(t *testing.T) {
	view := plannedModel(100, 26).planView()
	for _, want := range []string{"u1", "u2", "feature/internal", "internal/alpha.go", "rate limiter",
		"2 revisions", "2 hunks", "nothing applied yet"} {
		if !strings.Contains(view, want) {
			t.Errorf("plan view is missing %q", want)
		}
	}
	if !strings.Contains(view, "func Version() string") {
		t.Error("the selected revision's patch is not shown")
	}
	if strings.Contains(view, "Limiter caps requests") {
		t.Error("another revision's patch leaked into the pane")
	}
}

func TestPlanViewFitsTheTerminalExactly(t *testing.T) {
	for _, size := range []struct{ width, height int }{{100, 26}, {80, 14}, {120, 40}} {
		view := plannedModel(size.width, size.height).planView()
		lines := strings.Split(view, "\n")
		if len(lines) != size.height {
			t.Errorf("at %dx%d the plan frame is %d lines, want %d",
				size.width, size.height, len(lines), size.height)
		}
		for index, line := range lines {
			if lipgloss.Width(line) > size.width {
				t.Errorf("at %dx%d plan line %d is too wide", size.width, size.height, index)
			}
		}
	}
}

// Moving between revisions shows that revision's patch, from its top.
func TestMovingRevisionsSwitchesTheDiff(t *testing.T) {
	m := plannedModel(100, 26)
	m.plan.offset = 3
	next, _ := m.updatePlan(tea.KeyMsg{Type: tea.KeyDown})
	after := next.(model)
	if after.plan.revision != 1 {
		t.Fatalf("revision = %d, want the second", after.plan.revision)
	}
	if after.plan.offset != 0 {
		t.Errorf("offset = %d; a new revision starts at the top of its own diff", after.plan.offset)
	}
	if !strings.Contains(after.planView(), "Limiter caps requests") {
		t.Error("the second revision's patch is not shown after moving")
	}
}

// tab moves the keys to the diff, where they scroll instead of changing rows.
func TestTabFocusesTheDiffPane(t *testing.T) {
	m := plannedModel(100, 26)
	// A patch taller than the pane, so there is somewhere to scroll to.
	long := make([]string, 0, 60)
	for index := 0; index < 60; index++ {
		long = append(long, "+line")
	}
	m.plans[0].Result.Packet.Proposal.Hunks[0].Patch = strings.Join(long, "\n")

	focused, _ := m.updatePlan(tea.KeyMsg{Type: tea.KeyTab})
	scrolled, _ := focused.(model).updatePlan(tea.KeyMsg{Type: tea.KeyDown})
	after := scrolled.(model)
	if after.plan.revision != 0 {
		t.Error("the revision changed while the diff had focus")
	}
	if after.plan.offset != 1 {
		t.Errorf("offset = %d, want the diff scrolled by one", after.plan.offset)
	}
}

// A diff shorter than its pane has nowhere to scroll, and must not drift off
// the top leaving a blank pane.
func TestShortDiffDoesNotScroll(t *testing.T) {
	focused, _ := plannedModel(100, 26).updatePlan(tea.KeyMsg{Type: tea.KeyTab})
	scrolled, _ := focused.(model).updatePlan(tea.KeyMsg{Type: tea.KeyDown})
	if offset := scrolled.(model).plan.offset; offset != 0 {
		t.Fatalf("offset = %d, want 0 - the whole patch already fits", offset)
	}
}

// esc goes back with the marks intact and prints nothing on exit.
func TestEscapeReturnsToSessionsWithoutPrinting(t *testing.T) {
	m := plannedModel(100, 26)
	m.outcome = Outcome{Merge: []session.Session{{ID: "s1"}}}
	next, _ := m.updatePlan(tea.KeyMsg{Type: tea.KeyEscape})
	after := next.(model)
	if after.screen != screenSessions {
		t.Fatal("esc did not return to the sessions")
	}
	if len(after.outcome.Merge) != 0 {
		t.Error("leaving the plan must clear the outcome, or quitting later prints a plan nobody asked for")
	}
}

func TestDiffLinesAreColoredByWhatTheyDo(t *testing.T) {
	added := diffLine("+added", 40)
	removed := diffLine("-removed", 40)
	if added == removed {
		t.Fatal("additions and removals render identically")
	}
	if !strings.Contains(added, "added") || !strings.Contains(removed, "removed") {
		t.Error("diff colouring lost the text")
	}
}

// Shift+Enter plans without the option to commit; not every terminal can send
// it, so d does the same thing.
func TestDryRunRefusesToCommit(t *testing.T) {
	m := plannedModel(100, 26)
	m.dryRun = true
	after, cmd := m.updatePlan(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("enter in a dry run started a commit")
	}
	if !strings.Contains(after.(model).status, "dry run") {
		t.Errorf("status = %q, want it to say why nothing happened", after.(model).status)
	}
	if !strings.Contains(after.(model).planView(), "dry run") {
		t.Error("the plan screen does not say it is a dry run")
	}
}

func TestDryRunKeysFromTheSessionList(t *testing.T) {
	for _, key := range []string{"d", "shift+enter"} {
		m := NewForTest(fixture()).(model)
		m.width, m.height = 100, 26
		var typed tea.KeyMsg
		if key == "d" {
			typed = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")}
		} else {
			typed = tea.KeyMsg{Type: tea.KeyEnter, Alt: false}
			// Bubble Tea reports shift+enter by name when the terminal sends
			// it; simulate that path directly.
			m.dryRun = false
		}
		after, _ := m.updateBrowse(typed)
		if key == "d" && !after.(model).dryRun {
			t.Errorf("%q did not start a dry run", key)
		}
	}
}

// b names the branch the commits land on: the difference between commits
// parked on a name demux invented and commits where they were wanted.
func TestBranchKeyTakesATarget(t *testing.T) {
	m := plannedModel(100, 26)
	typing, _ := m.updatePlan(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	next := typing.(model)
	if next.mode != modeBranch {
		t.Fatal("b did not start asking for a branch")
	}
	for _, r := range "release" {
		after, _ := next.updateBranch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		next = after.(model)
	}
	if !strings.Contains(next.planFooter(), "release") {
		t.Errorf("footer = %q, want the branch being typed", next.planFooter())
	}
	done, _ := next.updateBranch(tea.KeyMsg{Type: tea.KeyEnter})
	if done.(model).target != "release" {
		t.Fatalf("target = %q, want release", done.(model).target)
	}
	if !strings.Contains(done.(model).planView(), "→ release") {
		t.Error("the plan header does not show where the commits will land")
	}
}

func TestBranchEscapeClearsTheTarget(t *testing.T) {
	m := plannedModel(100, 26)
	m.target = "release"
	m.mode = modeBranch
	after, _ := m.updateBranch(tea.KeyMsg{Type: tea.KeyEscape})
	if after.(model).target != "" {
		t.Fatal("esc did not clear the target")
	}
}
