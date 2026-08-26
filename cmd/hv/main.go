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
	"path/filepath"
	"strings"
	"time"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/demux/matcher"
	"github.com/satoricorp/hv/apply"
	"github.com/satoricorp/hv/desktop"
	"github.com/satoricorp/hv/launch"
	"github.com/satoricorp/hv/parse"
	hvplan "github.com/satoricorp/hv/plan"
	"github.com/satoricorp/hv/resolve"
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
	case "undo":
		err = undoCmd(ctx, os.Args[2:])
	case "conflicts":
		err = conflictsCmd(ctx, os.Args[2:])
	case "resolve":
		err = resolveCmd(ctx, os.Args[2:])
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

  hv [-repo]                                    open the session browser
  hv -store DIR                                 read a different session store
  hv sessions [-tool t] [-since d] [-state s]   list coding sessions
  hv watch [-interval d]                        tail session edits as they land
  hv merge [-session IDs] [-C dir]              commit the changes n sessions made,
                                                asking a harness to settle any
                                                conflict (-no-resolve to refuse)
  hv merge -dry-run                             …show the plan without committing
  hv open <session>                             reopen a session where it lives
  hv undo [-C dir] [-list]                      undo the last merge
  hv conflicts -ours REF -theirs REF [-json]    what two sessions disagree about
  hv resolve -ours REF -theirs REF              have a coding agent fix it and
                                                commit the merge (-harness pi|fx|
                                                claude|codex|opencode|<binary>)

With no -session, merge lists the sessions that touched this repo and asks
which to merge. Their edits are attributed to hunks, grouped into revisions,
routed onto branches, and committed. Nothing is checked out and the working
tree is left exactly as it was; hv undo takes the commits back.
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
// storeFlag registers -store, which points hv at a session store other than
// the real one. It exists so trying hv against a synthetic store needs no
// wrapper script and no environment variable - the two things that made it
// possible to run the wrong binary and conclude hv was broken.
func storeFlag(flags *flag.FlagSet) *string {
	return flags.String("store", "",
		"read session logs from this directory instead of your home directory")
}

func applyStore(store string) {
	if strings.TrimSpace(store) != "" {
		parse.SetHome(store)
	}
	if !parse.IsDefaultHome() {
		fmt.Fprintf(os.Stderr, "hv: reading sessions from %s\n", parse.Home())
	}
}

func uiCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("ui", flag.ExitOnError)
	store := storeFlag(flags)
	dir := flags.String("C", ".", "run as if started in this directory")
	since := flags.Duration("since", 7*24*time.Hour, "how far back to read session logs")
	scoped := flags.Bool("repo", false, "start narrowed to this repository instead of showing every session")
	if err := flags.Parse(args); err != nil {
		return err
	}
	applyStore(*store)
	options := ui.Options{MaxAge: *since, ScopeToRepo: *scoped}
	// The repository is remembered so the scope key has something to narrow
	// to, not to filter by: hv watches every session on the machine, and a
	// browser that hid the others would be lying about what it is.
	if root, err := hvplan.RepoRoot(ctx, *dir); err == nil {
		options.RepoRoot = root
	}
	outcome, err := ui.Run(ctx, options)
	if err != nil {
		return err
	}
	if len(outcome.Merge) == 0 {
		return nil
	}
	_, events, err := scan(*since)
	if err != nil {
		return err
	}
	return planPerRepo(ctx, outcome.Merge, events, false, nil)
}

// planPerRepo plans each repository the chosen sessions worked in.
//
// A plan is per repository by construction - it is a diff of one worktree -
// but a selection is not: hv lists every session on the machine, so picking
// three of them can reach three different repositories. Each gets its own
// plan rather than one being silently chosen.
// conflictResolver picks the harness that settles conflicts hit while
// committing. Merging should finish; a patch that collides with what is
// already on the branch is exactly the case an agent can decide.
func conflictResolver(name string, disabled bool) apply.Resolver {
	if disabled {
		return nil
	}
	harness, err := harnessFor(name, "", "")
	if err != nil {
		// No harness is not an error until a conflict actually happens, and
		// then apply says so plainly.
		return nil
	}
	return resolve.Adapter{Harness: harness}
}

func resolverName(resolver apply.Resolver) string {
	if resolver == nil {
		return "nobody"
	}
	return resolver.Name()
}

