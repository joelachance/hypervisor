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

// fakeRunner stands in for fx so the pipeline is testable without a model or
// an account.
type fakeRunner struct {
	reply func(Request) Resolution
	seen  []Request
}

func (f *fakeRunner) Name() string { return "fake" }
func (f *fakeRunner) Resolve(_ context.Context, request Request) (Resolution, error) {
	f.seen = append(f.seen, request)
	return f.reply(request), nil
}

func TestParseResolutionTakesFencedJSON(t *testing.T) {
	got, err := ParseResolution("Here you go:\n```json\n{\"resolved\":[\"a\"],\"reasoning\":\"r\"}\n```\n")
	if err != nil {
		t.Fatalf("ParseResolution() error = %v", err)
	}
	if len(got.Resolved) != 1 || got.Resolved[0] != "a" {
		t.Fatalf("resolved = %v", got.Resolved)
	}
}

func TestParseResolutionRejectsAnEmptyAnswer(t *testing.T) {
	if _, err := ParseResolution(`{"resolved":[],"reasoning":"none"}`); err == nil {
		t.Fatal("an empty resolution was accepted")
	}
}

// The guard that makes an agent's answer safe to commit.
func TestVerifyCatchesASilentlyDroppedLine(t *testing.T) {
	request := Request{
		OursFrom: "A", TheirsFrom: "B",
		Ours:   []string{"keep me", "and me"},
		Theirs: []string{"mine too"},
	}
	resolution := Resolution{Resolved: []string{"keep me", "mine too"}}
	problems := Verify(request, resolution)
	if len(problems) != 1 || !strings.Contains(problems[0], "and me") {
		t.Fatalf("problems = %v, want the unexplained line named", problems)
	}
}

// Combining two lines into one is an honest resolution, and without a way to
// say so it would look like two lines going missing.
func TestVerifyAcceptsARewrittenLine(t *testing.T) {
	request := Request{
		Ours:   []string{"// caps requests, with a burst"},
		Theirs: []string{"// caps requests, backing off"},
	}
	resolution := Resolution{
		Resolved: []string{"// caps requests, with a burst, backing off"},
		Rewritten: []Rewrite{
			{From: "// caps requests, with a burst", To: "// caps requests, with a burst, backing off", Why: "combined"},
			{From: "// caps requests, backing off", To: "// caps requests, with a burst, backing off", Why: "combined"},
		},
	}
	if problems := Verify(request, resolution); len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
}

func TestVerifyAcceptsADroppedLineWithAReason(t *testing.T) {
	request := Request{Ours: []string{"old call"}, Theirs: []string{"new call"}}
	resolution := Resolution{
		Resolved: []string{"new call"},
		Dropped:  []string{"old call -- replaced by the new call"},
	}
	if problems := Verify(request, resolution); len(problems) != 0 {
		t.Fatalf("problems = %v, want none - the line was accounted for", problems)
	}
}

