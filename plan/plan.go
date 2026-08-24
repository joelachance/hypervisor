// Package plan hands a session-attributed working tree to the demux planner.
//
// The handoff is two-pass, and it has to be. Demux assigns hunk IDs while
// parsing the diff, and the matcher needs those exact IDs to attribute a hunk
// to a session - so hv proposes once to learn the catalog, matches against it,
// then proposes again with the evidence attached. IDs are positional over the
// diff text, so the second pass must be given a byte-identical diff and
// otherwise identical options, or the links point at the wrong hunks.
package plan

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/hv/match"
	"github.com/satoricorp/hv/selection"
	"github.com/satoricorp/hv/session"
)

// Request is one planning run over the working tree.
type Request struct {
	// RepoRoot is the worktree to plan. Required.
	RepoRoot string
	// Staged plans the index rather than the working tree.
	Staged bool
	// Sessions are the selected sessions. Their edited files scope the diff;
	// empty means plan everything, unattributed.
	Sessions []session.Session
	// Events are the session events backing those sessions, for matching.
	Events []capture.SessionEvent
	// Intent prefixes each proposed revision.
	Intent string
	// Stacks are the existing stacks revisions may be routed onto.
	Stacks []demux.StackContext
	// Matcher configures hunk attribution; the zero value uses the defaults.
	Matcher match.Matcher
	// Repair applies demux's deterministic repair hints before returning.
	// It never reaches for the model round - that stays an explicit call to
	// Repair with planOnly false.
	Repair bool
}

// Result is a plan plus the attribution that shaped it.
type Result struct {
	Packet     demux.DemuxPlanPacket
	Attributed match.Result
	Diff       string
}

// Run plans the working tree, attributing hunks to the selected sessions.
func Run(ctx context.Context, request Request) (Result, error) {
	root := strings.TrimSpace(request.RepoRoot)
	if root == "" {
		return Result{}, fmt.Errorf("plan: RepoRoot is required")
	}
	files := scopedFiles(root, selection.Files(request.Sessions))
	diff, err := WorkingDiff(ctx, root, request.Staged, files)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(diff) == "" {
		return Result{}, fmt.Errorf("plan: no changes to demux in %s", root)
	}
	head, err := HeadCommit(ctx, root)
	if err != nil {
		return Result{}, err
	}

	planner := demux.NewPlanner(request.Stacks...)
	planner.Probe = Probe{}
	options := demux.ProposeOptions{
		Intent:         request.Intent,
		RepoRoot:       root,
		SourceCommitID: head,
	}

	// First pass: learn the hunk catalog. Its IDs are what the matcher links.
	catalog, err := planner.ProposeFromDiff(ctx, diff, options)
	if err != nil {
		return Result{}, err
	}
	if len(request.Events) == 0 {
		packet, err := finish(ctx, planner, catalog, request.Repair)
		return Result{Packet: packet, Diff: diff}, err
	}

	attributed := request.Matcher.Link(catalog.Hunks, eventsFor(request.Sessions, request.Events))

	// Second pass: same diff, same options, now carrying the evidence. Session
	// links refine grouping and stamp provenance - they never pick a branch.
	options.Capture = attributed.Capture()
	options.ProvenanceStatus = attributed.Status()
	options.SessionIDs = fallbackSessionIDs(request.Sessions)
	proposal, err := planner.ProposeFromDiff(ctx, diff, options)
	if err != nil {
		return Result{}, err
	}
	packet, err := finish(ctx, planner, proposal, request.Repair)
	if err != nil {
		return Result{}, err
	}
	return Result{Packet: packet, Attributed: attributed, Diff: diff}, nil
}

// finish reviews a proposal, optionally converging it with demux's
// deterministic repair hints first.
func finish(ctx context.Context, planner *demux.Planner, proposal demux.DemuxProposal, repair bool) (demux.DemuxPlanPacket, error) {
	if repair {
		repaired, err := Repair(ctx, planner, proposal, true)
		// A repair that cannot reach ready_to_apply still returns its best
		// proposal alongside the error; keeping it beats discarding the work.
		if err == nil || repaired.Proposal.ID != "" {
			proposal = repaired.Proposal
		}
	}
	return planner.PlanPacket(ctx, proposal)
}

