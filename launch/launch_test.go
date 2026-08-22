package launch

import (
	"context"
	"strings"
	"testing"

	"github.com/satoricorp/hv/session"
)

// The tmux case is the only one that returns to a live session with its
// scrollback, so it is tried first.
func TestFindTmuxPaneReadsTheResumeArgument(t *testing.T) {
	processes := strings.Join([]string{
		"/usr/sbin/syslogd",
		"/opt/homebrew/bin/tmux -L yeet new-session -d -s yeet-tennis-1 -c /Users/joe/git/tennis 'claude' '--resume' '74a5cde7-9f1b-4706-b3f1-7ebc4175a2d0'",
		"tmux new-session -d -s plain -c /repo claude --resume 11111111-2222-3333-4444-555555555555",
	}, "\n")

	pane, ok := findTmuxPaneIn(processes, "74a5cde7-9f1b-4706-b3f1-7ebc4175a2d0")
	if !ok || pane.name != "yeet-tennis-1" || pane.socket != "yeet" {
		t.Fatalf("pane = %+v, %v; want the yeet-socket tennis session", pane, ok)
	}
	if plain, ok := findTmuxPaneIn(processes, "11111111-2222-3333-4444-555555555555"); !ok || plain.socket != "" {
		t.Fatalf("pane = %+v, %v; want the default-socket session", plain, ok)
	}
	if _, ok := findTmuxPaneIn(processes, "99999999-9999-9999-9999-999999999999"); ok {
		t.Error("a session no pane is running must not match")
	}
}

func TestPlanUsesTheDeepLinkForAppSessions(t *testing.T) {
	target, err := Plan(context.Background(), session.Session{
		ID: "abc", Origin: session.OriginApp, AppSessionID: "local_x", RepoRoot: "/repo",
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if target.Method != MethodApp || target.String() != "open claude://resume?sessionId=abc" {
		t.Fatalf("target = %+v, want the desktop deep link", target)
	}
	if target.Replace {
		t.Error("handing off to another application must not replace this process")
	}
}

func TestPlanResumesCLISessionsInPlace(t *testing.T) {
	target, err := Plan(context.Background(), session.Session{
		ID: "abc", Origin: session.OriginCLI, RepoRoot: "/repo",
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if target.Method != MethodCLI || target.String() != "claude --resume abc" {
		t.Fatalf("target = %+v, want a fresh resume", target)
	}
	if !target.Replace {
		t.Error("an interactive resume must take over the terminal")
	}
}

// Without a directory there is nothing to resume into, and guessing one would
// start the session in the wrong repo.
func TestPlanRefusesWithoutADirectory(t *testing.T) {
	if _, err := Plan(context.Background(), session.Session{ID: "abc", Origin: session.OriginCLI}); err == nil {
		t.Fatal("Plan() accepted a session with no recorded directory")
	}
}
