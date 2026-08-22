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
	hvplan "github.com/satoricorp/hv/plan"
	"github.com/satoricorp/hv/selection"
	"github.com/satoricorp/hv/session"
	"github.com/satoricorp/hv/watch"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "sessions":
		err = sessionsCmd(os.Args[2:])
	case "watch":
		err = watchCmd(ctx, os.Args[2:])
	case "merge", "plan":
		err = mergeCmd(ctx, os.Args[2:])
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

  hv sessions [-tool t] [-since d] [-state s]   list coding sessions
  hv watch [-interval d]                        tail session edits as they land
  hv merge [-session IDs] [-C dir] [-json]      plan the changes n sessions made

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

func sessionsCmd(args []string) error {
	flags := flag.NewFlagSet("sessions", flag.ExitOnError)
	tool := flags.String("tool", "", "restrict to one tool (claude, codex, cursor)")
	since := flags.Duration("since", 24*time.Hour, "only sessions active within this window")
	state := flags.String("state", "", "restrict to live, idle, or ended sessions")
	repo := flags.String("repo", "", "restrict to sessions that worked in this directory")
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
		Since:    now.Add(-*since).UnixMilli(),
		RepoRoot: *repo,
		Limit:    *limit,
	}
	if *tool != "" {
		filter.Tools = []string{*tool}
	}
	sessions := selection.Apply(watcher.Sessions(), filter)
	if *state != "" {
		sessions = withState(sessions, session.State(*state), now.UnixMilli())
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(sessions)
	}
	if len(sessions) == 0 {
		fmt.Printf("no coding sessions active in the last %s\n", *since)
		return nil
	}
	for _, current := range sessions {
		fmt.Printf("%-5s %-7s %-13s %3d edits  %-19s %s\n",
			current.State(now.UnixMilli()), current.Tool, shortID(current.ID), current.EditCount,
			time.UnixMilli(current.LastActiveAt).Format("01-02 15:04:05"),
			firstNonEmpty(current.Summary, current.RepoRoot))
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
	return selection.Interactive(os.Stdin, os.Stdout, candidates, now.UnixMilli())
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