// Repair converges a proposal toward ready_to_apply. With planOnly set it uses
// only demux's deterministic repair hints; otherwise demux may take one
// bounded model round, which needs OPENAI_API_KEY set.
func Repair(ctx context.Context, planner *demux.Planner, proposal demux.DemuxProposal, planOnly bool) (demux.DemuxAIReviewResult, error) {
	return planner.RepairDemuxProposal(ctx, proposal, demux.DemuxAIReviewOptions{PlanOnly: planOnly})
}

// fallbackSessionIDs is the provenance demux stamps on a revision no hunk link
// claims.
//
// Only one session may be named there. demux stamps from links only when they
// are exact or fuzzy - a temporal link is too weak to assert authorship - and
// hv's matcher currently produces temporal links alone, so nearly every
// revision takes this path. Listing every selected session there would credit
// all of them with all of the work: merge three sessions and each revision
// claims all three, which reads as fact and is wrong.
//
// One session is different: the person named it, and everything in the plan is
// scoped to what it touched. Beyond one, hv says nothing rather than guessing.
func fallbackSessionIDs(sessions []session.Session) []string {
	if len(sessions) != 1 {
		return nil
	}
	return selection.IDs(sessions)
}

// scopedFiles keeps the selected sessions' files that live in this repo and
// makes them repo-relative.
//
// A session edits wherever it likes - other checkouts, worktrees, scratch
// directories - and git refuses a pathspec outside the repository, so the
// files have to be narrowed to this tree before they can scope a diff. An
// empty result means the session did work, just not here, and the caller
// plans the whole tree rather than nothing.
func scopedFiles(root string, files []string) []string {
	root = strings.TrimSuffix(root, "/")
	out := make([]string, 0, len(files))
	for _, file := range files {
		switch {
		case !filepath.IsAbs(file):
			out = append(out, file)
		case strings.HasPrefix(file, root+"/"):
			out = append(out, strings.TrimPrefix(file, root+"/"))
		}
	}
	return out
}

// eventsFor keeps only the events belonging to the selected sessions. An empty
// selection means no filtering: everything captured is fair evidence.
func eventsFor(sessions []session.Session, events []capture.SessionEvent) []capture.SessionEvent {
	if len(sessions) == 0 {
		return events
	}
	wanted := map[string]struct{}{}
	for _, current := range sessions {
		wanted[current.ID] = struct{}{}
	}
	out := make([]capture.SessionEvent, 0, len(events))
	for _, event := range events {
		if _, ok := wanted[event.SessionID]; ok {
			out = append(out, event)
		}
	}
	return out
}

// RepoOf resolves the repository a session's work belongs to.
//
// A session's recorded directory is where the agent was started, which is not
// always the repository: it may be a parent directory, or a worktree whose
// root differs from the checkout it was launched from. The files it actually
// edited are the better evidence, so they are asked first and the recorded
// directory is the fallback.
func RepoOf(ctx context.Context, current session.Session) (string, error) {
	for _, file := range current.Files {
		if !filepath.IsAbs(file) {
			continue
		}
		if root, err := RepoRoot(ctx, filepath.Dir(file)); err == nil {
			return root, nil
		}
	}
	if root := strings.TrimSpace(current.RepoRoot); root != "" {
		return RepoRoot(ctx, root)
	}
	return "", fmt.Errorf("session %s is not in a repository", current.ID)
}

// GroupByRepo sorts sessions into the repositories their work lives in,
// returning the roots in the order they were first seen.
//
// Selecting across repositories is normal for hv - it watches every session on
// the machine, not the one directory it was started in - and a plan is
// necessarily per repository, so the caller runs one per group.
func GroupByRepo(ctx context.Context, sessions []session.Session) ([]string, map[string][]session.Session, []error) {
	order := []string{}
	groups := map[string][]session.Session{}
	problems := []error{}
	for _, current := range sessions {
		root, err := RepoOf(ctx, current)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if _, ok := groups[root]; !ok {
			order = append(order, root)
		}
		groups[root] = append(groups[root], current)
	}
	return order, groups, problems
}
