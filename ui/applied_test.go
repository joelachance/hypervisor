package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/hv/parse"
	"github.com/satoricorp/hv/session"
)

// The browser's merges were briefer than the same merge from the command line,
// because attribution was not passed along and revisions demux left
// unattributed committed with no Why paragraph. This runs the browser's own
// plan-and-commit path against a real repository.
func TestBrowserCommitsCarryTheWhyParagraph(t *testing.T) {
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

	// A session that edited that file, with a real transcript on disk: the Why
	// paragraph is read back from it, so an empty log would prove nothing.
	home := t.TempDir()
	parse.SetHome(home)
	defer parse.SetHome("")
	logDir := filepath.Join(home, ".claude", "projects", "-repo")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "s1.jsonl")
	transcript := `{"type":"user","sessionId":"s1","timestamp":"2026-08-26T10:00:00Z","cwd":"` + repo +
		`","message":{"content":[{"type":"text","text":"raise the request limit to two"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	current := session.Session{
		ID: "s1", Tool: capture.ToolClaude, Title: "raise the limit",
		RepoRoot: repo, Files: []string{filepath.Join(repo, "limits.go")},
		LastActiveAt: time.Now().UnixMilli(), LogPath: logPath,
	}
	events := []capture.SessionEvent{{
		SessionID: "s1", Tool: capture.ToolClaude, Kind: capture.KindEdit,
		TS: time.Now().UnixMilli(), FilePath: filepath.Join(repo, "limits.go"),
		NewText: "func Limit() int { return 2 }",
	}}

	planned, ok := planCmd([]session.Session{current}, events)().(plannedMsg)
	if !ok || planned.err != nil {
		t.Fatalf("planning failed: %v", planned.err)
	}
	if len(planned.groups) != 1 {
		t.Fatalf("groups = %d, want one", len(planned.groups))
	}
	applied, ok := applyCmd(planned.groups[0], "")().(appliedMsg)
	if !ok || applied.err != nil {
		t.Fatalf("committing failed: %v", applied.err)
	}
	if len(applied.entry.Commits) == 0 {
		t.Fatal("no commits")
	}

	body, err := exec.Command("git", "-C", repo, "log", "-1", "--format=%B",
		applied.entry.Commits[0].SHA).Output()
	if err != nil {
		t.Fatal(err)
	}
	message := string(body)
	if !strings.Contains(message, "Changed:") {
		t.Errorf("message has no change summary:\n%s", message)
	}
	if !strings.Contains(message, "hv-session: s1") {
		t.Errorf("message has no session trailer:\n%s", message)
	}
	// The attribution has to reach the commit even when demux left the
	// revision's own SessionIDs empty, which is the usual case.
	if !strings.Contains(message, "raise the limit") {
		t.Errorf("message does not name the session that wrote it:\n%s", message)
	}
	// The paragraph that only hv can write, and the one that went missing.
	if !strings.Contains(message, "raise the request limit to two") {
		t.Errorf("message has no Why paragraph from the transcript:\n%s", message)
	}
}