func TestPromptCarriesBothIntentsAndBothVersions(t *testing.T) {
	prompt := Prompt(Request{
		Path: "app.go", OursFrom: "rate limiter", TheirsFrom: "retry bug",
		OursIntent: "add a limiter", TheirsIntent: "fix retries",
		Base: []string{"old"}, Ours: []string{"limiter"}, Theirs: []string{"retry"},
	})
	for _, want := range []string{"app.go", "rate limiter", "retry bug", "add a limiter", "fix retries", "old", "limiter", "retry", "\"resolved\""} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

func TestReplaceRegionsLeavesTheRestByteForByte(t *testing.T) {
	merged := "top\n<<<<<<< ours\nA\n||||||| base\nO\n=======\nB\n>>>>>>> theirs\nbottom\n"
	got, err := replaceRegions(merged, [][]string{{"AB"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "top\nAB\nbottom\n" {
		t.Fatalf("got %q", got)
	}
}

// Replacing blocks one at a time renumbers the ones that follow, so a file
// with two conflicts would look for region 2 in a file that no longer has one.
func TestReplaceRegionsHandlesSeveralBlocks(t *testing.T) {
	merged := strings.Join([]string{
		"top",
		"<<<<<<< ours", "A1", "||||||| base", "O1", "=======", "B1", ">>>>>>> theirs",
		"middle",
		"<<<<<<< ours", "A2", "||||||| base", "O2", "=======", "B2", ">>>>>>> theirs",
		"bottom", "",
	}, "\n")
	got, err := replaceRegions(merged, [][]string{{"first"}, {"second"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "top\nfirst\nmiddle\nsecond\nbottom\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReplaceRegionsRejectsAMiscount(t *testing.T) {
	merged := "a\n<<<<<<< ours\nA\n=======\nB\n>>>>>>> theirs\nb\n"
	if _, err := replaceRegions(merged, [][]string{{"one"}, {"two"}}); err == nil {
		t.Fatal("a resolution count that does not match the file was accepted")
	}
}

// End to end: two branches that conflict, a resolution, a real merge commit.
func TestMergeCommitsWithTwoParents(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := conflictingRepo(t)
	report, err := conflict.Analyze(context.Background(), repo,
		conflict.Side{Ref: "session-a", Title: "rate limiter"},
		conflict.Side{Ref: "session-b", Title: "retry bug"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Clean {
		t.Fatal("the fixture did not conflict")
	}

	runner := &fakeRunner{reply: func(request Request) Resolution {
		return Resolution{
			Resolved:   []string{"func One() int { return 111 + 222 }"},
			KeptOurs:   request.Ours,
			KeptTheirs: request.Theirs,
			Reasoning:  "both changes are additive, so combine them",
		}
	}}

	outcome, err := Merge(context.Background(), repo, report, runner, "hv/merge-test")
	if err != nil {
		t.Fatalf("Merge() error = %v (problems %v)", err, outcome.Problems)
	}
	if outcome.Commit == "" {
		t.Fatal("no commit was produced")
	}
	parents := gitOut(t, repo, "rev-list", "--parents", "-n", "1", outcome.Commit)
	if len(strings.Fields(parents)) != 3 {
		t.Fatalf("rev-list = %q, want a commit with two parents", parents)
	}
	body := gitOut(t, repo, "show", "hv/merge-test:app.go")
	if strings.Contains(body, "<<<<<<<") {
		t.Fatal("the committed file still has conflict markers")
	}
	if !strings.Contains(body, "111 + 222") {
		t.Fatalf("the resolution is not in the commit: %q", body)
	}
	if subject := gitOut(t, repo, "log", "-1", "--format=%s", "hv/merge-test"); !strings.Contains(subject, "rate limiter") {
		t.Errorf("subject = %q, want both sessions named", subject)
	}
	if outcome.EntryID == "" {
		t.Error("the merge was not journalled, so hv undo cannot take it back")
	}
	// The prompt must have carried the session that produced each side.
	if len(runner.seen) != 1 || runner.seen[0].OursFrom != "rate limiter" {
		t.Errorf("request = %+v, want the sessions named", runner.seen)
	}
}

// A resolution that loses a line must never reach a commit.
func TestMergeRefusesAnUnverifiableResolution(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := conflictingRepo(t)
	report, err := conflict.Analyze(context.Background(), repo,
		conflict.Side{Ref: "session-a"}, conflict.Side{Ref: "session-b"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{reply: func(Request) Resolution {
		return Resolution{Resolved: []string{"func One() int { return 0 }"}}
	}}
	if _, err := Merge(context.Background(), repo, report, runner, "hv/merge-bad"); err == nil {
		t.Fatal("a resolution that dropped both sides was committed")
	}
	if gitOut(t, repo, "rev-parse", "--verify", "--quiet", "refs/heads/hv/merge-bad") != "" {
		t.Fatal("a branch was created for a rejected merge")
	}
}

func conflictingRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "hv@test")
	run("config", "user.name", "hv")
	write("package app\n\nfunc One() int { return 1 }\n")
	run("add", "-A")
	run("commit", "-qm", "seed")
	run("checkout", "-q", "-b", "session-a")
	write("package app\n\nfunc One() int { return 111 }\n")
	run("add", "-A")
	run("commit", "-qm", "a")
	run("checkout", "-q", "main")
	run("checkout", "-q", "-b", "session-b")
	write("package app\n\nfunc One() int { return 222 }\n")
	run("add", "-A")
	run("commit", "-qm", "b")
	run("checkout", "-q", "main")
	return repo
}

func gitOut(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
