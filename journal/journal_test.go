package journal

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
		{"init", "-q", "-b", "main"}, {"config", "user.email", "hv@test"}, {"config", "user.name", "hv"},
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

func headSHA(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestHistoryIsNewestFirstAndSupersedesByID(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()
	if err := Record(ctx, dir, Entry{ID: "one", Repo: dir, AppliedAt: 100}); err != nil {
		t.Fatal(err)
	}
	if err := Record(ctx, dir, Entry{ID: "two", Repo: dir, AppliedAt: 200}); err != nil {
		t.Fatal(err)
	}
	// An undo is recorded as another line with the same id.
	if err := Record(ctx, dir, Entry{ID: "one", Repo: dir, AppliedAt: 100, UndoneAt: 300}); err != nil {
		t.Fatal(err)
	}

	history, err := History(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history = %d entries, want two", len(history))
	}
	if history[0].ID != "two" {
		t.Errorf("first = %q, want the newest", history[0].ID)
	}
	for _, entry := range history {
		if entry.ID == "one" && !entry.Undone() {
			t.Error("the later line did not supersede the earlier one")
		}
	}

	last, ok, err := Last(ctx, dir)
	if err != nil || !ok || last.ID != "two" {
		t.Fatalf("Last() = %q, %v, %v; want the newest entry that is not undone", last.ID, ok, err)
	}
}

func TestUndoDeletesCreatedBranchesAndResetsExtendedOnes(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()
	seed := headSHA(t, dir)

	// Two branches at the same commit: one hv "created", one it "extended".
	for _, name := range []string{"made", "grown"} {
		if out, err := exec.Command("git", "-C", dir, "branch", name).CombinedOutput(); err != nil {
			t.Fatalf("git branch: %v: %s", err, out)
		}
	}
	entry := Entry{
		ID: "e1", Repo: dir, AppliedAt: 1,
		Branches: []BranchChange{
			{Name: "made", After: seed},
			{Name: "grown", Before: seed, After: seed},
		},
	}
	if _, err := Undo(ctx, entry); err != nil {
		t.Fatalf("Undo() = %v", err)
	}
	if out, _ := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/made").Output(); strings.TrimSpace(string(out)) != "" {
		t.Error("a branch hv created survived the undo")
	}
	if out, _ := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/grown").Output(); strings.TrimSpace(string(out)) == "" {
		t.Error("a branch hv only extended was deleted")
	}
}

// If someone else moved the branch, undo would throw their work away.
func TestUndoRefusesAMovedBranch(t *testing.T) {
	dir := repo(t)
	entry := Entry{
		ID: "e1", Repo: dir, AppliedAt: 1,
		Branches: []BranchChange{{Name: "main", Before: "aaaa", After: "bbbb"}},
	}
	if _, err := Undo(context.Background(), entry); err == nil {
		t.Fatal("undo moved a branch that had changed since it was applied")
	}
}

func TestUndoTwiceIsRefused(t *testing.T) {
	entry := Entry{ID: "e1", UndoneAt: 1}
	if _, err := Undo(context.Background(), entry); err == nil {
		t.Fatal("an entry was undone twice")
	}
}
