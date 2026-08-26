package merge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/demux/matcher"

	"github.com/satoricorp/hv/parse"
	"github.com/satoricorp/hv/session"
)

// Attribution comes from hv's own links, not the proposal's provenance field,
// which demux leaves empty more often than not.
func TestRevisionSessionsReadsTheLinks(t *testing.T) {
	revision := demux.RevisionProposal{ID: "u1", HunkIDs: []string{"h1", "h2", "h3"}}
	links := []matcher.HunkLink{
		{HunkID: "h1", SessionID: "s1"},
		{HunkID: "h2", SessionID: "s2"},
		{HunkID: "h3", SessionID: "s1"},
	}
	got := RevisionSessions(revision, links)
	if len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Fatalf("sessions = %v, want each once, in hunk order", got)
	}
}

func TestRevisionSessionsFallsBackToEmbeddedHunks(t *testing.T) {
	revision := demux.RevisionProposal{ID: "u1", Hunks: []demux.HunkRange{{ID: "h7"}}}
	got := RevisionSessions(revision, []matcher.HunkLink{{HunkID: "h7", SessionID: "s9"}})
	if len(got) != 1 || got[0] != "s9" {
		t.Fatalf("sessions = %v; a shaping pass can clear HunkIDs while the hunks survive", got)
	}
}

// The whole point of this package: one path from sessions to commits, so the
// browser and the command line cannot drift apart.
func TestCommitWritesTheWholeStory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "hv@test")
	run("config", "user.name", "hv")
	write("limits.go", "package app\n\nfunc Limit() int { return 1 }\n")
	run("add", "-A")
	run("commit", "-qm", "seed")
	write("limits.go", "package app\n\nfunc Limit() int { return 2 }\n")

	home := t.TempDir()
	parse.SetHome(home)
	defer parse.SetHome("")
	logDir := filepath.Join(home, ".claude", "projects", "-repo")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "s1.jsonl")
	transcript := `{"type":"user","sessionId":"s1","timestamp":"2026-08-26T10:00:00Z","cwd":"` + repo +
		`","message":{"content":[{"type":"text","text":"raise the request limit"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	current := session.Session{
		ID: "s1", Tool: capture.ToolClaude, Title: "the limit", RepoRoot: repo,
		Files: []string{filepath.Join(repo, "limits.go")}, LogPath: logPath,
		LastActiveAt: time.Now().UnixMilli(),
	}
	result, err := Commit(context.Background(), Request{
		RepoRoot: repo,
		Sessions: []session.Session{current},
		Events: []capture.SessionEvent{{
			SessionID: "s1", Kind: capture.KindEdit, TS: time.Now().UnixMilli(),
			FilePath: filepath.Join(repo, "limits.go"), NewText: "func Limit() int { return 2 }",
		}},
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if !result.Committed || len(result.Entry.Commits) == 0 {
		t.Fatal("nothing was committed")
	}

	body, err := exec.Command("git", "-C", repo, "log", "-1", "--format=%B",
		result.Entry.Commits[0].SHA).Output()
	if err != nil {
		t.Fatal(err)
	}
	message := string(body)
	for _, want := range []string{"raise the request limit", "the limit", "Changed:", "hv-session: s1"} {
		if !strings.Contains(message, want) {
			t.Errorf("message is missing %q:\n%s", want, message)
		}
	}

	// The commits are copies: the working tree still has the change.
	status, _ := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	if !strings.Contains(string(status), "limits.go") {
		t.Errorf("status = %q, want the change still uncommitted", status)
	}
}

// A target branch that is the checked-out one has to finish the job: the ref
// moves and the index moves with it, or git reports the work as changes that
// undo the commit.
func TestCommitToHeadRefreshesTheIndex(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"config", "user.email", "hv@test"}, {"config", "user.name", "hv"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "seed"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Commit(context.Background(), Request{RepoRoot: repo, Branch: "HEAD"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if result.Entry.CheckedOut != "main" {
		t.Fatalf("CheckedOut = %q, want main", result.Entry.CheckedOut)
	}
	status, _ := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	if strings.TrimSpace(string(status)) != "" {
		t.Fatalf("status = %q, want a clean tree - the work is committed now", status)
	}
}
