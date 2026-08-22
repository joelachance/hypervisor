package desktop

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/satoricorp/hv/session"
)

func writeRecord(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "account", "org")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKeysByCLISessionID(t *testing.T) {
	root := t.TempDir()
	writeRecord(t, root, "local_a.json", `{"sessionId":"local_a","cliSessionId":"cli-1",
	 "title":"benchmarks","titleSource":"user","isArchived":false,"branch":"claude/bench",
	 "lastActivityAt":200,"prs":[{"prNumber":58,"state":"MERGED","url":"u","repo":"r"}]}`)

	records, err := LoadFrom(root)
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	got, ok := records["cli-1"]
	if !ok {
		t.Fatal("record not keyed by cliSessionId")
	}
	if got.Title != "benchmarks" || !got.UserNamed() {
		t.Errorf("title = %q (source %q), want a user-authored benchmarks", got.Title, got.TitleSource)
	}
	if len(got.PRs) != 1 || got.PRs[0].Number != 58 || got.PRs[0].State != "MERGED" {
		t.Errorf("prs = %+v, want PR 58 merged", got.PRs)
	}
}

// A session resumed more than once has several registry entries; the one the
// app shows is the most recently active.
func TestLoadKeepsTheMostRecentEntry(t *testing.T) {
	root := t.TempDir()
	writeRecord(t, root, "local_old.json", `{"cliSessionId":"cli-1","title":"old","lastActivityAt":100}`)
	writeRecord(t, root, "local_new.json", `{"cliSessionId":"cli-1","title":"new","lastActivityAt":900}`)

	records, _ := LoadFrom(root)
	if records["cli-1"].Title != "new" {
		t.Fatalf("title = %q, want the most recently active entry", records["cli-1"].Title)
	}
}

func TestLoadToleratesMissingAndBrokenFiles(t *testing.T) {
	if records, err := LoadFrom(filepath.Join(t.TempDir(), "absent")); err != nil || len(records) != 0 {
		t.Fatalf("LoadFrom(missing) = %v, %v; a machine without the app is not an error", records, err)
	}
	root := t.TempDir()
	writeRecord(t, root, "local_broken.json", `{not json`)
	writeRecord(t, root, "local_ok.json", `{"cliSessionId":"cli-1","title":"fine"}`)
	records, err := LoadFrom(root)
	if err != nil || len(records) != 1 {
		t.Fatalf("LoadFrom() = %v, %v; one broken file must not lose the rest", records, err)
	}
}

// The app's title wins: a person's own name for a session is the only name
// they will recognize, and even its automatic title is what they have seen.
func TestApplyPrefersTheAppTitle(t *testing.T) {
	sessions := []session.Session{{ID: "cli-1", Title: "Setup gx review script for Code Review Bench", TitleSource: "auto"}}
	records := map[string]Record{"cli-1": {
		CLISessionID: "cli-1", Title: "benchmarks", TitleSource: "user",
		Archived: true, Branch: "claude/bench", AppSessionID: "local_a",
	}}
	got := Apply(sessions, records)[0]
	if got.Title != "benchmarks" || got.TitleSource != "user" {
		t.Errorf("title = %q/%q, want the app's user-authored name", got.Title, got.TitleSource)
	}
	if !got.Archived || got.Branch != "claude/bench" || got.AppSessionID != "local_a" {
		t.Errorf("overlay = %+v, want archive state, branch, and app id carried over", got)
	}
}

func TestApplyLeavesUnknownSessionsAlone(t *testing.T) {
	sessions := []session.Session{{ID: "cli-9", Title: "from the transcript"}}
	got := Apply(sessions, map[string]Record{})[0]
	if got.Title != "from the transcript" {
		t.Fatalf("title = %q, want the transcript's title when the app knows nothing", got.Title)
	}
}
