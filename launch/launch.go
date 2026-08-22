// Package launch reopens a coding session where it lives.
//
// Three ways a session can be reachable, in descending order of certainty:
//
//	tmux  a pane is still running `claude --resume <id>`, so attaching to it
//	      returns to the live session with its scrollback intact
//	app   the desktop app knows the session, and its claude://resume deep link
//	      opens it there
//	cli   nothing is running it, so it is resumed fresh in this terminal
//
// A terminal tab that is not under tmux cannot be raised - there is no handle
// for "that tab" - so a plain CLI session is resumed here rather than pretended
// back into the window it came from.
package launch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"

	"github.com/satoricorp/hv/session"
)

// Method is how a session will be reopened.
type Method string

const (
	MethodTmux Method = "tmux"
	MethodApp  Method = "app"
	MethodCLI  Method = "cli"
)

// Target is a resolved plan for reopening one session.
type Target struct {
	Session session.Session
	Method  Method
	// Command is what will run, argv style.
	Command []string
	// Detail explains the choice - the tmux session name, the deep link, the
	// directory a fresh resume will start in.
	Detail string
	// Replace means Run should replace this process rather than spawn: true
	// for an interactive attach or resume, false for a deep link that hands
	// off to another application.
	Replace bool
}

// String renders the command a target will run.
func (t Target) String() string { return strings.Join(t.Command, " ") }

// Plan decides how to reopen a session without running anything.
func Plan(ctx context.Context, current session.Session) (Target, error) {
	if pane, ok := findTmuxPane(ctx, current.ID); ok {
		command := []string{"tmux"}
		if pane.socket != "" {
			command = append(command, "-L", pane.socket)
		}
		command = append(command, "attach-session", "-t", pane.name)
		return Target{
			Session: current,
			Method:  MethodTmux,
			Command: command,
			Detail:  fmt.Sprintf("tmux session %q is still running this session", pane.name),
			Replace: true,
		}, nil
	}
	if current.Origin == session.OriginApp || current.AppSessionID != "" {
		return Target{
			Session: current,
			Method:  MethodApp,
			Command: []string{"open", DeepLink(current.ID)},
			Detail:  "the desktop app owns this session",
		}, nil
	}
	if strings.TrimSpace(current.RepoRoot) == "" {
		return Target{}, fmt.Errorf("session %s has no recorded directory to resume in", short(current.ID))
	}
	return Target{
		Session: current,
		Method:  MethodCLI,
		Command: []string{"claude", "--resume", current.ID},
		Detail:  "resuming in " + current.RepoRoot,
		Replace: true,
	}, nil
}

// DeepLink is the URL that opens a CLI session in the desktop app.
//
// The app registers the claude:// scheme and handles a resume link; the
// parameter name is taken from the app's own error strings rather than from
// documentation, so treat a failure here as "wrong shape", not "no such
// feature".
func DeepLink(sessionID string) string {
	return "claude://resume?sessionId=" + sessionID
}

// Run carries out a target.
func Run(ctx context.Context, target Target) error {
	if len(target.Command) == 0 {
		return fmt.Errorf("nothing to run")
	}
	if target.Replace {
		return replace(target)
	}
	command := exec.CommandContext(ctx, target.Command[0], target.Command[1:]...)
	command.Dir = target.Session.RepoRoot
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	return command.Run()
}

// replace hands the terminal to the session, which is the only way an
// interactive attach or resume can work: hv has nothing useful left to do, and
// staying in the way would leave the session sharing a tty with its watcher.
func replace(target Target) error {
	binary, err := exec.LookPath(target.Command[0])
	if err != nil {
		return err
	}
	if dir := strings.TrimSpace(target.Session.RepoRoot); dir != "" && target.Method == MethodCLI {
		if err := os.Chdir(dir); err != nil {
			return err
		}
	}
	return syscall.Exec(binary, target.Command, os.Environ())
}

type tmuxPane struct{ socket, name string }

// tmuxResume matches a tmux command line that launched a claude session, which
// is how a pane is tied back to a session id. The id only appears when the
// pane was started with --resume; a session started plain is invisible here.
var tmuxResume = regexp.MustCompile(`tmux(?:\s+-L\s+(\S+))?[^\n]*?-s\s+(\S+)[^\n]*?--resume'?\s+'?([0-9a-fA-F-]{36})`)

func findTmuxPane(ctx context.Context, sessionID string) (tmuxPane, bool) {
	out, err := exec.CommandContext(ctx, "ps", "-eo", "command").Output()
	if err != nil {
		return tmuxPane{}, false
	}
	return findTmuxPaneIn(string(out), sessionID)
}

func findTmuxPaneIn(processes, sessionID string) (tmuxPane, bool) {
	for _, line := range strings.Split(processes, "\n") {
		match := tmuxResume.FindStringSubmatch(line)
		if match == nil || !strings.EqualFold(match[3], sessionID) {
			continue
		}
		return tmuxPane{socket: match[1], name: strings.Trim(match[2], "'\"")}, true
	}
	return tmuxPane{}, false
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
