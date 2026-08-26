package resolve

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/satoricorp/hv/conflict"
	"github.com/satoricorp/hv/internal/gitx"
	"github.com/satoricorp/hv/journal"
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
		// Every region is resolved first and the file rewritten in one pass.
		// Replacing them one at a time would renumber the blocks that follow,
		// so region 2 would be looked for in a file that no longer has one.
		replacements := make([][]string, 0, len(fileConflict.Regions))
		for _, region := range fileConflict.Regions {
			request := requestForRegion(report, fileConflict.Path, region)
			resolution, err := runner.Resolve(ctx, request)
			if err != nil {
				return outcome, fmt.Errorf("%s region %d: %w", fileConflict.Path, region.Index, err)
			}
			if problems := Verify(request, resolution); len(problems) > 0 {
				fixed.Problems = append(fixed.Problems, problems...)
			}
			replacements = append(replacements, resolution.Resolved)
			fixed.Resolutions = append(fixed.Resolutions, resolution)
		}
		merged, err := gitx.ReplaceRegions(fileConflict.Merged, replacements)
		if err != nil {
			return outcome, fmt.Errorf("%s: %w", fileConflict.Path, err)
		}
		if strings.Contains(merged, "<<<<<<<") {
			return outcome, fmt.Errorf("%s still has conflict markers after resolution", fileConflict.Path)
		}
		blob, err := gitx.HashObject(ctx, repo, merged)
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

	ours, err := gitx.Run(ctx, repo, "rev-parse", report.Ours.Ref)
	if err != nil {
		return outcome, err
	}
	theirs, err := gitx.Run(ctx, repo, "rev-parse", report.Theirs.Ref)
	if err != nil {
		return outcome, err
	}
	outcome.Parents = []string{ours, theirs}

	commit, err := gitx.CommitTree(ctx, repo, tree, []string{ours, theirs}, message(report, outcome), nil)
	if err != nil {
		return outcome, err
	}
	outcome.Commit = commit

	before, _ := gitx.Run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if _, err := gitx.Run(ctx, repo, "update-ref", "refs/heads/"+branch, commit); err != nil {
		return outcome, err
	}
	entry := journal.Entry{
		ID:        journal.NewID(),
		AppliedAt: time.Now().UnixMilli(),
		Repo:      repo,
		Base:      report.Base,
		Commits: []journal.Commit{{
			Revision: "merge", Branch: branch, SHA: commit,
			Subject: firstLine(message(report, outcome)), Files: len(outcome.Fixed),
		}},
		Branches: []journal.BranchChange{{Name: branch, Before: before, After: commit}},
	}
	if err := journal.Record(ctx, repo, entry); err != nil {
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

// treeWith writes a tree that is the merge's tree with the resolved files
// swapped in. A scratch index again, so nothing touches the real one.
func treeWith(ctx context.Context, repo, baseTree string, blobs map[string]string) (tree string, err error) {
	err = gitx.ScratchIndex(func(env []string) error {
		if _, err := gitx.RunIn(ctx, repo, env, "read-tree", baseTree); err != nil {
			return err
		}
		for path, blob := range blobs {
			mode := fileMode(ctx, repo, baseTree, path)
			if _, err := gitx.RunIn(ctx, repo, env, "update-index", "--add", "--cacheinfo",
				fmt.Sprintf("%s,%s,%s", mode, blob, path)); err != nil {
				return err
			}
		}
		tree, err = gitx.RunIn(ctx, repo, env, "write-tree")
		return err
	})
	return tree, err
}

func fileMode(ctx context.Context, repo, tree, path string) string {
	out, err := gitx.Run(ctx, repo, "ls-tree", tree, "--", path)
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
