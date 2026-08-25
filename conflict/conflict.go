// Package conflict finds where two sessions' work collides and writes down,
// in full, what a person or an agent has to decide to reconcile them.
//
// Git is the oracle. `git merge-tree --write-tree` merges two commits entirely
// in memory - no index, no checkout, no working tree - and its exit status is
// the verdict: clean, or a list of conflicted paths with base/ours/theirs blob
// ids. That makes the analysis deterministic and side-effect free, which is the
// same property the rest of hv relies on.
//
// What hv adds is the part git cannot know: which agent session produced each
// side, what it was asked to do, and what it concluded. A conflict reported as
// "both sessions rewrote this line" is a merge conflict; a conflict reported as
// "the rate limiter session and the retry-bug session disagree about this line,
// and here is what each was trying to do" is a decision someone can make.
package conflict

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/satoricorp/hv/recall"
)

// Side is one half of a comparison: a ref, and the session behind it.
type Side struct {
	Ref       string          `json:"ref"`
	SessionID string          `json:"session_id,omitempty"`
	Title     string          `json:"title,omitempty"`
	Context   *recall.Summary `json:"context,omitempty"`
}

// Region is one stretch of a file the two sides disagree about, with every
// line of all three versions kept - the whole point is that nothing is
// summarized away.
type Region struct {
	Index     int      `json:"index"`
	StartLine int      `json:"start_line"`
	Base      []string `json:"base"`
	Ours      []string `json:"ours"`
	Theirs    []string `json:"theirs"`
	Identical bool     `json:"identical,omitempty"`
}

// FileConflict is one path the merge could not settle.
type FileConflict struct {
	Path       string   `json:"path"`
	BaseBlob   string   `json:"base_blob,omitempty"`
	OursBlob   string   `json:"ours_blob,omitempty"`
	TheirsBlob string   `json:"theirs_blob,omitempty"`
	Regions    []Region `json:"regions"`
	// Merged is the file with conflict markers, exactly as git produced it.
	Merged string `json:"merged,omitempty"`
}

// Step is one decision to make. Steps are ordered and exhaustive: applying
// every step resolves the merge, and no changed line is outside one.
type Step struct {
	N          int      `json:"n"`
	Action     string   `json:"action"`
	Path       string   `json:"path"`
	Region     int      `json:"region,omitempty"`
	Reason     string   `json:"reason"`
	Ours       []string `json:"ours,omitempty"`
	Theirs     []string `json:"theirs,omitempty"`
	Base       []string `json:"base,omitempty"`
	OursFrom   string   `json:"ours_from,omitempty"`
	TheirsFrom string   `json:"theirs_from,omitempty"`
	Lines      int      `json:"lines"`
}

// Actions a step can call for. Nothing here picks a winner automatically: a
// genuine disagreement about a line has no answer derivable from the diffs.
const (
	ActionAuto   = "auto_merged"
	ActionChoose = "choose"
	ActionBoth   = "keep_both"
)

// Accounting proves no changed line went missing. Every line either merged
// cleanly or belongs to a region a step covers; Unaccounted must be empty.
type Accounting struct {
	OursChangedLines   int      `json:"ours_changed_lines"`
	TheirsChangedLines int      `json:"theirs_changed_lines"`
	AutoMergedFiles    int      `json:"auto_merged_files"`
	ConflictedLines    int      `json:"conflicted_lines"`
	StepsCoverLines    int      `json:"steps_cover_lines"`
	Unaccounted        []string `json:"unaccounted"`
	Complete           bool     `json:"complete"`
}

// Report is the whole finding, shaped for JSON so an agent can act on it
// without re-deriving anything.
type Report struct {
	Repo       string         `json:"repo"`
	Base       string         `json:"base"`
	Ours       Side           `json:"ours"`
	Theirs     Side           `json:"theirs"`
	Clean      bool           `json:"clean"`
	MergedTree string         `json:"merged_tree,omitempty"`
	AutoMerged []string       `json:"auto_merged,omitempty"`
	Conflicts  []FileConflict `json:"conflicts,omitempty"`
	Steps      []Step         `json:"steps"`
	Accounting Accounting     `json:"accounting"`
}

