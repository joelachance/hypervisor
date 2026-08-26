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
	"github.com/satoricorp/hv/desktop"
	"github.com/satoricorp/hv/internal/text"
	hvplan "github.com/satoricorp/hv/plan"
	"github.com/satoricorp/hv/recall"
	"github.com/satoricorp/hv/selection"
	"github.com/satoricorp/hv/session"
)

// conflictsCmd compares two branches and reports what has to be decided,
// naming the sessions behind each side.
func conflictsCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("conflicts", flag.ExitOnError)
	store := storeFlag(flags)
	dir := flags.String("C", ".", "run as if started in this directory")
	ours := flags.String("ours", "", "branch or commit on one side")
	theirs := flags.String("theirs", "", "branch or commit on the other")
	oursSession := flags.String("ours-session", "", "session behind -ours, for context")
	theirsSession := flags.String("theirs-session", "", "session behind -theirs, for context")
	asJSON := flags.Bool("json", false, "print the report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*ours) == "" || strings.TrimSpace(*theirs) == "" {
		return fmt.Errorf("usage: hv conflicts -ours <ref> -theirs <ref> [-ours-session ID] [-theirs-session ID]")
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
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	printConflicts(report)
	return nil
}

// withContext attaches what hv remembers about a session to its side of the
// comparison: the title it goes by, what it was asked to do, what it concluded.
func withContext(side conflict.Side, sessionID string, known []session.Session) conflict.Side {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return side
	}
	matches := selection.Apply(known, selection.Filter{IDs: []string{sessionID}, IncludeArchived: true})
	if len(matches) == 0 {
		side.SessionID = sessionID
		return side
	}
	current := matches[0]
	side.SessionID = current.ID
	side.Title = current.Label()
	items, err := recall.Read(current, 0)
	if err != nil {
		return side
	}
	summary := recall.Summarize(current, items)
	side.Context = &summary
	return side
}

func knownSessions(maxAge time.Duration) ([]session.Session, error) {
	watcher, _, err := scan(maxAge)
	if err != nil {
		return nil, err
	}
	records, err := desktop.Load()
	if err != nil {
		return watcher.Sessions(), nil
	}
	return desktop.Apply(watcher.Sessions(), records), nil
}

func printConflicts(report conflict.Report) {
	if report.Clean {
		fmt.Printf("clean: %s and %s merge with no overlap (%d files, %d + %d changed lines)\n",
			sideLabel(report.Ours), sideLabel(report.Theirs), len(report.AutoMerged),
			report.Accounting.OursChangedLines, report.Accounting.TheirsChangedLines)
		fmt.Printf("merged tree %s is ready to commit\n", text.Short(report.MergedTree))
		return
	}
	fmt.Printf("%d file(s) conflict between %s and %s, from base %s\n\n",
		len(report.Conflicts), sideLabel(report.Ours), sideLabel(report.Theirs), text.Short(report.Base))
	for _, step := range report.Steps {
		if step.Action == conflict.ActionAuto && step.Path == "" {
			fmt.Printf("%d. %s\n\n", step.N, step.Reason)
			continue
		}
		fmt.Printf("%d. %s  %s region %d\n   %s\n", step.N, strings.ToUpper(step.Action), step.Path, step.Region, step.Reason)
		printSide("   "+step.OursFrom+":", step.Ours)
		printSide("   "+step.TheirsFrom+":", step.Theirs)
		fmt.Println()
	}
	for _, side := range []conflict.Side{report.Ours, report.Theirs} {
		if side.Context == nil {
			continue
		}
		fmt.Printf("%s was asked: %s\n", sideLabel(side), intentOrNote(side.Context.Intent))
	}
	fmt.Printf("\n%d conflicted lines, all covered by the steps above: %v\n",
		report.Accounting.ConflictedLines, report.Accounting.Complete)
}

func printSide(label string, lines []string) {
	if len(lines) == 0 {
		fmt.Printf("%s (nothing)\n", label)
		return
	}
	fmt.Println(label)
	for _, line := range lines {
		fmt.Printf("     %s\n", line)
	}
}

// intentOrNote is what the session was asked to do, or an honest note that the
// transcript did not record one.
func intentOrNote(intent string) string {
	if strings.TrimSpace(intent) != "" {
		return intent
	}
	return "(no prompt recorded)"
}

func sideLabel(side conflict.Side) string {
	if title := strings.TrimSpace(side.Title); title != "" {
		return title
	}
	return side.Ref
}
