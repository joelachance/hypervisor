package conflict

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "hv@test")
	run(t, dir, "config", "user.name", "hv")
	write(t, dir, "app.go", "package app\n\nfunc One() int { return 1 }\n\nfunc Two() int { return 2 }\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-qm", "seed")
	return dir
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// branchWith commits body on a new branch cut from main, the way hv's own
// merge would have left one session's work.
func branchWith(t *testing.T, dir, branch, file, body string) {
	t.Helper()
	run(t, dir, "checkout", "-q", "-b", branch, "main")
	write(t, dir, file, body)
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-qm", branch)
	run(t, dir, "checkout", "-q", "main")
}

func TestCleanMergeNeedsNoDecisions(t *testing.T) {
	dir := repo(t)
	branchWith(t, dir, "session-a", "app.go", "package app\n\nfunc One() int { return 100 }\n\nfunc Two() int { return 2 }\n")
	branchWith(t, dir, "session-b", "other.go", "package app\n\nfunc Three() int { return 3 }\n")

	report, err := Analyze(context.Background(), dir,
		Side{Ref: "session-a", Title: "rate limiter"}, Side{Ref: "session-b", Title: "retry bug"})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if !report.Clean {
		t.Fatalf("report is not clean: %+v", report.Conflicts)
	}
	if report.MergedTree == "" {
		t.Error("a clean merge should carry the tree it produced, ready to commit")
	}
	if len(report.Steps) != 1 || report.Steps[0].Action != ActionAuto {
		t.Errorf("steps = %+v, want a single auto-merged step", report.Steps)
	}
	if !report.Accounting.Complete {
		t.Error("accounting should be complete for a clean merge")
	}
}

func TestOverlappingEditsBecomeAChoice(t *testing.T) {
	dir := repo(t)
	branchWith(t, dir, "session-a", "app.go", "package app\n\nfunc One() int { return 111 }\n\nfunc Two() int { return 2 }\n")
	branchWith(t, dir, "session-b", "app.go", "package app\n\nfunc One() int { return 222 }\n\nfunc Two() int { return 2 }\n")

	report, err := Analyze(context.Background(), dir,
		Side{Ref: "session-a", Title: "rate limiter"}, Side{Ref: "session-b", Title: "retry bug"})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if report.Clean {
		t.Fatal("two rewrites of the same line reported as clean")
	}
	if len(report.Conflicts) != 1 || report.Conflicts[0].Path != "app.go" {
		t.Fatalf("conflicts = %+v, want app.go", report.Conflicts)
	}
	regions := report.Conflicts[0].Regions
	if len(regions) != 1 {
		t.Fatalf("regions = %+v, want one", regions)
	}
	// Both versions must survive in full: a step that summarized them away
	// would be a step someone cannot act on.
	if !strings.Contains(strings.Join(regions[0].Ours, "\n"), "111") {
		t.Errorf("our version is missing from the region: %v", regions[0].Ours)
	}
	if !strings.Contains(strings.Join(regions[0].Theirs, "\n"), "222") {
		t.Errorf("their version is missing from the region: %v", regions[0].Theirs)
	}
	if !strings.Contains(strings.Join(regions[0].Base, "\n"), "return 1") {
		t.Errorf("the base version is missing from the region: %v", regions[0].Base)
	}

	step := lastChoose(t, report)
	if step.Path != "app.go" || step.Region != 1 {
		t.Errorf("step = %+v, want it to name the file and region", step)
	}
	if !strings.Contains(step.Reason, "rate limiter") || !strings.Contains(step.Reason, "retry bug") {
		t.Errorf("reason = %q, want both sessions named - that is what git cannot say", step.Reason)
	}
	if !report.Accounting.Complete || len(report.Accounting.Unaccounted) != 0 {
		t.Errorf("accounting = %+v, want every conflicted line covered", report.Accounting)
	}
	if report.Accounting.StepsCoverLines != report.Accounting.ConflictedLines {
		t.Errorf("steps cover %d of %d conflicted lines",
			report.Accounting.StepsCoverLines, report.Accounting.ConflictedLines)
	}
}

