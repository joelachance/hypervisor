package plan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/satoricorp/demux"
)

// Probe implements demux.ContentProbe over git, which is what lets demux drop
// hunks whose content already landed on the routed stack.
type Probe struct{}

// DiffRevisionRange implements demux.ContentProbe.
func (Probe) DiffRevisionRange(ctx context.Context, repoRoot, leftRev, rightRev string, files []string) (string, error) {
	args := append([]string{"diff", "--no-color", "--no-ext-diff", leftRev, rightRev, "--"}, files...)
	out, err := run(ctx, repoRoot, args...)
	if err != nil {
		if strings.Contains(err.Error(), "unknown revision") || strings.Contains(err.Error(), "bad revision") {
			return "", fmt.Errorf("%s: %w", leftRev, demux.ErrRevisionUnavailable)
		}
		return "", err
	}
	return out, nil
}

// WorkingDiff returns the unified diff hv plans over. Scoping it to files is
// how a session selection narrows the plan to what those sessions touched.
//
// Untracked files are included. `git diff` alone does not see them, and a
// coding session's most characteristic act is creating a file - leaving them
// out silently plans a fraction of the work, which is worse than failing.
func WorkingDiff(ctx context.Context, repoRoot string, staged bool, files []string) (string, error) {
	args := []string{"diff", "--no-color", "--no-ext-diff"}
	if staged {
		args = append(args, "--cached")
	}
	args = append(args, "--")
	args = append(args, files...)
	tracked, err := runRaw(ctx, repoRoot, args...)
	if err != nil {
		return "", err
	}
	if staged {
		// Staged means the index, and an untracked file is by definition not
		// in it.
		return tracked, nil
	}
	untracked, err := untrackedDiff(ctx, repoRoot, files)
	if err != nil {
		return "", err
	}
	return tracked + untracked, nil
}

// untrackedDiff renders new files as unified diffs against nothing.
//
// This deliberately avoids `git add -N`, which would get the same result by
// writing to the index: hv plans, it does not stage, and a planning command
// that mutates the repository is a planning command people stop trusting.
func untrackedDiff(ctx context.Context, repoRoot string, files []string) (string, error) {
	args := append([]string{"ls-files", "--others", "--exclude-standard", "--"}, files...)
	listing, err := runRaw(ctx, repoRoot, args...)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, path := range strings.Split(listing, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		// --no-index exits 1 whenever the files differ, which is always here,
		// so its status says nothing and the output has to be read anyway.
		diff := runIgnoringStatus(ctx, repoRoot,
			"diff", "--no-color", "--no-ext-diff", "--no-index", "--", os.DevNull, path)
		if strings.TrimSpace(diff) == "" {
			continue
		}
		builder.WriteString(diff)
	}
	return builder.String(), nil
}

// HeadCommit returns the commit the working diff is taken against. It anchors
// the plan to an immutable source, which already-applied dedup requires.
func HeadCommit(ctx context.Context, repoRoot string) (string, error) {
	return run(ctx, repoRoot, "rev-parse", "HEAD")
}

// RepoRoot resolves the worktree root containing dir.
func RepoRoot(ctx context.Context, dir string) (string, error) {
	return run(ctx, dir, "rev-parse", "--show-toplevel")
}

func run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := runRaw(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

// runIgnoringStatus returns git's stdout whatever its exit status. Some git
// commands report findings through the exit code rather than failure, and for
// those the output is the answer.
func runIgnoringStatus(ctx context.Context, dir string, args ...string) string {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	out, _ := command.Output()
	return string(out)
}

func runRaw(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		detail := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, detail)
	}
	return string(out), nil
}
