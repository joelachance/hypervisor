package resolve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satoricorp/hv/conflict"
)

func problemsMention(problems []string, want string) bool {
	for _, problem := range problems {
		if strings.Contains(problem, want) {
			return true
		}
	}
	return false
}

// A claim is checked, not trusted: a line said to be kept has to be there.
func TestVerifyRejectsAKeptClaimMissingFromTheResolution(t *testing.T) {
	request := Request{Ours: []string{"alpha"}, Theirs: []string{"beta"}}
	resolution := Resolution{Resolved: []string{"beta"}, KeptOurs: []string{"alpha"}, KeptTheirs: []string{"beta"}}
	problems := Verify(request, resolution)
	if !problemsMention(problems, "claimed kept but not in the resolution: alpha") {
		t.Fatalf("problems = %v, want the false claim named", problems)
	}
}

// The resolution is read back too: a line from nowhere, or a line twice, is
// as much a defect as a line lost.
func TestVerifyRejectsInventedAndDuplicatedLines(t *testing.T) {
	request := Request{Ours: []string{"alpha"}, Theirs: []string{"beta"}}
	invented := Resolution{Resolved: []string{"alpha", "beta", "rm -rf /"}}
	if problems := Verify(request, invented); !problemsMention(problems, "rm -rf /") {
		t.Fatalf("problems = %v, want the invented line named", problems)
	}
	duplicated := Resolution{Resolved: []string{"alpha", "alpha", "beta"}}
	if problems := Verify(request, duplicated); len(problems) != 1 || !strings.Contains(problems[0], "duplicate") {
		t.Fatalf("problems = %v, want the duplicate named once", problems)
	}
}

// A new line is allowed when it is declared and explained.
func TestVerifyAcceptsADeclaredAddition(t *testing.T) {
	request := Request{Ours: []string{"use(a)"}, Theirs: []string{"use(b)"}}
	declared := Resolution{
		Resolved: []string{"import c", "use(a)", "use(b)"},
		Added:    []Addition{{Line: "import c", Why: "both calls need it"}},
	}
	if problems := Verify(request, declared); len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	unexplained := Resolution{
		Resolved: []string{"import c", "use(a)", "use(b)"},
		Added:    []Addition{{Line: "import c"}},
	}
	if problems := Verify(request, unexplained); !problemsMention(problems, "added without a reason") {
		t.Fatalf("problems = %v, want the missing reason named", problems)
	}
}

func TestVerifyRejectsADropWithoutAReason(t *testing.T) {
	request := Request{Ours: []string{"old"}, Theirs: []string{"new"}}
	resolution := Resolution{Resolved: []string{"new"}, Dropped: []string{"old"}}
	if problems := Verify(request, resolution); !problemsMention(problems, "dropped without a reason") {
		t.Fatalf("problems = %v", problems)
	}
	contradiction := Resolution{Resolved: []string{"old", "new"}, Dropped: []string{"old -- obsolete"}}
	if problems := Verify(request, contradiction); !problemsMention(problems, "dropped but still in the resolution") {
		t.Fatalf("problems = %v", problems)
	}
}

// A line both sides had is one line in the resolution, whichever list it
// was claimed under.
func TestVerifyAcceptsASharedLineClaimedOnce(t *testing.T) {
	request := Request{Ours: []string{"}", "a()"}, Theirs: []string{"}", "b()"}}
	resolution := Resolution{
		Resolved:   []string{"}", "a()", "b()"},
		KeptOurs:   []string{"}", "a()"},
		KeptTheirs: []string{"b()"},
	}
	if problems := Verify(request, resolution); len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
}

// A branch that already has commits the merge does not include is not
// overwritten: hv adds, it never discards.
func TestMergeRefusesABranchWithOtherCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := conflictingRepo(t)
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("checkout", "-q", "-b", "taken", "main")
	if err := os.WriteFile(filepath.Join(repo, "extra.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "someone's work")
	run("checkout", "-q", "main")
	before := gitOut(t, repo, "rev-parse", "taken")

	report, err := conflict.Analyze(context.Background(), repo,
		conflict.Side{Ref: "session-a"}, conflict.Side{Ref: "session-b"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{reply: func(request Request) Resolution {
		return Resolution{Resolved: request.Ours, Dropped: []string{request.Theirs[0] + " -- ours wins"}}
	}}
	_, err = Merge(context.Background(), repo, report, runner, "taken")
	if err == nil || !strings.Contains(err.Error(), "commits this merge does not include") {
		t.Fatalf("Merge() error = %v, want a refusal", err)
	}
	if gitOut(t, repo, "rev-parse", "taken") != before {
		t.Fatal("the branch was moved despite the refusal")
	}
}
