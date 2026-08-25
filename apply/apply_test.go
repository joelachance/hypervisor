package apply

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satoricorp/demux"
)

func repoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "hv@test")
	run("config", "user.name", "hv")
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-qm", "seed")
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

func packet(revisions []demux.RevisionProposal, hunks []demux.HunkRange) demux.DemuxPlanPacket {
	return demux.DemuxPlanPacket{Proposal: demux.DemuxProposal{Revisions: revisions, Hunks: hunks}}
}

const bumpPatch = `diff --git a/version.go b/version.go
--- a/version.go
+++ b/version.go
@@ -1 +1 @@
-package main // 1
+package main // 2
`

const newFilePatch = `diff --git a/added.go b/added.go
new file mode 100644
--- /dev/null
+++ b/added.go
@@ -0,0 +1 @@
+package main
`

func TestApplyCommitsWithoutTouchingTheWorkingTree(t *testing.T) {
	repo := repoWith(t, map[string]string{"version.go": "package main // 1\n"})
	// The working tree holds the change, as it does in real use: hv commits
	// out of a dirty tree and leaves it dirty.
	if err := os.WriteFile(filepath.Join(repo, "version.go"), []byte("package main // 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := gitOut(t, repo, "status", "--porcelain")

	entry, err := Apply(context.Background(), repo, packet(
		[]demux.RevisionProposal{{ID: "u1", Intent: "bump the version", Files: []string{"version.go"},
			HunkIDs: []string{"h1"}, TargetStack: "feature/version"}},
		[]demux.HunkRange{{ID: "h1", File: "version.go", Patch: bumpPatch}}), Options{})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(entry.Commits) != 1 || entry.Commits[0].Branch != "feature/version" {
		t.Fatalf("commits = %+v", entry.Commits)
	}
	if subject := gitOut(t, repo, "log", "-1", "--format=%s", "feature/version"); subject != "bump the version" {
		t.Errorf("subject = %q, want the revision's intent", subject)
	}
	if committed := gitOut(t, repo, "show", "feature/version:version.go"); !strings.Contains(committed, "// 2") {
		t.Errorf("the commit does not contain the change: %q", committed)
	}
	if after := gitOut(t, repo, "status", "--porcelain"); after != before {
		t.Errorf("working tree changed: %q -> %q", before, after)
	}
	if head := gitOut(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); head != "main" {
		t.Errorf("checked-out branch = %q, want main - apply must not check anything out", head)
	}
}

// Revisions routed to one branch become successive commits on it, in order.
func TestApplyStacksRevisionsOnOneBranch(t *testing.T) {
	repo := repoWith(t, map[string]string{"version.go": "package main // 1\n"})
	entry, err := Apply(context.Background(), repo, packet(
		[]demux.RevisionProposal{
			{ID: "u1", Intent: "bump", HunkIDs: []string{"h1"}, TargetStack: "feature/x"},
			{ID: "u2", Intent: "add a file", HunkIDs: []string{"h2"}, TargetStack: "feature/x"},
		},
		[]demux.HunkRange{
			{ID: "h1", File: "version.go", Patch: bumpPatch},
			{ID: "h2", File: "added.go", Patch: newFilePatch},
		}), Options{})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(entry.Commits) != 2 || len(entry.Branches) != 1 {
		t.Fatalf("entry = %+v, want two commits on one branch", entry)
	}
	log := gitOut(t, repo, "log", "--format=%s", "feature/x")
	if !strings.HasPrefix(log, "add a file\nbump") {
		t.Errorf("log = %q, want the revisions in plan order", log)
	}
}

func TestUndoDeletesBranchesItCreated(t *testing.T) {
	repo := repoWith(t, map[string]string{"version.go": "package main // 1\n"})
	entry, err := Apply(context.Background(), repo, packet(
		[]demux.RevisionProposal{{ID: "u1", Intent: "bump", HunkIDs: []string{"h1"}, TargetStack: "feature/x"}},
		[]demux.HunkRange{{ID: "h1", File: "version.go", Patch: bumpPatch}}), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if gitOut(t, repo, "rev-parse", "--verify", "--quiet", "refs/heads/feature/x") == "" {
		t.Fatal("branch was not created")
	}
	undone, err := Undo(context.Background(), entry)
	if err != nil {
		t.Fatalf("Undo() error = %v", err)
	}
	if !undone.Undone() {
		t.Error("the entry was not marked undone")
	}
	if gitOut(t, repo, "rev-parse", "--verify", "--quiet", "refs/heads/feature/x") != "" {
		t.Fatal("branch survived the undo")
	}
	if _, err := Undo(context.Background(), undone); err == nil {
		t.Error("undoing twice must fail rather than delete something else")
	}
}

// An existing branch is extended, and undo resets it rather than deleting it -
// hv adds commits, it does not discard anyone else's.
func TestUndoResetsBranchesItExtended(t *testing.T) {
	repo := repoWith(t, map[string]string{"version.go": "package main // 1\n"})
	out, err := exec.Command("git", "-C", repo, "branch", "feature/x").CombinedOutput()
	if err != nil {
		t.Fatalf("git branch: %v: %s", err, out)
	}
	original := gitOut(t, repo, "rev-parse", "feature/x")

	entry, err := Apply(context.Background(), repo, packet(
		[]demux.RevisionProposal{{ID: "u1", Intent: "bump", HunkIDs: []string{"h1"}, TargetStack: "feature/x"}},
		[]demux.HunkRange{{ID: "h1", File: "version.go", Patch: bumpPatch}}), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Branches[0].Created() {
		t.Fatal("an existing branch was recorded as created")
	}
	if _, err := Undo(context.Background(), entry); err != nil {
		t.Fatalf("Undo() error = %v", err)
	}
	if now := gitOut(t, repo, "rev-parse", "feature/x"); now != original {
		t.Fatalf("branch is at %s, want its original %s", now, original)
	}
}

// If someone else moved the branch after hv wrote it, undo would throw their
// work away. It refuses instead.
func TestUndoRefusesAMovedBranch(t *testing.T) {
	repo := repoWith(t, map[string]string{"version.go": "package main // 1\n"})
	entry, err := Apply(context.Background(), repo, packet(
		[]demux.RevisionProposal{{ID: "u1", Intent: "bump", HunkIDs: []string{"h1"}, TargetStack: "feature/x"}},
		[]demux.HunkRange{{ID: "h1", File: "version.go", Patch: bumpPatch}}), Options{})
	if err != nil {
		t.Fatal(err)
	}
	main := gitOut(t, repo, "rev-parse", "main")
	if out, err := exec.Command("git", "-C", repo, "update-ref", "refs/heads/feature/x", main).CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v: %s", err, out)
	}
	if _, err := Undo(context.Background(), entry); err == nil {
		t.Fatal("undo moved a branch that had changed since it was applied")
	}
}

func TestHistoryRecordsAppliedAndUndoneState(t *testing.T) {
	repo := repoWith(t, map[string]string{"version.go": "package main // 1\n"})
	entry, err := Apply(context.Background(), repo, packet(
		[]demux.RevisionProposal{{ID: "u1", Intent: "bump", HunkIDs: []string{"h1"}, TargetStack: "feature/x"}},
		[]demux.HunkRange{{ID: "h1", File: "version.go", Patch: bumpPatch}}), Options{})
	if err != nil {
		t.Fatal(err)
	}
	last, ok, err := Last(context.Background(), repo)
	if err != nil || !ok || last.ID != entry.ID {
		t.Fatalf("Last() = %v, %v, %v; want the entry just applied", last.ID, ok, err)
	}
	if _, err := Undo(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Last(context.Background(), repo); ok {
		t.Error("an undone entry is still offered as the thing to undo")
	}
	history, err := History(context.Background(), repo)
	if err != nil || len(history) != 1 || !history[0].Undone() {
		t.Fatalf("history = %+v, %v; want one entry marked undone", history, err)
	}
}

// Session provenance survives in the commit, not only in hv's journal.
func TestCommitMessageCarriesSessionTrailers(t *testing.T) {
	repo := repoWith(t, map[string]string{"version.go": "package main // 1\n"})
	_, err := Apply(context.Background(), repo, packet(
		[]demux.RevisionProposal{{ID: "u1", Intent: "bump", HunkIDs: []string{"h1"},
			TargetStack: "feature/x", SessionIDs: []string{"s1"}}},
		[]demux.HunkRange{{ID: "h1", File: "version.go", Patch: bumpPatch}}),
		Options{Trailers: map[string]string{"s1": "rate limiter"}})
	if err != nil {
		t.Fatal(err)
	}
	body := gitOut(t, repo, "log", "-1", "--format=%B", "feature/x")
	if !strings.Contains(body, "hv-session: s1 (rate limiter)") {
		t.Fatalf("commit message = %q, want a session trailer", body)
	}
}

// Worktrees share one object database and one set of refs, so a merge run in
// a worktree creates branches every checkout can see. The journal has to live
// in the common git directory to match, or those branches are undoable only
// from the worktree that made them - which is not where anyone would look.
func TestJournalIsSharedAcrossWorktrees(t *testing.T) {
	repo := repoWith(t, map[string]string{"version.go": "package main // 1\n"})
	worktree := filepath.Join(t.TempDir(), "wt")
	out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", worktree, "-b", "wt").CombinedOutput()
	if err != nil {
		t.Skipf("git worktree unavailable: %v: %s", err, out)
	}

	entry, err := Apply(context.Background(), worktree, packet(
		[]demux.RevisionProposal{{ID: "u1", Intent: "bump", HunkIDs: []string{"h1"}, TargetStack: "feature/x"}},
		[]demux.HunkRange{{ID: "h1", File: "version.go", Patch: bumpPatch}}), Options{})
	if err != nil {
		t.Fatalf("Apply() from a worktree = %v", err)
	}
	if gitOut(t, repo, "rev-parse", "--verify", "--quiet", "refs/heads/feature/x") == "" {
		t.Fatal("the branch is not visible from the main checkout")
	}

	// The point of the test: the main checkout can undo what the worktree did.
	last, ok, err := Last(context.Background(), repo)
	if err != nil || !ok {
		t.Fatalf("Last() from the main checkout = %v, %v; want the worktree's entry", ok, err)
	}
	if last.ID != entry.ID {
		t.Fatalf("Last() = %s, want %s", last.ID, entry.ID)
	}
	if _, err := Undo(context.Background(), last); err != nil {
		t.Fatalf("Undo() from the main checkout = %v", err)
	}
	if gitOut(t, repo, "rev-parse", "--verify", "--quiet", "refs/heads/feature/x") != "" {
		t.Fatal("the branch survived an undo run from the main checkout")
	}
}
