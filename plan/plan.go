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
	files := selection.Files(request.Sessions)
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
		packet, err := planner.PlanPacket(ctx, catalog)
		return Result{Packet: packet, Diff: diff}, err
	}

	attributed := request.Matcher.Link(catalog.Hunks, eventsFor(request.Sessions, request.Events))

	// Second pass: same diff, same options, now carrying the evidence. Session
	// links refine grouping and stamp provenance - they never pick a branch.
	options.Capture = attributed.Capture()
	options.SessionIDs = selection.IDs(request.Sessions)
	options.ProvenanceStatus = attributed.Status()
	proposal, err := planner.ProposeFromDiff(ctx, diff, options)
	if err != nil {
		return Result{}, err
	}
	packet, err := planner.PlanPacket(ctx, proposal)
	if err != nil {
		return Result{}, err
	}
	return Result{Packet: packet, Attributed: attributed, Diff: diff}, nil
}

// Repair converges a proposal toward ready_to_apply. With planOnly set it uses
// only demux's deterministic repair hints; otherwise demux may take one
// bounded model round, which needs OPENAI_API_KEY set.
func Repair(ctx context.Context, planner *demux.Planner, proposal demux.DemuxProposal, planOnly bool) (demux.DemuxAIReviewResult, error) {
	return planner.RepairDemuxProposal(ctx, proposal, demux.DemuxAIReviewOptions{PlanOnly: planOnly})
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
