package apply

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/hv/journal"
	"github.com/satoricorp/hv/plan"
)

const deletePatch = `diff --git a/gone.txt b/gone.txt
deleted file mode 100644
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-gone
`

const modePatch = `diff --git a/run.sh b/run.sh
old mode 100644
new mode 100755
`

func bumpRevision() ([]demux.RevisionProposal, []demux.HunkRange) {
	return []demux.RevisionProposal{{ID: "u1", Intent: "bump", Files: []string{"version.go"}, HunkIDs: []string{"h1"}, TargetStack: "feature/bump"}},
		[]demux.HunkRange{{ID: "h1", File: "version.go", Patch: bumpPatch}}
}

// The remainder lands after the revisions, on the branch the last one went
// to, and its commit says what it did.
func TestRemainderIsCommittedAfterTheRevisions(t *testing.T) {
	repo := repoWith(t, map[string]string{"version.go": "package main // 1\n", "gone.txt": "gone\n", "run.sh": "#!/bin/sh\n"})
	if err := os.WriteFile(filepath.Join(repo, "version.go"), []byte("package main // 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remainder := plan.Remainder{
		Patch:   deletePatch + modePatch,
		Changes: []plan.Change{{Path: "gone.txt", Kind: "delete"}, {Path: "run.sh", Kind: "mode"}},
	}
	revisions, hunks := bumpRevision()
	entry, err := Apply(context.Background(), repo, packet(revisions, hunks), Options{
		Remainder: remainder,
		Sessions:  []SessionInfo{{ID: "s1", Title: "cleanup"}},
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(entry.Commits) != 2 || entry.Commits[1].Revision != "hv" || entry.Commits[1].Branch != "feature/bump" {
		t.Fatalf("commits = %+v, want the remainder second on feature/bump", entry.Commits)
	}
	if got := gitOut(t, repo, "ls-tree", "--name-only", "feature/bump"); strings.Contains(got, "gone.txt") {
		t.Fatalf("gone.txt survives on the branch: %s", got)
	}
	if got := gitOut(t, repo, "ls-tree", "feature/bump", "run.sh"); !strings.HasPrefix(got, "100755") {
		t.Fatalf("run.sh mode = %s, want 100755", got)
	}
	if got := gitOut(t, repo, "cat-file", "-p", "feature/bump~1:version.go"); got != "package main // 2" {
		t.Fatalf("revision commit comes first; version.go = %q", got)
	}
	message := gitOut(t, repo, "log", "-1", "--format=%B", "feature/bump")
	for _, want := range []string{"remove gone.txt, chmod run.sh", "gone.txt: deleted", "run.sh: mode 100644 -> 100755", "hv-session: s1"} {
		if !strings.Contains(message, want) {
			t.Errorf("message lacks %q:\n%s", want, message)
		}
	}
	if gitOut(t, repo, "status", "--porcelain") == "" {
		t.Fatal("the working tree should still be dirty; the commits are copies")
	}
}

// With no revisions at all, the remainder still has somewhere to go.
func TestRemainderAloneGetsABranch(t *testing.T) {
	repo := repoWith(t, map[string]string{"gone.txt": "gone\n"})
	entry, err := Apply(context.Background(), repo, packet(nil, nil), Options{
		Branch:    "cleanup",
		Remainder: plan.Remainder{Patch: deletePatch, Changes: []plan.Change{{Path: "gone.txt", Kind: "delete"}}},
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(entry.Commits) != 1 || entry.Commits[0].Branch != "cleanup" {
		t.Fatalf("commits = %+v", entry.Commits)
	}
	if got := gitOut(t, repo, "ls-tree", "--name-only", "cleanup"); got != "" {
		t.Fatalf("cleanup still has %q", got)
	}
}

// Moving a branch another worktree has checked out would leave that worktree
// with an index that disagrees with its HEAD. Refused, before any ref moves.
func TestApplyRefusesABranchCheckedOutElsewhere(t *testing.T) {
	repo := repoWith(t, map[string]string{"version.go": "package main // 1\n"})
	other := filepath.Join(t.TempDir(), "other")
	if out, err := gitCmd(repo, "worktree", "add", "-q", "-b", "feature/bump", other); err != nil {
		t.Fatalf("worktree add: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "version.go"), []byte("package main // 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	revisions, hunks := bumpRevision()
	_, err := Apply(context.Background(), repo, packet(revisions, hunks), Options{})
	if err == nil || !strings.Contains(err.Error(), "checked out in") {
		t.Fatalf("Apply() error = %v, want a refusal naming the other worktree", err)
	}
	if got := gitOut(t, repo, "rev-parse", "feature/bump"); got != gitOut(t, repo, "rev-parse", "main") {
		t.Fatal("feature/bump moved despite the refusal")
	}
	if got := gitOut(t, other, "status", "--porcelain"); got != "" {
		t.Fatalf("the other worktree now shows %q", got)
	}
	if _, ok, _ := journal.Last(context.Background(), repo); ok {
		t.Fatal("nothing moved, so nothing should be journalled")
	}
}

// When a later ref update fails, the ones that already moved are journalled
// so undo can take them back.
func TestPartialRefMoveIsJournalled(t *testing.T) {
	repo := repoWith(t, map[string]string{"version.go": "package main // 1\n", "added.go": ""})
	if err := os.WriteFile(filepath.Join(repo, "version.go"), []byte("package main // 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// feature/pkg cannot be created while feature/pkg/child exists: the name
	// is a directory in refs. Sorted after "aaa", so aaa moves first.
	if out, err := gitCmd(repo, "branch", "feature/pkg/child"); err != nil {
		t.Fatalf("branch: %v: %s", err, out)
	}
	revisions := []demux.RevisionProposal{
		{ID: "u1", Intent: "bump", Files: []string{"version.go"}, HunkIDs: []string{"h1"}, TargetStack: "aaa"},
		{ID: "u2", Intent: "add", Files: []string{"added.go"}, HunkIDs: []string{"h2"}, TargetStack: "feature/pkg"},
	}
	hunks := []demux.HunkRange{
		{ID: "h1", File: "version.go", Patch: bumpPatch},
		{ID: "h2", File: "added.go", Patch: strings.Replace(newFilePatch, "new file mode 100644\n--- /dev/null\n", "--- a/added.go\n", 1)},
	}
	_, err := Apply(context.Background(), repo, packet(revisions, hunks), Options{})
	if err == nil || !strings.Contains(err.Error(), "hv undo") {
		t.Fatalf("Apply() error = %v, want the partial move reported as undoable", err)
	}
	last, ok, jerr := journal.Last(context.Background(), repo)
	if jerr != nil || !ok {
		t.Fatalf("journal.Last() = %v, %v, %v", last, ok, jerr)
	}
	if len(last.Branches) != 1 || last.Branches[0].Name != "aaa" {
		t.Fatalf("journalled branches = %+v, want aaa alone", last.Branches)
	}
	if _, err := journal.Undo(context.Background(), last); err != nil {
		t.Fatalf("Undo() error = %v", err)
	}
	if gitOut(t, repo, "rev-parse", "--verify", "--quiet", "refs/heads/aaa") != "" {
		t.Fatal("aaa survives undo")
	}
}

func TestChangeSummaryTagsTheRightFile(t *testing.T) {
	got := changeSummary(demux.RevisionProposal{}, bumpPatch+newFilePatch)
	if len(got) != 2 {
		t.Fatalf("changeSummary() = %v", got)
	}
	if strings.Contains(got[0], "new file") || !strings.Contains(got[1], "added.go: +1/-0 (new file)") {
		t.Fatalf("changeSummary() = %v; the new-file tag belongs to added.go", got)
	}
}

func gitCmd(repo string, args ...string) ([]byte, error) {
	return exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
}

// A conflicted script that a resolver settles keeps its executable bit.
func TestResolvedFileKeepsItsMode(t *testing.T) {
	repo := repoWith(t, map[string]string{"run.sh": "#!/bin/sh\necho 1\n"})
	run := func(args ...string) {
		t.Helper()
		if out, err := gitCmd(repo, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("update-index", "--chmod=+x", "run.sh")
	run("commit", "-qm", "executable")
	run("checkout", "-q", "-b", "feature/x")
	// WriteFile does not change an existing file's mode; the bit has to be
	// set for real or commit -a would record the branch at 100644.
	if err := os.Chmod(filepath.Join(repo, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "run.sh"), []byte("#!/bin/sh\necho branch\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run("commit", "-qam", "branch work")
	run("checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(repo, "run.sh"), []byte("#!/bin/sh\necho 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	realPatch := gitOut(t, repo, "diff", "--no-color")
	plan := packet(
		[]demux.RevisionProposal{{ID: "u1", Intent: "bump", HunkIDs: []string{"h1"}, TargetStack: "feature/x"}},
		[]demux.HunkRange{{ID: "h1", File: "run.sh", Patch: realPatch}})
	resolver := &fakeResolver{reply: func(ConflictRequest) []string { return []string{"echo branch and 2"} }}
	if _, err := Apply(context.Background(), repo, plan, Options{Resolver: resolver}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := gitOut(t, repo, "ls-tree", "feature/x", "run.sh"); !strings.HasPrefix(got, "100755") {
		t.Fatalf("run.sh = %s, want mode 100755 kept", got)
	}
}