// Two sessions adding different code at the same spot do not replace each
// other, and saying "choose" there would throw one session's work away.
func TestAdjacentAdditionsAskToKeepBoth(t *testing.T) {
	dir := repo(t)
	branchWith(t, dir, "session-a", "app.go",
		"package app\n\nfunc One() int { return 1 }\n\nfunc AAA() int { return 0 }\n\nfunc Two() int { return 2 }\n")
	branchWith(t, dir, "session-b", "app.go",
		"package app\n\nfunc One() int { return 1 }\n\nfunc BBB() int { return 0 }\n\nfunc Two() int { return 2 }\n")

	report, err := Analyze(context.Background(), dir,
		Side{Ref: "session-a", Title: "a"}, Side{Ref: "session-b", Title: "b"})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if report.Clean {
		t.Skip("git merged these without a conflict; nothing to assert")
	}
	found := false
	for _, step := range report.Steps {
		if step.Action == ActionBoth {
			found = true
		}
	}
	if !found {
		t.Errorf("steps = %+v, want a keep-both step for pure additions", report.Steps)
	}
}

func TestAnalyzeRefusesUnrelatedHistories(t *testing.T) {
	dir := repo(t)
	if _, err := Analyze(context.Background(), dir, Side{Ref: "main"}, Side{Ref: "nope"}); err == nil {
		t.Fatal("Analyze() accepted a ref that does not exist")
	}
}

func lastChoose(t *testing.T, report Report) Step {
	t.Helper()
	for _, step := range report.Steps {
		if step.Action == ActionChoose {
			return step
		}
	}
	t.Fatalf("no choose step in %+v", report.Steps)
	return Step{}
}

// A binary file both sides changed cannot be shown line by line. The report
// says so: a whole-file step, and an accounting that admits it is not
// complete rather than one that always says it is.
func TestBinaryConflictIsAWholeFileStep(t *testing.T) {
	dir := repo(t)
	write(t, dir, "logo.bin", "base\x00bytes")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-qm", "binary")
	branchWith(t, dir, "session-a", "logo.bin", "ours\x00bytes")
	branchWith(t, dir, "session-b", "logo.bin", "theirs\x00bytes")

	report, err := Analyze(context.Background(), dir, Side{Ref: "session-a"}, Side{Ref: "session-b"})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if len(report.Conflicts) != 1 || !report.Conflicts[0].Binary {
		t.Fatalf("conflicts = %+v, want logo.bin marked binary", report.Conflicts)
	}
	step := lastChoose(t, report)
	if step.Path != "logo.bin" || !strings.Contains(step.Reason, "taken whole") {
		t.Fatalf("step = %+v, want a whole-file decision", step)
	}
	if report.Accounting.Complete || len(report.Accounting.Unaccounted) != 1 {
		t.Fatalf("accounting = %+v, want the file admitted as unaccounted", report.Accounting)
	}
}

// The conflicted-line figure is read from the marked-up text, and the steps
// are checked against it - so it can disagree.
func TestAccountingCountsFromTheText(t *testing.T) {
	dir := repo(t)
	branchWith(t, dir, "session-a", "app.go", "package app\n\nfunc One() int { return 100 }\n\nfunc Two() int { return 2 }\n")
	branchWith(t, dir, "session-b", "app.go", "package app\n\nfunc One() int { return 200 }\n\nfunc Two() int { return 2 }\n")
	report, err := Analyze(context.Background(), dir, Side{Ref: "session-a"}, Side{Ref: "session-b"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Accounting.ConflictedLines != 2 || report.Accounting.StepsCoverLines != 2 || !report.Accounting.Complete {
		t.Fatalf("accounting = %+v, want 2 lines counted and covered", report.Accounting)
	}
	// Steps that claim less than the text holds are caught.
	report.Conflicts[0].Regions = nil
	report.Conflicts[0].Problem = "regions lost"
	_, accounting := steps(report)
	if accounting.Complete {
		t.Fatalf("accounting = %+v, want incomplete once the regions are gone", accounting)
	}
}