func planPerRepo(ctx context.Context, sessions []session.Session, events []capture.SessionEvent, commit bool, resolver apply.Resolver) error {
	order, groups, problems := hvplan.GroupByRepo(ctx, sessions)
	for _, problem := range problems {
		fmt.Fprintf(os.Stderr, "hv: %v\n", problem)
	}
	for index, root := range order {
		if len(order) > 1 {
			if index > 0 {
				fmt.Println()
			}
			fmt.Printf("=== %s ===\n", root)
		}
		result, err := hvplan.Run(ctx, hvplan.Request{
			RepoRoot: root,
			Sessions: groups[root],
			Events:   events,
		})
		if err != nil {
			// One repository with nothing to plan must not lose the others.
			fmt.Fprintf(os.Stderr, "hv: %s: %v\n", root, err)
			continue
		}
		printPlan(result, groups[root])
		if !commit {
			continue
		}
		if err := commitPlan(ctx, root, result, groups[root], resolver); err != nil {
			// One repository failing to commit must not stop the others; the
			// ones that succeeded each printed their own undo command.
			fmt.Fprintf(os.Stderr, "hv: %s: %v\n", root, err)
		}
	}
	if !commit {
		fmt.Fprintln(os.Stderr, "\ndry run: nothing was committed")
	}
	return nil
}

func sessionsCmd(args []string) error {
	flags := flag.NewFlagSet("sessions", flag.ExitOnError)
	store := storeFlag(flags)
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

	applyStore(*store)
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
	short := session.Abbreviate(sessions, 8)
	for _, current := range sessions {
		fmt.Printf("%-6s %-3s %-12s %-30s %-13s %3d edits  %-9s %s\n",
			current.State(now.UnixMilli()), originLabel(current),
			truncate(repoName(current), 12), truncate(current.Label(), 30),
			short[current.ID], current.EditCount,
			humanAge(now.UnixMilli()-current.LastActiveAt), landedLabel(current))
	}
	return nil
}

func watchCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("watch", flag.ExitOnError)
	store := storeFlag(flags)
	interval := flags.Duration("interval", watch.DefaultInterval, "sweep interval")
	since := flags.Duration("since", time.Hour, "ignore logs untouched for longer than this")
	if err := flags.Parse(args); err != nil {
		return err
	}
	applyStore(*store)
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
	store := storeFlag(flags)
	dir := flags.String("C", ".", "run as if started in this directory")
	sessionIDs := flags.String("session", "", "session IDs or prefixes to merge (comma separated)")
	since := flags.Duration("since", 24*time.Hour, "only consider sessions active within this window")
	all := flags.Bool("all", false, "merge every candidate session without asking")
	staged := flags.Bool("staged", false, "plan staged changes only")
	intent := flags.String("intent", "", "intent prefix for proposed revisions")
	repair := flags.Bool("repair", false, "apply demux's deterministic repair hints to the plan")
	dryRun := flags.Bool("dry-run", false, "show the plan without committing anything")
	harnessName := flags.String("harness", "", "coding harness to settle conflicts: pi, fx, claude, codex, opencode, or a binary")
	noResolve := flags.Bool("no-resolve", false, "fail on a conflict instead of asking a harness to settle it")
	asJSON := flags.Bool("json", false, "print the full demux plan packet as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}

	applyStore(*store)
	watcher, events, err := scan(*since)
	if err != nil {
		return err
	}

	// The repository is only needed for the paths that plan here: naming
	// sessions by id plans each one where it lives, and hv is routinely run
	// from outside any repository at all.
	named := splitIDs(*sessionIDs)
	root := ""
	if len(named) == 0 {
		if root, err = hvplan.RepoRoot(ctx, *dir); err != nil {
			return err
		}
	}

	sessions, cancelled, err := chooseSessions(watcher, root, *sessionIDs, *since, *all)
	if err != nil {
		return err
	}
	if cancelled {
		// The picker was shown and nothing was chosen. That is an answer, not
		// an invitation to plan the whole tree on their behalf.
		fmt.Fprintln(os.Stderr, "nothing selected")
		return nil
	}

	if len(sessions) == 0 {
		// Attributing hunks to sessions nobody selected would stamp provenance
		// the person never asked for. An unattributed plan says so honestly.
		events = nil
	}
	// Sessions named by id can come from anywhere - hv watches the whole
	// machine - so they are planned in their own repositories rather than
	// forced into whichever directory hv happens to be standing in.
	// merge commits. Reviewing without committing is what -dry-run is for, and
	// -json is an inspection flag, so neither writes anything.
	commit := !*dryRun && !*asJSON
	resolver := conflictResolver(*harnessName, *noResolve)
	if len(named) > 0 {
		return planPerRepo(ctx, sessions, events, commit, resolver)
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
	if commit {
		return commitPlan(ctx, root, result, sessions, resolver)
	}
	fmt.Fprintln(os.Stderr, "\ndry run: nothing was committed")
	return nil
}

// commitPlan writes a plan to the repository and reports what it wrote.
func commitPlan(ctx context.Context, root string, result hvplan.Result, sessions []session.Session, resolver apply.Resolver) error {
	entry, err := apply.Apply(ctx, root, result.Packet, apply.Options{
		SessionIDs: selection.IDs(sessions),
		Trailers:   sessionTitles(sessions),
		Resolver:   resolver,
	})
	if err != nil {
		return err
	}
	fmt.Println()
	for _, commit := range entry.Commits {
		fmt.Printf("  %s  %-24s %s\n", commit.SHA[:8], commit.Branch, commit.Subject)
	}
	created, extended := 0, 0
	for _, branch := range entry.Branches {
		if branch.Created() {
			created++
		} else {
			extended++
		}
	}
	for _, path := range entry.Resolved {
		fmt.Printf("\n  %s conflicted with the branch; settled by %s\n", path, resolverName(resolver))
	}
	for _, rename := range entry.Renamed {
		fmt.Printf("\n  %s was taken by unrelated history; used %s instead\n", rename.From, rename.To)
	}
	fmt.Printf("\n%d commits on %d branches (%d new, %d extended); working tree untouched\n",
		len(entry.Commits), len(entry.Branches), created, extended)
	fmt.Printf("undo with: hv undo -C %s\n", root)
	return nil
}

func sessionTitles(sessions []session.Session) map[string]string {
	titles := map[string]string{}
	for _, current := range sessions {
		titles[current.ID] = current.Label()
	}
	return titles
}

func undoCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("undo", flag.ExitOnError)
	dir := flags.String("C", ".", "run as if started in this directory")
	list := flags.Bool("list", false, "list applied plans instead of undoing one")
	id := flags.String("id", "", "undo this entry rather than the most recent")
	if err := flags.Parse(args); err != nil {
		return err
	}
	root, err := hvplan.RepoRoot(ctx, *dir)
	if err != nil {
		return err
	}
	history, err := apply.History(ctx, root)
	if err != nil {
		return err
	}
	if *list {
		if len(history) == 0 {
			fmt.Println("no plans have been applied here")
			return nil
		}
		for _, entry := range history {
			state := "applied"
			if entry.Undone() {
				state = "undone"
			}
			fmt.Printf("%-24s %-8s %d commits  %s\n", entry.ID, state, len(entry.Commits),
				time.UnixMilli(entry.AppliedAt).Format("01-02 15:04:05"))
			for _, commit := range entry.Commits {
				fmt.Printf("    %s  %-24s %s\n", commit.SHA[:8], commit.Branch, commit.Subject)
			}
		}
		return nil
	}

	entry, found := apply.Entry{}, false
	if strings.TrimSpace(*id) != "" {
		for _, candidate := range history {
			if candidate.ID == *id {
				entry, found = candidate, true
			}
		}
		if !found {
			return fmt.Errorf("no applied plan %q in %s", *id, root)
		}
	} else if entry, found, err = apply.Last(ctx, root); err != nil {
		return err
	} else if !found {
		return fmt.Errorf("nothing to undo in %s", root)
	}

	undone, err := apply.Undo(ctx, entry)
	if err != nil {
		return err
	}
	for _, branch := range undone.Branches {
		if branch.Created() {
			fmt.Printf("  deleted %s\n", branch.Name)
			continue
		}
		fmt.Printf("  reset %s to %s\n", branch.Name, branch.Before[:8])
	}
	fmt.Printf("\nundid %d commits; working tree untouched\n", len(undone.Commits))
	return nil
}

