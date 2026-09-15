package plan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const mixedDiff = `diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 1234567..0000000
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-gone
diff --git a/kept.go b/kept.go
index 1234567..89abcde 100644
--- a/kept.go
+++ b/kept.go
@@ -1 +1 @@
-package a
+package b
diff --git a/run.sh b/run.sh
old mode 100644
new mode 100755
diff --git a/pic.png b/pic.png
index 1234567..89abcde 100644
GIT binary patch
literal 4
LcmZQzWMT#Y01f~L

diff --git a/with space.txt b/with space.txt
old mode 100644
new mode 100755
diff --git a/empty.txt b/empty.txt
new file mode 100644
index 0000000..e69de29
`

// The planned half is exactly the sections demux's parser can hold, and the
// remainder is everything else, classified.
func TestSplitKeepsWhatDemuxCannotParse(t *testing.T) {
	planned, remainder := Split(mixedDiff)
	if !strings.HasPrefix(planned, "diff --git a/kept.go b/kept.go\n") || strings.Count(planned, "diff --git") != 1 {
		t.Fatalf("planned = %q, want only kept.go", planned)
	}
	want := map[string]string{
		"gone.txt": "delete", "run.sh": "mode", "pic.png": "binary", "with space.txt": "mode", "empty.txt": "add",
	}
	if len(remainder.Changes) != len(want) {
		t.Fatalf("remainder = %+v, want %d changes", remainder.Changes, len(want))
	}
	for _, change := range remainder.Changes {
		if want[change.Path] != change.Kind {
			t.Errorf("%s classified %q, want %q", change.Path, change.Kind, want[change.Path])
		}
	}
	if !strings.Contains(remainder.Patch, "GIT binary patch") || strings.Contains(remainder.Patch, "kept.go") {
		t.Fatalf("remainder patch = %q", remainder.Patch)
	}
	if got := remainder.Summary(); !strings.HasPrefix(got, "remove gone.txt, chmod run.sh") {
		t.Fatalf("Summary() = %q", got)
	}
}

func TestUncoveredNamesTheFilesAPlanLost(t *testing.T) {
	_, remainder := Split(mixedDiff)
	if missing := Uncovered(mixedDiff, []string{"kept.go"}, remainder); len(missing) != 0 {
		t.Fatalf("Uncovered() = %v, want nothing", missing)
	}
	if missing := Uncovered(mixedDiff, nil, remainder); len(missing) != 1 || missing[0] != "kept.go" {
		t.Fatalf("Uncovered() = %v, want kept.go", missing)
	}
}

func TestGitLinePathHandlesSpaces(t *testing.T) {
	if got := GitLinePath("diff --git a/with space.txt b/with space.txt\n"); got != "with space.txt" {
		t.Fatalf("GitLinePath() = %q", got)
	}
	if got := GitLinePath("diff --git a/x b/x\n"); got != "x" {
		t.Fatalf("GitLinePath() = %q", got)
	}
}

// A deleted file used to take every file after it out of the plan: demux's
// parser never recovers from `+++ /dev/null`. Now the deletion is hv's and
// the rest is planned.
func TestRunPlansEverythingAfterADeletion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := newRepo(t)
	write(t, repo, "aaa.txt", "first\n")
	write(t, repo, "zzz.go", "package z\n")
	commitAll(t, repo)
	if err := os.Remove(filepath.Join(repo, "aaa.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "zzz.go", "package z // changed\n")
	write(t, repo, "added.go", "package added\n")

	result, err := Run(context.Background(), Request{RepoRoot: repo})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	files := hunkFiles(result.Packet.Proposal.Hunks)
	if len(files) != 2 {
		t.Fatalf("planned files = %v, want zzz.go and added.go", files)
	}
	if got := result.Remainder.Files(); len(got) != 1 || got[0] != "aaa.txt" {
		t.Fatalf("remainder = %v, want the deletion", got)
	}
	if result.Remainder.Changes[0].Kind != "delete" {
		t.Fatalf("kind = %q", result.Remainder.Changes[0].Kind)
	}
}

// A commit is built from HEAD, so the plan has to be too: an edit sitting in
// the index used to drop out of the commit without a word.
func TestRunIncludesStagedChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := newRepo(t)
	write(t, repo, "billing.go", "package billing\n\nfunc Total() int { return 2 }\n")
	git(t, repo, "add", "billing.go")
	write(t, repo, "billing.go", "package billing\n\nfunc Total() int { return 2 }\n\nfunc Tax() int { return 1 }\n")

	result, err := Run(context.Background(), Request{RepoRoot: repo})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(result.Diff, "-func Total() int { return 1 }") || !strings.Contains(result.Diff, "+func Tax()") {
		t.Fatalf("diff lost the staged or the unstaged half:\n%s", result.Diff)
	}
}

// A tree with nothing but deletions has no hunks for demux; the remainder is
// the whole plan rather than an error.
func TestRunWithOnlyDeletionsSkipsDemux(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := newRepo(t)
	if err := os.Remove(filepath.Join(repo, "billing.go")); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Request{RepoRoot: repo})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Packet.Proposal.Revisions) != 0 || result.Remainder.Empty() {
		t.Fatalf("result = %+v, want a remainder-only plan", result)
	}
}

// diff.noprefix used to leave `git apply` unable to name a file; the diff
// hv plans over is pinned to a/ and b/ whatever the config says.
func TestWorkingDiffIgnoresPrefixConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := newRepo(t)
	git(t, repo, "config", "diff.noprefix", "true")
	write(t, repo, "billing.go", "package billing\n")
	diff, err := WorkingDiff(context.Background(), repo, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "--- a/billing.go\n+++ b/billing.go\n") {
		t.Fatalf("diff = %q, want a/ and b/ prefixes", diff)
	}
}

func commitAll(t *testing.T, repo string) {
	t.Helper()
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "more")
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
