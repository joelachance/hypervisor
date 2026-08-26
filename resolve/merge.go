package resolve

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/satoricorp/hv/apply"
	"github.com/satoricorp/hv/conflict"
)

// Fixed is one file the agent resolved.
type Fixed struct {
	Path        string       `json:"path"`
	Regions     int          `json:"regions"`
	Resolutions []Resolution `json:"resolutions"`
	Problems    []string     `json:"problems,omitempty"`
}

// Outcome is a resolved merge: the branch and commit it produced, and what was
// decided to get there.
type Outcome struct {
	Repo     string          `json:"repo"`
	Runner   string          `json:"runner"`
	Branch   string          `json:"branch"`
	Commit   string          `json:"commit"`
	Tree     string          `json:"tree"`
	Parents  []string        `json:"parents"`
	Fixed    []Fixed         `json:"fixed"`
	Problems []string        `json:"problems,omitempty"`
	Report   conflict.Report `json:"report"`
	EntryID  string          `json:"entry_id,omitempty"`
}

// Merge resolves every conflict in a report and commits the result as a real
// merge commit - two parents, on its own branch.
//
// A resolution that cannot be verified stops the merge before anything is
// written. Committing a merge that quietly lost a line is worse than not
// merging: nobody would go looking for what went missing.
func Merge(ctx context.Context, repo string, report conflict.Report, runner Runner, branch string) (Outcome, error) {
	outcome := Outcome{Repo: repo, Runner: runner.Name(), Branch: branch, Report: report}
	if report.Clean {
		return outcome, fmt.Errorf("nothing to resolve: %s and %s merge cleanly",
			label(report.Ours.Title, report.Ours.Ref), label(report.Theirs.Title, report.Theirs.Ref))
	}

	blobs := map[string]string{}
	for _, fileConflict := range report.Conflicts {
		fixed := Fixed{Path: fileConflict.Path, Regions: len(fileConflict.Regions)}
		merged := fileConflict.Merged
		for _, region := range fileConflict.Regions {
			request := requestForRegion(report, fileConflict.Path, region)
			resolution, err := runner.Resolve(ctx, request)
			if err != nil {
				return outcome, fmt.Errorf("%s region %d: %w", fileConflict.Path, region.Index, err)
			}
			if problems := Verify(request, resolution); len(problems) > 0 {
				fixed.Problems = append(fixed.Problems, problems...)
			}
			replaced, err := replaceRegion(merged, region.Index, resolution.Resolved)
			if err != nil {
				return outcome, fmt.Errorf("%s region %d: %w", fileConflict.Path, region.Index, err)
			}
			merged = replaced
			fixed.Resolutions = append(fixed.Resolutions, resolution)
		}
		if strings.Contains(merged, "<<<<<<<") {
			return outcome, fmt.Errorf("%s still has conflict markers after resolution", fileConflict.Path)
		}
		blob, err := hashObject(ctx, repo, merged)
		if err != nil {
			return outcome, err
		}
		blobs[fileConflict.Path] = blob
		outcome.Problems = append(outcome.Problems, fixed.Problems...)
		outcome.Fixed = append(outcome.Fixed, fixed)
	}
	if len(outcome.Problems) > 0 {
		return outcome, fmt.Errorf("%d line(s) unaccounted for; nothing was committed", len(outcome.Problems))
	}

	tree, err := treeWith(ctx, repo, report.MergedTree, blobs)
	if err != nil {
		return outcome, err
	}
	outcome.Tree = tree

	ours, err := git(ctx, repo, "rev-parse", report.Ours.Ref)
	if err != nil {
		return outcome, err
	}
	theirs, err := git(ctx, repo, "rev-parse", report.Theirs.Ref)
	if err != nil {
		return outcome, err
	}
	outcome.Parents = []string{ours, theirs}

	commit, err := commitTree(ctx, repo, tree, []string{ours, theirs}, message(report, outcome))
	if err != nil {
		return outcome, err
	}
	outcome.Commit = commit

	before, _ := git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if _, err := git(ctx, repo, "update-ref", "refs/heads/"+branch, commit); err != nil {
		return outcome, err
	}
	entry := apply.Entry{
		ID:        apply.NewID(),
		AppliedAt: time.Now().UnixMilli(),
		Repo:      repo,
		Base:      report.Base,
		Commits: []apply.Commit{{
			Revision: "merge", Branch: branch, SHA: commit,
			Subject: firstLine(message(report, outcome)), Files: len(outcome.Fixed),
		}},
		Branches: []apply.BranchChange{{Name: branch, Before: before, After: commit}},
	}
	if err := apply.Journal(ctx, repo, entry); err != nil {
		return outcome, err
	}
	outcome.EntryID = entry.ID
	return outcome, nil
}

