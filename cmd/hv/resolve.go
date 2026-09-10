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
	"github.com/satoricorp/hv/internal/text"
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
	model := flags.String("model", "", "model for the harness to use")
	harnessName := flags.String("harness", "", "coding harness: pi, fx, claude, codex, opencode, or a binary path")
	harnessArgs := flags.String("harness-args", "", "argument template for a custom harness; {prompt} is the question")
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
		fmt.Fprintln(os.Stderr, "\ndry run: no harness was asked and nothing was committed")
		return nil
	}

	target := strings.TrimSpace(*branch)
	if target == "" {
		target = "hv/merge-" + slug(*ours) + "-" + slug(*theirs)
	}
	runner, err := harnessFor(*harnessName, *harnessArgs, *model)
	if err != nil {
		return err
	}

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
		fmt.Printf("%s  (%d region%s)\n", fixed.Path, fixed.Regions, text.Plural("s", fixed.Regions))
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
	fmt.Printf("  %s  %s\n", text.Short(outcome.Commit), outcome.Branch)
	fmt.Printf("  merge of %s and %s, resolved by %s\n",
		text.Short(outcome.Parents[0]), text.Short(outcome.Parents[1]), outcome.Runner)
	fmt.Printf("\nevery conflicted line is accounted for; working tree untouched\n")
	fmt.Printf("undo with: hv undo -C %s\n", outcome.Repo)
}

// harnessFor resolves which coding agent to ask. Named harnesses come with a
// recipe; anything else is treated as a binary plus an argument template, so
// hv can drive a harness it has never heard of.
func harnessFor(name, args, model string) (resolve.Harness, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(os.Getenv("HV_HARNESS"))
	}
	if strings.TrimSpace(args) == "" {
		args = strings.TrimSpace(os.Getenv("HV_HARNESS_ARGS"))
	}
	if name == "" {
		harness, ok := resolve.Detect()
		if !ok {
			return resolve.Harness{}, fmt.Errorf(
				"no coding harness found on PATH; install one of %s or pass -harness <binary> -harness-args '<template with {prompt}>'",
				strings.Join(harnessNames(), ", "))
		}
		harness.Model = model
		return harness, nil
	}
	if harness, ok := resolve.Builtin(name); ok {
		harness.Model = model
		return harness, nil
	}
	if strings.TrimSpace(args) == "" {
		return resolve.Harness{}, fmt.Errorf(
			"unknown harness %q; known: %s. For anything else pass -harness-args with {prompt} in it",
			name, strings.Join(harnessNames(), ", "))
	}
	harness := resolve.Custom(name, strings.Fields(args))
	harness.Model = model
	return harness, nil
}

func harnessNames() []string {
	names := []string{}
	for _, harness := range resolve.Builtins() {
		names = append(names, harness.Label)
	}
	return names
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
