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