func requestForRegion(report conflict.Report, path string, region conflict.Region) Request {
	request := Request{
		Path: path, Region: region.Index,
		Base: region.Base, Ours: region.Ours, Theirs: region.Theirs,
		OursFrom:   label(report.Ours.Title, report.Ours.Ref),
		TheirsFrom: label(report.Theirs.Title, report.Theirs.Ref),
	}
	if report.Ours.Context != nil {
		request.OursIntent = report.Ours.Context.Intent
	}
	if report.Theirs.Context != nil {
		request.TheirsIntent = report.Theirs.Context.Intent
	}
	return request
}

// replaceRegion swaps the nth conflict block for the resolved lines, leaving
// everything around it byte for byte as git produced it.
func replaceRegion(merged string, index int, resolved []string) (string, error) {
	lines := strings.Split(merged, "\n")
	out := make([]string, 0, len(lines))
	seen, inBlock, replaced := 0, false, false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "<<<<<<<"):
			seen++
			inBlock = seen == index
			if !inBlock {
				out = append(out, line)
				continue
			}
			out = append(out, resolved...)
			replaced = true
		case inBlock && strings.HasPrefix(line, ">>>>>>>"):
			inBlock = false
		case inBlock:
			// dropped: the resolution replaces the whole block
		default:
			out = append(out, line)
		}
	}
	if !replaced {
		return "", fmt.Errorf("region %d not found", index)
	}
	return strings.Join(out, "\n"), nil
}

// treeWith writes a tree that is the merge's tree with the resolved files
// swapped in. A scratch index again, so nothing touches the real one.
func treeWith(ctx context.Context, repo, baseTree string, blobs map[string]string) (string, error) {
	indexFile, err := os.CreateTemp("", "hv-merge-index-*")
	if err != nil {
		return "", err
	}
	indexPath := indexFile.Name()
	indexFile.Close()
	os.Remove(indexPath)
	defer os.Remove(indexPath)

	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if _, err := gitEnv(ctx, repo, env, "read-tree", baseTree); err != nil {
		return "", err
	}
	for path, blob := range blobs {
		mode := fileMode(ctx, repo, baseTree, path)
		if _, err := gitEnv(ctx, repo, env, "update-index", "--add", "--cacheinfo",
			fmt.Sprintf("%s,%s,%s", mode, blob, path)); err != nil {
			return "", err
		}
	}
	return gitEnv(ctx, repo, env, "write-tree")
}

func fileMode(ctx context.Context, repo, tree, path string) string {
	out, err := git(ctx, repo, "ls-tree", tree, "--", path)
	if err != nil || out == "" {
		return "100644"
	}
	if fields := strings.Fields(out); len(fields) > 0 {
		return fields[0]
	}
	return "100644"
}

func message(report conflict.Report, outcome Outcome) string {
	ours := label(report.Ours.Title, report.Ours.Ref)
	theirs := label(report.Theirs.Title, report.Theirs.Ref)
	var builder strings.Builder
	fmt.Fprintf(&builder, "merge %s and %s\n\n", ours, theirs)
	fmt.Fprintf(&builder, "Resolved by %s.\n", outcome.Runner)
	for _, fixed := range outcome.Fixed {
		for _, resolution := range fixed.Resolutions {
			if strings.TrimSpace(resolution.Reasoning) == "" {
				continue
			}
			fmt.Fprintf(&builder, "\n%s: %s", fixed.Path, resolution.Reasoning)
		}
	}
	builder.WriteString("\n")
	for _, side := range []conflict.Side{report.Ours, report.Theirs} {
		if side.SessionID != "" {
			fmt.Fprintf(&builder, "\nhv-session: %s", side.SessionID)
			if side.Title != "" {
				fmt.Fprintf(&builder, " (%s)", side.Title)
			}
		}
	}
	builder.WriteString("\n")
	return builder.String()
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}

func label(title, ref string) string {
	if strings.TrimSpace(title) != "" {
		return title
	}
	return ref
}

func hashObject(ctx context.Context, repo, body string) (string, error) {
	command := exec.CommandContext(ctx, "git", "hash-object", "-w", "--stdin")
	command.Dir = repo
	command.Stdin = strings.NewReader(body)
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git hash-object: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func commitTree(ctx context.Context, repo, tree string, parents []string, body string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	args = append(args, "-F", "-")
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = repo
	command.Stdin = strings.NewReader(body)
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git commit-tree: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitEnv(ctx, dir, nil, args...)
}

func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = env
	out, err := command.Output()
	if err != nil {
		detail := ""
		var exitErr *exec.ExitError
		if ok := asExit(err, &exitErr); ok {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, detail)
	}
	return strings.TrimSpace(string(out)), nil
}