// chooseSessions resolves which sessions to merge: named explicitly, taken
// wholesale, or picked interactively from the ones that worked in this repo.
// chooseSessions resolves the sessions to merge. The second return reports
// that a person was asked and declined, which is different from there being
// nothing to ask about.
func chooseSessions(watcher *watch.Watcher, root, ids string, since time.Duration, all bool) ([]session.Session, bool, error) {
	now := time.Now()
	if named := splitIDs(ids); len(named) > 0 {
		// Naming a session is intent: an archived one is still what was asked
		// for, so the archive filter does not apply here.
		chosen := selection.Apply(sessionsOf(watcher), selection.Filter{IDs: named, IncludeArchived: true})
		if len(chosen) == 0 {
			return nil, false, fmt.Errorf("no session matches %q", ids)
		}
		return chosen, false, nil
	}
	candidates := selection.Apply(sessionsOf(watcher), selection.Filter{
		TouchedUnder: root,
		Since:        now.Add(-since).UnixMilli(),
		EditsOnly:    true,
	})
	if len(candidates) == 0 {
		fmt.Fprintf(os.Stderr, "no sessions edited %s in the last %s; planning the tree unattributed\n", root, since)
		return nil, false, nil
	}
	if all {
		return candidates, false, nil
	}
	// The picker prints to stderr: stdout is the plan, and a prompt written
	// there corrupts -json for anything downstream.
	chosen, err := selection.Interactive(os.Stdin, os.Stderr, candidates, now.UnixMilli(), func(current session.Session) error {
		target, err := launch.Plan(context.Background(), current)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s: %s\n", target.Method, target.Detail)
		return launch.Run(context.Background(), target)
	})
	return chosen, len(chosen) == 0, err
}

func printPlan(result hvplan.Result, sessions []session.Session) {
	proposal := result.Packet.Proposal
	short := session.Abbreviate(sessions, 8)
	fmt.Printf("plan %s  state=%s  hunks=%d  revisions=%d  sessions=%d  attributed=%.0f%%\n",
		proposal.ID, result.Packet.State, len(proposal.Hunks), len(proposal.Revisions),
		len(sessions), result.Attributed.Coverage*100)
	for _, revision := range proposal.Revisions {
		fmt.Printf("\n%-4s %-40s -> %s\n", revision.ID, revision.Intent, routeOf(revision))
		for _, file := range revision.Files {
			fmt.Printf("       %s\n", file)
		}
		// Attribution comes from hv's own links rather than the revision's
		// provenance field: demux stamps that field only from exact or fuzzy
		// links, and hv's matcher produces temporal ones, so the field is
		// usually empty even when every hunk is accounted for. A revision can
		// also draw on several sessions - demux pairs a test with its source,
		// and those are routinely different sessions.
		if contributors := revisionSessions(revision, result.Attributed.Links); len(contributors) > 0 {
			names := make([]string, 0, len(contributors))
			for _, id := range contributors {
				names = append(names, sessionName(id, short, sessions))
			}
			fmt.Printf("       from %s\n", strings.Join(names, ", "))
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

// revisionSessions returns the sessions whose edits landed in a revision, in
// the order their hunks appear.
func revisionSessions(revision demux.RevisionProposal, links []matcher.HunkLink) []string {
	if len(links) == 0 {
		return nil
	}
	bySession := map[string]string{}
	for _, link := range links {
		bySession[link.HunkID] = link.SessionID
	}
	ids := revision.HunkIDs
	if len(ids) == 0 {
		for _, hunk := range revision.Hunks {
			ids = append(ids, hunk.ID)
		}
	}
	seen := map[string]struct{}{}
	out := []string{}
	for _, id := range ids {
		sessionID := bySession[id]
		if sessionID == "" {
			continue
		}
		if _, ok := seen[sessionID]; ok {
			continue
		}
		seen[sessionID] = struct{}{}
		out = append(out, sessionID)
	}
	return out
}

// sessionName labels a session id with the title it goes by, so a plan reads
// in the same terms as the picker that produced it.
func sessionName(id string, short map[string]string, sessions []session.Session) string {
	abbreviated, ok := short[id]
	if !ok {
		abbreviated = shortID(id)
	}
	for _, current := range sessions {
		if current.ID == id && strings.TrimSpace(current.Title) != "" {
			return fmt.Sprintf("%s (%s)", current.Title, abbreviated)
		}
	}
	return abbreviated
}

func plural(word string, count int) string {
	if count == 1 {
		return word
	}
	return word + "s"
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
		// Codex and Cursor record no entrypoint; name the tool instead, cut to
		// the same width so the column stays a column.
		if len(current.Tool) > 3 {
			return current.Tool[:3]
		}
		return current.Tool
	}
}

// repoName is the last path element of a session's directory - hv lists every
// session on the machine, so a title alone does not say where the work is.
func repoName(current session.Session) string {
	root := strings.TrimSpace(current.RepoRoot)
	if root == "" {
		return ""
	}
	return filepath.Base(root)
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
	store := storeFlag(flags)
	dryRun := flags.Bool("n", false, "print the command instead of running it")
	since := flags.Duration("since", 30*24*time.Hour, "how far back to look for the session")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: hv open <session id or prefix>")
	}
	applyStore(*store)
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
