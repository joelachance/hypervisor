// Command hv watches agent coding sessions, lets you select them, and merges
// the changes they produced into a demux revision plan.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/hv/desktop"
	"github.com/satoricorp/hv/launch"
	hvplan "github.com/satoricorp/hv/plan"
	"github.com/satoricorp/hv/selection"
	"github.com/satoricorp/hv/session"
	"github.com/satoricorp/hv/ui"
	"github.com/satoricorp/hv/watch"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Bare `hv` opens the browser. Every one of its actions is also a flag,
	// so nothing here is reachable only by hand.
	if len(os.Args) < 2 {
		if err := uiCmd(ctx, nil); err != nil && err != context.Canceled {
			fmt.Fprintf(os.Stderr, "hv: %v\n", err)
			os.Exit(1)
		}
		return
	}

	var err error
	switch os.Args[1] {
	case "ui":
		err = uiCmd(ctx, os.Args[2:])
	case "sessions":
		err = sessionsCmd(os.Args[2:])
	case "watch":
		err = watchCmd(ctx, os.Args[2:])
	case "merge", "plan":
		err = mergeCmd(ctx, os.Args[2:])
	case "open":
		err = openCmd(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "hv: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `hv - watch coding sessions, merge their changes into a demux plan

  hv                                            open the session browser
  hv sessions [-tool t] [-since d] [-state s]   list coding sessions
  hv watch [-interval d]                        tail session edits as they land
  hv merge [-session IDs] [-C dir] [-json]      plan the changes n sessions made
  hv open <session>                             reopen a session where it lives

With no -session, merge lists the sessions that touched this repo and asks
which to merge. Selected sessions' edits are attributed to their hunks and
demux groups, orders, and routes the result.
`)
}

// scan sweeps the log roots once and returns the watcher holding the index.
func scan(maxAge time.Duration) (*watch.Watcher, []capture.SessionEvent, error) {
	watcher := watch.New(nil, 0)
	watcher.MaxAge = maxAge
	events, err := watcher.Scan()
	return watcher, events, err
}

// sessionsOf returns the indexed sessions with the desktop app's registry
// overlaid, which is where user-chosen titles, archive state, and PRs live.
func sessionsOf(watcher *watch.Watcher) []session.Session {
	records, err := desktop.Load()
	if err != nil {
		// The registry is a bonus, never a requirement: a machine with no
		// desktop app has none, and a broken one should not hide sessions.
		fmt.Fprintf(os.Stderr, "hv: could not read the desktop session registry: %v\n", err)
		return watcher.Sessions()
	}
	return desktop.Apply(watcher.Sessions(), records)
}

// uiCmd runs the browser, then carries out whatever it was asked for. The TUI
// collects an intent and exits; planning happens on the normal path, with its
// output on a normal terminal where it can be read, scrolled, and piped.
func uiCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("ui", flag.ExitOnError)
	dir := flags.String("C", ".", "run as if started in this directory")
	since := flags.Duration("since", 7*24*time.Hour, "how far back to read session logs")
	allRepos := flags.Bool("all-repos", false, "start unfiltered rather than scoped to this repository")
	if err := flags.Parse(args); err != nil {
		return err
	}
	options := ui.Options{MaxAge: *since}
	// Being in a repository is a strong hint about what the person means; if
	// they are somewhere else entirely, the browser opens unscoped.
	if root, err := hvplan.RepoRoot(ctx, *dir); err == nil && !*allRepos {
		options.RepoRoot = root
	}
	outcome, err := ui.Run(ctx, options)
	if err != nil {
		return err
	}
	if len(outcome.Merge) == 0 {
		return nil
	}
	root := options.RepoRoot
	if root == "" {
		root, err = hvplan.RepoRoot(ctx, *dir)
		if err != nil {
			return err
		}
	}
	watcher, events, err := scan(*since)
	if err != nil {
		return err
	}
	_ = watcher
	result, err := hvplan.Run(ctx, hvplan.Request{
		RepoRoot: root,
		Sessions: outcome.Merge,
		Events:   events,
	})
	if err != nil {
		return err
	}
	printPlan(result, outcome.Merge)
	return nil
}

func sessionsCmd(args []string) error {
	flags := flag.NewFlagSet("sessions", flag.ExitOnError)
	tool := flags.String("tool", "", "restrict to one tool (claude, codex, cursor)")
	since := flags.Duration("since", 7*24*time.Hour, "only sessions that wrote within this window")
	state := flags.String("state", "", "restrict to active or stale sessions")
	repo := flags.String("repo", "", "restrict to sessions that worked in this directory")
	archived := flags.Bool("archived", false, "include sessions archived in the app")
	limit := flags.Int("limit", 25, "maximum sessions to list")
	asJSON := flags.Bool("json", false, "print sessions as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}

	watcher, _, err := scan(*since)
	if err != nil {
		return err
	}
	now := time.Now()
	filter := selection.Filter{
		Since:           now.Add(-*since).UnixMilli(),
		RepoRoot:        *repo,
		IncludeArchived: *archived,
		Limit:           *limit,
	}
	if *tool != "" {
		filter.Tools = []string{*tool}
	}
	sessions := selection.Apply(sessionsOf(watcher), filter)
	if *state != "" {
		sessions = withState(sessions, session.State(*state), now.UnixMilli())
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(sessions)
	}
	if len(sessions) == 0 {
		fmt.Printf("no coding sessions wrote a log in the last %s\n", *since)
		return nil
	}
	for _, current := range sessions {
		fmt.Printf("%-6s %-4s %-34s %-13s %3d edits  %-9s %s\n",
			current.State(now.UnixMilli()), originLabel(current), truncate(current.Label(), 34),
			current.ID[:min(13, len(current.ID))], current.EditCount,
			humanAge(now.UnixMilli()-current.LastActiveAt), landedLabel(current))
	}
	return nil
}

func watchCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("watch", flag.ExitOnError)
	interval := flags.Duration("interval", watch.DefaultInterval, "sweep interval")
	since := flags.Duration("since", time.Hour, "ignore logs untouched for longer than this")
	if err := flags.Parse(args); err != nil {
		return err
	}
	watcher := watch.New(nil, *interval)
	watcher.MaxAge = *since
	fmt.Fprintf(os.Stderr, "watching session logs every %s; ctrl-c to stop\n", *interval)
	return watcher.Run(ctx, func(event capture.SessionEvent) {
		if !event.IsEditEvent() {
			return
		}
		fmt.Printf("%s %-7s %-13s %s\n",
			time.UnixMilli(event.TS).Format("15:04:05"), event.Tool,
			shortID(event.SessionID), event.FilePath)
	})
}

func mergeCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("merge", flag.ExitOnError)
	dir := flags.String("C", ".", "run as if started in this directory")
	sessionIDs := flags.String("session", "", "session IDs or prefixes to merge (comma separated)")
	since := flags.Duration("since", 24*time.Hour, "only consider sessions active within this window")
	all := flags.Bool("all", false, "merge every candidate session without asking")
	staged := flags.Bool("staged", false, "plan staged changes only")
	intent := flags.String("intent", "", "intent prefix for proposed revisions")
	repair := flags.Bool("repair", false, "apply demux's deterministic repair hints to the plan")
	asJSON := flags.Bool("json", false, "print the full demux plan packet as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}

	root, err := hvplan.RepoRoot(ctx, *dir)
	if err != nil {
		return err
	}
	watcher, events, err := scan(*since)
	if err != nil {
		return err
	}

	sessions, err := chooseSessions(watcher, root, *sessionIDs, *since, *all)
	if err != nil {
		return err
	}

	if len(sessions) == 0 {
		// Attributing hunks to sessions nobody selected would stamp provenance
		// the person never asked for. An unattributed plan says so honestly.
		events = nil
	}
	result, err := hvplan.Run(ctx, hvplan.Request{
		RepoRoot: root,
		Staged:   *staged,
		Sessions: sessions,
		Events:   events,
		Intent:   *intent,
		Repair:   *repair,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result.Packet)
	}
	printPlan(result, sessions)
	return nil
}

// chooseSessions resolves which sessions to merge: named explicitly, taken
// wholesale, or picked interactively from the ones that worked in this repo.
func chooseSessions(watcher *watch.Watcher, root, ids string, since time.Duration, all bool) ([]session.Session, error) {
	now := time.Now()
	if named := splitIDs(ids); len(named) > 0 {
		chosen := selection.Apply(watcher.Sessions(), selection.Filter{IDs: named})
		if len(chosen) == 0 {
			return nil, fmt.Errorf("no session matches %q", ids)
		}
		return chosen, nil
	}
	candidates := selection.Apply(watcher.Sessions(), selection.Filter{
		TouchedUnder: root,
		Since:        now.Add(-since).UnixMilli(),
		EditsOnly:    true,
	})
	if len(candidates) == 0 {
		fmt.Fprintf(os.Stderr, "no sessions edited %s in the last %s; planning the tree unattributed\n", root, since)
		return nil, nil
	}
	if all {
		return candidates, nil
	}
	return selection.Interactive(os.Stdin, os.Stdout, candidates, now.UnixMilli(), func(current session.Session) error {
		target, err := launch.Plan(context.Background(), current)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s: %s\n", target.Method, target.Detail)
		return launch.Run(context.Background(), target)
	})
}

func printPlan(result hvplan.Result, sessions []session.Session) {
	proposal := result.Packet.Proposal
	fmt.Printf("plan %s  state=%s  hunks=%d  revisions=%d  sessions=%d  attributed=%.0f%%\n",
		proposal.ID, result.Packet.State, len(proposal.Hunks), len(proposal.Revisions),
		len(sessions), result.Attributed.Coverage*100)
	for _, revision := range proposal.Revisions {
		fmt.Printf("\n%-4s %-40s -> %s\n", revision.ID, revision.Intent, routeOf(revision))
		for _, file := range revision.Files {
			fmt.Printf("       %s\n", file)
		}
		if len(revision.SessionIDs) > 0 {
			fmt.Printf("       session %s (%s)\n",
				shortID(revision.SessionIDs[0]), revision.ProvenanceStatus)
		}
	}
	for _, warning := range proposal.FeasibilityWarnings {
		fmt.Printf("\n! %s\n", warning.Message)
	}
}

func withState(sessions []session.Session, want session.State, now int64) []session.Session {
	out := make([]session.Session, 0, len(sessions))
	for _, current := range sessions {
		if current.State(now) == want {
			out = append(out, current)
		}
	}
	return out
}

func routeOf(revision demux.RevisionProposal) string {
	if route := strings.TrimSpace(revision.TargetStack); route != "" {
		return route
	}
	return "(unrouted)"
}

func splitIDs(value string) []string {
	out := []string{}
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// originLabel is where the session runs, short enough for a column.
func originLabel(current session.Session) string {
	switch current.Origin {
	case session.OriginApp:
		return "app"
	case session.OriginCLI:
		return "cli"
	default:
		return current.Tool
	}
}

// landedLabel reports what became of the session's work, using the pull
// requests the desktop app already tracked. It says nothing when the app knows
// nothing - an absent PR is not evidence that the work never landed.
func landedLabel(current session.Session) string {
	if len(current.PRs) == 0 {
		if branch := strings.TrimSpace(current.Branch); branch != "" {
			return branch
		}
		return ""
	}
	states := map[string]int{}
	for _, pr := range current.PRs {
		states[strings.ToUpper(pr.State)]++
	}
	parts := []string{}
	for _, state := range []string{"MERGED", "OPEN", "CLOSED", "DRAFT"} {
		if count := states[state]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, strings.ToLower(state)))
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%d PRs", len(current.PRs))
	}
	return strings.Join(parts, ", ")
}

func openCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("open", flag.ExitOnError)
	dryRun := flags.Bool("n", false, "print the command instead of running it")
	since := flags.Duration("since", 30*24*time.Hour, "how far back to look for the session")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: hv open <session id or prefix>")
	}
	watcher, _, err := scan(*since)
	if err != nil {
		return err
	}
	matches := selection.Apply(sessionsOf(watcher), selection.Filter{
		IDs:             []string{flags.Arg(0)},
		IncludeArchived: true,
	})
	if len(matches) == 0 {
		return fmt.Errorf("no session matches %q", flags.Arg(0))
	}
	if len(matches) > 1 {
		return fmt.Errorf("%q matches %d sessions; use a longer prefix", flags.Arg(0), len(matches))
	}
	target, err := launch.Plan(ctx, matches[0])
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("%s: %s\n%s\n", target.Method, target.Detail, target)
		return nil
	}
	fmt.Fprintf(os.Stderr, "%s: %s\n", target.Method, target.Detail)
	return launch.Run(ctx, target)
}

// humanAge renders how long ago a session last wrote, which is the evidence
// behind its active/stale label.
func humanAge(millis int64) string {
	age := time.Duration(millis) * time.Millisecond
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit-1] + "\u2026"
}
