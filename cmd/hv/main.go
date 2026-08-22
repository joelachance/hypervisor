// Command hv watches agent coding sessions, lets you select them, and plans
// the changes they produced with demux.
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
		err = sessionsCmd(ctx, os.Args[2:])
	case "watch":
		err = watchCmd(ctx, os.Args[2:])
	case "plan":
		err = planCmd(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && !isInterrupt(err) {
		fmt.Fprintf(os.Stderr, "hv: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `hv - watch coding sessions, plan their changes with demux

  hv sessions [-tool t] [-since d] [-json]   list known coding sessions
  hv watch [-interval d]                     tail session logs as they land
  hv plan -session ID [-staged] [-json]      plan the changes a session made
`)
}

func sessionsCmd(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("sessions", flag.ExitOnError)
	tool := flags.String("tool", "", "restrict to one tool (claude, codex, cursor)")
	since := flags.Duration("since", 24*time.Hour, "only sessions active within this window")
	limit := flags.Int("limit", 20, "maximum sessions to list")
	asJSON := flags.Bool("json", false, "print sessions as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}

	watcher := watch.New(nil, 0)
	if _, err := watcher.Scan(); err != nil {
		return err
	}
	filter := selection.Filter{
		Since:     time.Now().Add(-*since).UnixMilli(),
		EditsOnly: true,
		Limit:     *limit,
	}
	if *tool != "" {
		filter.Tools = []string{*tool}
	}
	sessions := selection.Apply(watcher.Sessions(), filter)
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(sessions)
	}
	if len(sessions) == 0 {
		fmt.Println("no coding sessions found")
		fmt.Println("(hv ships parser stubs; no tool's log format is decoded yet)")
		return nil
	}
	for _, current := range sessions {
		fmt.Printf("%-12s %-7s %3d edits  %-19s %s\n",
			shortID(current.ID), current.Tool, current.EditCount,
			time.UnixMilli(current.LastActiveAt).Format("2006-01-02 15:04:05"),
			strings.Join(current.Files, " "))
	}
	return nil
}

func watchCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("watch", flag.ExitOnError)
	interval := flags.Duration("interval", watch.DefaultInterval, "sweep interval")
	if err := flags.Parse(args); err != nil {
		return err
	}
	watcher := watch.New(nil, *interval)
	fmt.Fprintf(os.Stderr, "watching session logs every %s; ctrl-c to stop\n", *interval)
	return watcher.Run(ctx, func(event capture.SessionEvent) {
		if !event.IsEditEvent() {
			return
		}
		fmt.Printf("%s %-12s %s\n",
			time.UnixMilli(event.TS).Format("15:04:05"), shortID(event.SessionID), event.FilePath)
	})
}

func planCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("plan", flag.ExitOnError)
	dir := flags.String("C", ".", "run as if started in this directory")
	sessionID := flags.String("session", "", "session ID or prefix to plan (repeatable as a comma list)")
	staged := flags.Bool("staged", false, "plan staged changes only")
	intent := flags.String("intent", "", "intent prefix for proposed revisions")
	asJSON := flags.Bool("json", false, "print the full demux plan packet as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}

	root, err := hvplan.RepoRoot(ctx, *dir)
	if err != nil {
		return err
	}

	watcher := watch.New(nil, 0)
	events, err := watcher.Scan()
	if err != nil {
		return err
	}
	var sessions []session.Session
	if ids := splitIDs(*sessionID); len(ids) > 0 {
		sessions = selection.Apply(watcher.Sessions(), selection.Filter{IDs: ids})
		if len(sessions) == 0 {
			return fmt.Errorf("no session matches %q", *sessionID)
		}
	}

	result, err := hvplan.Run(ctx, hvplan.Request{
		RepoRoot: root,
		Staged:   *staged,
		Sessions: sessions,
		Events:   events,
		Intent:   *intent,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result.Packet)
	}
	printPlan(result)
	return nil
}

func printPlan(result hvplan.Result) {
	proposal := result.Packet.Proposal
	fmt.Printf("plan %s  state=%s  hunks=%d  revisions=%d  attributed=%.0f%%\n",
		proposal.ID, result.Packet.State, len(proposal.Hunks), len(proposal.Revisions),
		result.Attributed.Coverage*100)
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

func isInterrupt(err error) bool {
	return err == context.Canceled
}
