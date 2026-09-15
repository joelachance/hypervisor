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
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/satoricorp/hv/internal/gitx"
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
type Region = gitx.Region

// FileConflict is one path the merge could not settle.
type FileConflict struct {
	Path       string   `json:"path"`
	BaseBlob   string   `json:"base_blob,omitempty"`
	OursBlob   string   `json:"ours_blob,omitempty"`
	TheirsBlob string   `json:"theirs_blob,omitempty"`
	Regions    []Region `json:"regions"`
	// Merged is the file with conflict markers, exactly as git produced it.
	Merged string `json:"merged,omitempty"`
	// Binary marks a file git cannot show line by line. Problem says why a
	// file has no regions to decide about, when it has none.
	Binary  bool   `json:"binary,omitempty"`
	Problem string `json:"problem,omitempty"`
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

	base, err := gitx.Run(ctx, repo, "merge-base", ours.Ref, theirs.Ref)
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
	// merge-tree exits 1 when it finds conflicts, which is a result and not a
	// failure, so the output is read whatever the status.
	out := gitx.Output(ctx, repo, "merge-tree", "--write-tree", "--messages", "-z", ours, theirs)
	if strings.TrimSpace(out) == "" {
		return "", nil, nil, fmt.Errorf("git merge-tree said nothing about %s and %s", ours, theirs)
	}
	// With -z the sections are NUL separated: tree, then one entry per
	// conflicted stage, then the messages.
	fields := strings.Split(out, "\x00")
	tree := strings.TrimSpace(fields[0])
	stages := map[string][3]string{}
	messages := []string{}
	for _, field := range fields[1:] {
		field = strings.TrimRight(field, "\n")
		if field == "" {
			continue
		}
		path, stage, blob, ok := parseStageLine(field)
		if !ok {
			messages = append(messages, field)
			continue
		}
		entry := stages[path]
		entry[stage-1] = blob
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
	merged, err := gitx.MergeFile(ctx, repo, stage[0], stage[1], stage[2],
		[3]string{"base", "ours", "theirs"})
	if errors.Is(err, gitx.ErrBinary) {
		// A whole-file decision: git can say the sides differ and nothing
		// more, so no line-level accounting is possible here.
		fileConflict.Binary = true
		fileConflict.Problem = "binary file; the two versions cannot be shown line by line"
		return fileConflict, nil
	}
	if err != nil {
		return fileConflict, err
	}
	fileConflict.Merged = merged
	fileConflict.Regions = gitx.ParseRegions(merged)
	if len(fileConflict.Regions) == 0 {
		fileConflict.Problem = "git reports a conflict but the contents merge; a mode, type, or rename disagreement"
	}
	return fileConflict, nil
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
		// The figure the steps are checked against comes from the marked-up
		// text itself, not from the regions the steps are built from - a
		// count derived from the same loop would prove nothing.
		accounting.ConflictedLines += gitx.ConflictedLines(fileConflict.Merged)
		if len(fileConflict.Regions) == 0 {
			// Nothing to decide line by line, which is itself the finding:
			// the file needs a whole-file decision, and the accounting says
			// so rather than pretending the file is covered.
			out = append(out, Step{
				N: number, Action: ActionChoose, Path: fileConflict.Path,
				Reason: fmt.Sprintf("%s and %s both changed this file, and it has to be taken whole: %s",
					sideName(report.Ours), sideName(report.Theirs), fileConflict.Problem),
				OursFrom: sideName(report.Ours), TheirsFrom: sideName(report.Theirs),
			})
			accounting.Unaccounted = append(accounting.Unaccounted,
				fmt.Sprintf("%s: %s", fileConflict.Path, fileConflict.Problem))
			number++
			continue
		}
		for _, region := range fileConflict.Regions {
			lines := len(region.Ours) + len(region.Theirs)
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
	out := gitx.Output(ctx, repo, "diff", "--numstat", from, to)
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
		listing := gitx.Output(ctx, repo, "diff", "--name-only", base, ref)
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

// parseStageLine reads one "<mode> <oid> <stage>\t<path>" entry, which is how
// merge-tree and ls-files -u both describe a conflicted path.
func parseStageLine(field string) (path string, stage int, blob string, ok bool) {
	parts := strings.SplitN(field, "\t", 2)
	if len(parts) != 2 {
		return "", 0, "", false
	}
	meta := strings.Fields(parts[0])
	if len(meta) != 3 {
		return "", 0, "", false
	}
	stage, err := strconv.Atoi(meta[2])
	if err != nil || stage < 1 || stage > 3 {
		return "", 0, "", false
	}
	return parts[1], stage, meta[1], true
}