// Analyze merges two refs in memory and reports what has to be decided.
func Analyze(ctx context.Context, repo string, ours, theirs Side) (Report, error) {
	report := Report{Repo: repo, Ours: ours, Theirs: theirs}

	base, err := git(ctx, repo, "merge-base", ours.Ref, theirs.Ref)
	if err != nil {
		return report, fmt.Errorf("no common base for %s and %s: %w", ours.Ref, theirs.Ref, err)
	}
	report.Base = base

	tree, stages, _, err := mergeTree(ctx, repo, ours.Ref, theirs.Ref)
	if err != nil {
		return report, err
	}
	report.MergedTree = tree

	report.Accounting.OursChangedLines = changedLines(ctx, repo, base, ours.Ref)
	report.Accounting.TheirsChangedLines = changedLines(ctx, repo, base, theirs.Ref)

	if len(stages) == 0 {
		report.Clean = true
		report.AutoMerged = changedFiles(ctx, repo, base, ours.Ref, theirs.Ref)
		report.Accounting.AutoMergedFiles = len(report.AutoMerged)
		report.Steps = []Step{{
			N: 1, Action: ActionAuto, Reason: fmt.Sprintf(
				"git merged both sessions with no overlap; %d files, %d + %d changed lines, nothing to decide",
				len(report.AutoMerged), report.Accounting.OursChangedLines, report.Accounting.TheirsChangedLines),
			Lines: report.Accounting.OursChangedLines + report.Accounting.TheirsChangedLines,
		}}
		report.Accounting.StepsCoverLines = report.Steps[0].Lines
		report.Accounting.Complete = true
		return report, nil
	}

	for path, stage := range stages {
		fileConflict, err := analyzeFile(ctx, repo, path, stage)
		if err != nil {
			return report, err
		}
		report.Conflicts = append(report.Conflicts, fileConflict)
	}
	sortConflicts(report.Conflicts)

	// Files that changed on either side but are not conflicted merged cleanly,
	// and are listed so the report accounts for every touched path.
	conflicted := map[string]struct{}{}
	for path := range stages {
		conflicted[path] = struct{}{}
	}
	for _, file := range changedFiles(ctx, repo, base, ours.Ref, theirs.Ref) {
		if _, ok := conflicted[file]; !ok {
			report.AutoMerged = append(report.AutoMerged, file)
		}
	}
	report.Accounting.AutoMergedFiles = len(report.AutoMerged)
	report.Steps, report.Accounting = steps(report)
	return report, nil
}

// mergeTree runs the oracle. Exit status 1 means conflicts, which is a result
// and not a failure; anything else is a real error.
func mergeTree(ctx context.Context, repo, ours, theirs string) (string, map[string][3]string, []string, error) {
	command := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", "--messages", "-z", ours, theirs)
	command.Dir = repo
	out, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !asExit(err, &exitErr) || exitErr.ExitCode() != 1 {
			detail := ""
			if exitErr != nil {
				detail = strings.TrimSpace(string(exitErr.Stderr))
			}
			return "", nil, nil, fmt.Errorf("git merge-tree: %v: %s", err, detail)
		}
	}
	// With -z the sections are NUL separated: tree, then one entry per
	// conflicted stage, then the messages.
	fields := strings.Split(string(out), "\x00")
	if len(fields) == 0 {
		return "", nil, nil, fmt.Errorf("git merge-tree returned nothing")
	}
	tree := strings.TrimSpace(fields[0])
	stages := map[string][3]string{}
	messages := []string{}
	for _, field := range fields[1:] {
		field = strings.TrimRight(field, "\n")
		if field == "" {
			continue
		}
		// "<mode> <oid> <stage>\t<path>"
		parts := strings.SplitN(field, "\t", 2)
		if len(parts) != 2 {
			messages = append(messages, field)
			continue
		}
		meta := strings.Fields(parts[0])
		if len(meta) != 3 {
			messages = append(messages, field)
			continue
		}
		stage, err := strconv.Atoi(meta[2])
		if err != nil || stage < 1 || stage > 3 {
			continue
		}
		path := parts[1]
		entry := stages[path]
		entry[stage-1] = meta[1]
		stages[path] = entry
	}
	return tree, stages, messages, nil
}

// analyzeFile reconstructs the three versions of a conflicted path and asks
// git to merge them with markers, which is where the exact regions come from.
func analyzeFile(ctx context.Context, repo, path string, stage [3]string) (FileConflict, error) {
	fileConflict := FileConflict{
		Path: path, BaseBlob: stage[0], OursBlob: stage[1], TheirsBlob: stage[2],
	}
	dir, err := os.MkdirTemp("", "hv-conflict-*")
	if err != nil {
		return fileConflict, err
	}
	defer os.RemoveAll(dir)

	names := [3]string{"base", "ours", "theirs"}
	paths := [3]string{}
	for index, blob := range stage {
		target := filepath.Join(dir, names[index])
		paths[index] = target
		body := ""
		if blob != "" {
			if body, err = git(ctx, repo, "cat-file", "blob", blob); err != nil {
				return fileConflict, err
			}
			body += "\n"
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			return fileConflict, err
		}
	}
	// merge-file writes conflict markers with the base kept (--diff3), which
	// is what makes each region show all three versions.
	merged := runIgnoringStatus(ctx, repo, "merge-file", "-p", "--diff3",
		"-L", "ours", "-L", "base", "-L", "theirs", paths[1], paths[0], paths[2])
	fileConflict.Merged = merged
	fileConflict.Regions = parseRegions(merged)
	return fileConflict, nil
}

