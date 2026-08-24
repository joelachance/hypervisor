package plan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/hv/session"
)

func TestEventsForKeepsOnlySelectedSessions(t *testing.T) {
	events := []capture.SessionEvent{
		{SessionID: "s1", Kind: capture.KindEdit},
		{SessionID: "s2", Kind: capture.KindEdit},
	}
	got := eventsFor([]session.Session{{ID: "s2"}}, events)
	if len(got) != 1 || got[0].SessionID != "s2" {
		t.Fatalf("eventsFor() = %v, want only s2", got)
	}
	if all := eventsFor(nil, events); len(all) != 2 {
		t.Fatalf("eventsFor(nil) = %v, want everything unfiltered", all)
	}
}

// TestRunPlansWorkingTree exercises the two-pass handoff against a real
// repository: propose to learn the hunk catalog, match, propose again with the
// evidence attached.
func TestRunPlansWorkingTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := newRepo(t)
	write(t, repo, "billing.go", "package billing\n\nfunc Total() int { return 2 }\n")

	result, err := Run(context.Background(), Request{
		RepoRoot: repo,
		Sessions: []session.Session{{ID: "s1", Tool: capture.ToolClaude, Files: []string{"billing.go"}}},
		Events: []capture.SessionEvent{
			{SessionID: "s1", Tool: capture.ToolClaude, Kind: capture.KindEdit, TS: 100, FilePath: filepath.Join(repo, "billing.go")},
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Packet.Proposal.Revisions) == 0 {
		t.Fatal("Run() produced no revisions")
	}
	if len(result.Attributed.Links) != len(result.Packet.Proposal.Hunks) {
		t.Fatalf("attributed %d of %d hunks; want every hunk linked to s1",
			len(result.Attributed.Links), len(result.Packet.Proposal.Hunks))
	}
	for _, link := range result.Attributed.Links {
		if link.SessionID != "s1" {
			t.Errorf("link %s attributed to %q, want s1", link.HunkID, link.SessionID)
		}
	}
	for _, revision := range result.Packet.Proposal.Revisions {
		if len(revision.SessionIDs) == 0 {
			t.Errorf("revision %s carries no session provenance", revision.ID)
		}
	}
}

func TestRunRejectsCleanTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := newRepo(t)
	if _, err := Run(context.Background(), Request{RepoRoot: repo}); err == nil {
		t.Fatal("Run() over a clean tree returned no error")
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "hv@test"},
		{"config", "user.name", "hv"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write(t, repo, "billing.go", "package billing\n\nfunc Total() int { return 1 }\n")
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "seed"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return repo
}

func write(t *testing.T, repo, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A coding session's most characteristic act is creating a file, and `git
// diff` alone does not see one. Planning a fraction of the work silently is
// worse than failing.
func TestRunIncludesUntrackedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := newRepo(t)
	write(t, repo, "limiter.go", "package billing\n\nfunc Limit() int { return 1 }\n")

	result, err := Run(context.Background(), Request{RepoRoot: repo})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	found := false
	for _, hunk := range result.Packet.Proposal.Hunks {
		if hunk.File == "limiter.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the new file never reached the plan; hunks = %+v", result.Packet.Proposal.Hunks)
	}
}

func TestUntrackedFilesRespectTheSessionScope(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := newRepo(t)
	write(t, repo, "wanted.go", "package billing\n\nfunc Wanted() int { return 1 }\n")
	write(t, repo, "unwanted.go", "package billing\n\nfunc Unwanted() int { return 2 }\n")

	result, err := Run(context.Background(), Request{
		RepoRoot: repo,
		Sessions: []session.Session{{ID: "s1", Files: []string{filepath.Join(repo, "wanted.go")}}},
		Events: []capture.SessionEvent{{SessionID: "s1", Kind: capture.KindEdit, TS: 1,
			FilePath: filepath.Join(repo, "wanted.go")}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, hunk := range result.Packet.Proposal.Hunks {
		if hunk.File == "unwanted.go" {
			t.Fatal("a file the session never touched reached the plan")
		}
	}
}

// demux stamps provenance from exact and fuzzy links only, so with hv's
// temporal links nearly every revision takes the fallback. Naming every
// selected session there would credit all of them with all of the work.
func TestFallbackProvenanceNamesAtMostOneSession(t *testing.T) {
	one := []session.Session{{ID: "s1"}}
	if got := fallbackSessionIDs(one); len(got) != 1 || got[0] != "s1" {
		t.Errorf("fallback for one session = %v, want it named", got)
	}
	two := []session.Session{{ID: "s1"}, {ID: "s2"}}
	if got := fallbackSessionIDs(two); len(got) != 0 {
		t.Errorf("fallback for two sessions = %v, want silence rather than crediting both", got)
	}
}

// hv watches every session on the machine, so a selection can span
// repositories - a plan cannot, and each group gets its own.
func TestGroupByRepoSplitsSelectionsAcrossRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	first, second := newRepo(t), newRepo(t)
	sessions := []session.Session{
		{ID: "s1", Files: []string{filepath.Join(first, "billing.go")}},
		{ID: "s2", Files: []string{filepath.Join(second, "billing.go")}},
		{ID: "s3", Files: []string{filepath.Join(first, "billing.go")}},
	}
	order, groups, problems := GroupByRepo(context.Background(), sessions)
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if len(order) != 2 {
		t.Fatalf("order = %v, want one entry per repository", order)
	}
	if len(groups[order[0]]) != 2 || len(groups[order[1]]) != 1 {
		t.Fatalf("groups = %v, want the two sessions in the first repo together", groups)
	}
}

// A session's recorded directory is where the agent started, which may be a
// parent of the repository or a worktree; the files it edited are better
// evidence.
func TestRepoOfPrefersEditedFilesOverTheRecordedDirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := newRepo(t)
	got, err := RepoOf(context.Background(), session.Session{
		ID:       "s1",
		RepoRoot: filepath.Dir(repo), // the parent, which is not a repository
		Files:    []string{filepath.Join(repo, "billing.go")},
	})
	if err != nil {
		t.Fatalf("RepoOf() error = %v", err)
	}
	if filepath.Base(got) != filepath.Base(repo) {
		t.Fatalf("RepoOf() = %q, want the repository the file lives in", got)
	}
}
