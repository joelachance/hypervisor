package gitx

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
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "hv@test"},
		{"config", "user.name", "hv"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "seed"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// Several git commands report a finding through the exit code rather than
// failing. Discarding their output on a non-zero status throws away the answer.
func TestOutputKeepsStdoutOnANonZeroExit(t *testing.T) {
	dir := repo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// --exit-code makes diff exit 1 when there are differences.
	got := Output(context.Background(), dir, "diff", "--exit-code")
	if !strings.Contains(got, "-one") || !strings.Contains(got, "+two") {
		t.Fatalf("Output() = %q, want the diff despite the exit status", got)
	}
}

func TestRunReportsGitsComplaint(t *testing.T) {
	dir := repo(t)
	_, err := Run(context.Background(), dir, "rev-parse", "--verify", "nope")
	if err == nil {
		t.Fatal("a bad revision was accepted")
	}
	if !strings.Contains(err.Error(), "rev-parse") {
		t.Errorf("error = %v, want it to name the command", err)
	}
}

func TestScratchIndexLeavesTheRealOneAlone(t *testing.T) {
	dir := repo(t)
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ScratchIndex(func(env []string) error {
		if _, err := RunIn(context.Background(), dir, env, "read-tree", "HEAD"); err != nil {
			return err
		}
		_, err := RunIn(context.Background(), dir, env, "add", "b.txt")
		return err
	})
	if err != nil {
		t.Fatalf("ScratchIndex() = %v", err)
	}
	status, _ := Run(context.Background(), dir, "status", "--porcelain")
	if !strings.Contains(status, "?? b.txt") {
		t.Fatalf("status = %q; the file should still be untracked in the real index", status)
	}
}

func TestReplaceRegionsHandlesSeveralBlocks(t *testing.T) {
	merged := strings.Join([]string{
		"top",
		"<<<<<<< ours", "A1", "||||||| base", "O1", "=======", "B1", ">>>>>>> theirs",
		"middle",
		"<<<<<<< ours", "A2", "||||||| base", "O2", "=======", "B2", ">>>>>>> theirs",
		"bottom", "",
	}, "\n")
	regions := ParseRegions(merged)
	if len(regions) != 2 || regions[0].Ours[0] != "A1" || regions[1].Theirs[0] != "B2" {
		t.Fatalf("regions = %+v", regions)
	}
	got, err := ReplaceRegions(merged, [][]string{{"first"}, {"second"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "top\nfirst\nmiddle\nsecond\nbottom\n" {
		t.Fatalf("got %q", got)
	}
}

func TestReplaceRegionsRejectsAMiscount(t *testing.T) {
	merged := "a\n<<<<<<< ours\nA\n=======\nB\n>>>>>>> theirs\nb\n"
	if _, err := ReplaceRegions(merged, [][]string{{"one"}, {"two"}}); err == nil {
		t.Fatal("a resolution count that does not match the file was accepted")
	}
}