// parseRegions reads git's conflict markers back into structured regions.
func parseRegions(merged string) []Region {
	regions := []Region{}
	var current *Region
	section := 0 // 1 ours, 2 base, 3 theirs
	for number, line := range strings.Split(merged, "\n") {
		switch {
		case strings.HasPrefix(line, "<<<<<<<"):
			current = &Region{Index: len(regions) + 1, StartLine: number + 1}
			section = 1
		case strings.HasPrefix(line, "|||||||") && current != nil:
			section = 2
		case strings.HasPrefix(line, "=======") && current != nil:
			section = 3
		case strings.HasPrefix(line, ">>>>>>>") && current != nil:
			current.Identical = equalLines(current.Ours, current.Theirs)
			regions = append(regions, *current)
			current, section = nil, 0
		case current != nil:
			switch section {
			case 1:
				current.Ours = append(current.Ours, line)
			case 2:
				current.Base = append(current.Base, line)
			case 3:
				current.Theirs = append(current.Theirs, line)
			}
		}
	}
	return regions
}

// steps turns the findings into an ordered, exhaustive list of decisions, and
// checks that the list accounts for every conflicted line.
func steps(report Report) ([]Step, Accounting) {
	accounting := report.Accounting
	out := []Step{}
	number := 1
	if len(report.AutoMerged) > 0 {
		out = append(out, Step{
			N: number, Action: ActionAuto,
			Reason: fmt.Sprintf("%d files merged with no overlap: %s",
				len(report.AutoMerged), strings.Join(report.AutoMerged, ", ")),
		})
		number++
	}
	for _, fileConflict := range report.Conflicts {
		for _, region := range fileConflict.Regions {
			lines := len(region.Ours) + len(region.Theirs)
			accounting.ConflictedLines += lines
			action, reason := ActionChoose, fmt.Sprintf(
				"%s and %s changed the same %d lines differently; both versions are given in full",
				sideName(report.Ours), sideName(report.Theirs), maxInt(len(region.Ours), len(region.Theirs)))
			if region.Identical {
				action = ActionAuto
				reason = "both sessions made the same change here"
			} else if len(region.Base) == 0 {
				action = ActionBoth
				reason = fmt.Sprintf(
					"%s and %s each added new lines at the same place; neither replaces the other, so keep both unless they duplicate",
					sideName(report.Ours), sideName(report.Theirs))
			}
			out = append(out, Step{
				N: number, Action: action, Path: fileConflict.Path, Region: region.Index,
				Reason: reason, Ours: region.Ours, Theirs: region.Theirs, Base: region.Base,
				OursFrom: sideName(report.Ours), TheirsFrom: sideName(report.Theirs),
				Lines: lines,
			})
			accounting.StepsCoverLines += lines
			number++
		}
	}
	if accounting.StepsCoverLines != accounting.ConflictedLines {
		accounting.Unaccounted = append(accounting.Unaccounted, fmt.Sprintf(
			"%d conflicted lines are not covered by any step",
			accounting.ConflictedLines-accounting.StepsCoverLines))
	}
	accounting.Complete = len(accounting.Unaccounted) == 0
	return out, accounting
}

func sideName(side Side) string {
	if title := strings.TrimSpace(side.Title); title != "" {
		return title
	}
	if id := strings.TrimSpace(side.SessionID); id != "" {
		return id
	}
	return side.Ref
}

// changedLines counts added and removed lines between two refs.
func changedLines(ctx context.Context, repo, from, to string) int {
	out := runIgnoringStatus(ctx, repo, "diff", "--numstat", from, to)
	total := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, field := range fields[:2] {
			if count, err := strconv.Atoi(field); err == nil {
				total += count
			}
		}
	}
	return total
}

func changedFiles(ctx context.Context, repo, base string, refs ...string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, ref := range refs {
		listing := runIgnoringStatus(ctx, repo, "diff", "--name-only", base, ref)
		for _, file := range strings.Split(listing, "\n") {
			file = strings.TrimSpace(file)
			if file == "" {
				continue
			}
			if _, ok := seen[file]; ok {
				continue
			}
			seen[file] = struct{}{}
			out = append(out, file)
		}
	}
	return out
}

func equalLines(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sortConflicts(conflicts []FileConflict) {
	for outer := 1; outer < len(conflicts); outer++ {
		for inner := outer; inner > 0 && conflicts[inner].Path < conflicts[inner-1].Path; inner-- {
			conflicts[inner], conflicts[inner-1] = conflicts[inner-1], conflicts[inner]
		}
	}
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	out, err := command.Output()
	if err != nil {
		detail := ""
		var exitErr *exec.ExitError
		if asExit(err, &exitErr) {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, detail)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// runIgnoringStatus returns stdout whatever the exit status: several git
// commands report findings through the exit code rather than failure.
func runIgnoringStatus(ctx context.Context, dir string, args ...string) string {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	out, _ := command.Output()
	return string(out)
}

func asExit(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}
