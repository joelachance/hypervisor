// Package gitx is the one place hv shells out to git.
//
// Four packages had grown their own copy of these five functions, which is
// four places to fix a quoting bug and four subtly different ideas about when
// a non-zero exit is a failure. It is not: several git commands report a
// finding through the exit code, and reading their output is the whole point.
package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Run returns git's trimmed stdout, or an error carrying its stderr.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := RunRaw(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

// RunRaw is Run without trimming, for output whose whitespace matters - a
// diff, a file body.
func RunRaw(ctx context.Context, dir string, args ...string) (string, error) {
	return runWith(ctx, dir, nil, "", args...)
}

// RunIn runs git with an explicit environment, which is how a caller points it
// at a scratch index.
func RunIn(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	out, err := runWith(ctx, dir, env, "", args...)
	return strings.TrimSpace(out), err
}

// RunInput feeds stdin - a patch, a commit message - and returns trimmed
// stdout.
func RunInput(ctx context.Context, dir string, env []string, input string, args ...string) (string, error) {
	out, err := runWith(ctx, dir, env, input, args...)
	return strings.TrimSpace(out), err
}

// Output returns git's stdout whatever its exit status.
//
// `diff --no-index`, `merge-file`, and `merge-tree` report differences and
// conflicts through the exit code rather than failing, so for those the status
// says nothing and only the output is the answer.
func Output(ctx context.Context, dir string, args ...string) string {
	out, _ := runWith(ctx, dir, nil, "", args...)
	return out
}

// Succeeded reports whether a command exited zero, for the questions git
// answers with a status and no output - `merge-base --is-ancestor`.
func Succeeded(ctx context.Context, dir string, args ...string) bool {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	return command.Run() == nil
}

func runWith(ctx context.Context, dir string, env []string, input string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = env
	if input != "" {
		command.Stdin = strings.NewReader(input)
	}
	out, err := command.Output()
	// The output is returned even on failure: Output relies on it, and a
	// caller that wanted the error gets both.
	if err != nil {
		return string(out), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, stderr(err))
	}
	return string(out), nil
}

// stderr is what git complained about, which is the part worth showing.
func stderr(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return clip(strings.TrimSpace(string(exitErr.Stderr)))
	}
	return ""
}

// clip keeps an error readable when git decides to be verbose.
func clip(detail string) string {
	const max = 400
	if len(detail) <= max {
		return detail
	}
	return detail[:max] + "…"
}

// HashObject writes a blob and returns its id.
func HashObject(ctx context.Context, dir, body string) (string, error) {
	return RunInput(ctx, dir, nil, body, "hash-object", "-w", "--stdin")
}

// CommitTree commits a tree with the given parents and message.
func CommitTree(ctx context.Context, dir, tree string, parents []string, message string, env []string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	args = append(args, "-F", "-")
	return RunInput(ctx, dir, env, message, args...)
}

// ScratchIndex runs work against a temporary index file, so nothing hv does
// touches the real one or the working tree. The env it passes carries
// GIT_INDEX_FILE; every git call inside must use it.
func ScratchIndex(work func(env []string) error) error {
	file, err := os.CreateTemp("", "hv-index-*")
	if err != nil {
		return err
	}
	path := file.Name()
	file.Close()
	// git wants to create the file itself.
	os.Remove(path)
	defer os.Remove(path)
	return work(append(os.Environ(), "GIT_INDEX_FILE="+path))
}
