package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/satoricorp/hv/conflict"
	hvplan "github.com/satoricorp/hv/plan"
	"github.com/satoricorp/hv/resolve"
)

// resolveCmd finds what two sessions disagree about, has fx fix it, and
// commits the result as a real merge.
func resolveCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("resolve", flag.ExitOnError)
	store := storeFlag(flags)
	dir := flags.String("C", ".", "run as if started in this directory")
	ours := flags.String("ours", "", "branch or commit on one side")
	theirs := flags.String("theirs", "", "branch or commit on the other")
	oursSession := flags.String("ours-session", "", "session behind -ours, for context")
	theirsSession := flags.String("theirs-session", "", "session behind -theirs, for context")
	branch := flags.String("branch", "", "branch for the merge (default hv/merge-<ours>-<theirs>)")
	model := flags.String("model", "", "model for fx to use")
	binary := flags.String("fx", "fx", "the fx binary")
	dryRun := flags.Bool("dry-run", false, "report the conflicts without asking fx or committing")
	asJSON := flags.Bool("json", false, "print the outcome as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*ours) == "" || strings.TrimSpace(*theirs) == "" {
		return fmt.Errorf("usage: hv resolve -ours <ref> -theirs <ref>")
	}
	applyStore(*store)
	root, err := hvplan.RepoRoot(ctx, *dir)
	if err != nil {
		return err
	}

	left := conflict.Side{Ref: *ours}
	right := conflict.Side{Ref: *theirs}
	if strings.TrimSpace(*oursSession) != "" || strings.TrimSpace(*theirsSession) != "" {
		known, err := knownSessions(7 * 24 * time.Hour)
		if err != nil {
			return err
		}
		left = withContext(left, *oursSession, known)
		right = withContext(right, *theirsSession, known)
	}

	report, err := conflict.Analyze(ctx, root, left, right)
	if err != nil {
		return err
	}
	if report.Clean {
		fmt.Printf("nothing to resolve: %s and %s merge cleanly (%d files)\n",
			sideLabel(report.Ours), sideLabel(report.Theirs), len(report.AutoMerged))
		return nil
	}
	if *dryRun {
		printConflicts(report)
		fmt.Fprintln(os.Stderr, "\ndry run: fx was not asked and nothing was committed")
		return nil
	}

	target := strings.TrimSpace(*branch)
	if target == "" {
		target = "hv/merge-" + slug(*ours) + "-" + slug(*theirs)
	}
	runner := resolve.Fx{Binary: *binary, Dir: root, Model: *model}

	fmt.Fprintf(os.Stderr, "%d file(s) conflict; asking %s to resolve %d region(s)…\n",
		len(report.Conflicts), runner.Name(), regionCount(report))

	outcome, err := resolve.Merge(ctx, root, report, runner, target)
	if err != nil {
		for _, problem := range outcome.Problems {
			fmt.Fprintf(os.Stderr, "  %s\n", problem)
		}
		return err
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(outcome)
	}
	printOutcome(outcome)
	return nil
}

// printOutcome is the callout: what conflicted, what was decided and why, and
// where the merge landed.
func printOutcome(outcome resolve.Outcome) {
	report := outcome.Report
	fmt.Printf("resolved %s ← %s\n\n", sideLabel(report.Ours), sideLabel(report.Theirs))
	for _, fixed := range outcome.Fixed {
		fmt.Printf("%s  (%d region%s)\n", fixed.Path, fixed.Regions, plural("", fixed.Regions))
		for index, resolution := range fixed.Resolutions {
			fmt.Printf("  region %d: %s\n", index+1, resolution.Reasoning)
			for _, line := range resolution.Resolved {
				fmt.Printf("    %s\n", line)
			}
			for _, dropped := range resolution.Dropped {
				fmt.Printf("    dropped: %s\n", dropped)
			}
		}
		fmt.Println()
	}
	if len(report.AutoMerged) > 0 {
		fmt.Printf("%d file(s) merged with no conflict: %s\n\n",
			len(report.AutoMerged), strings.Join(report.AutoMerged, ", "))
	}
	fmt.Printf("  %s  %s\n", short(outcome.Commit), outcome.Branch)
	fmt.Printf("  merge of %s and %s, resolved by %s\n",
		short(outcome.Parents[0]), short(outcome.Parents[1]), outcome.Runner)
	fmt.Printf("\nevery conflicted line is accounted for; working tree untouched\n")
	fmt.Printf("undo with: hv undo -C %s\n", outcome.Repo)
}

func regionCount(report conflict.Report) int {
	total := 0
	for _, fileConflict := range report.Conflicts {
		total += len(fileConflict.Regions)
	}
	return total
}

func slug(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.ReplaceAll(ref, "/", "-")
	if len(ref) > 24 {
		ref = ref[:24]
	}
	return ref
}
