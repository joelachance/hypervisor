// Package merge is the one path from chosen sessions to commits.
//
// The browser and the command line both used to do this themselves: read the
// transcripts, work out who wrote which revision, call the planner, call the
// committer, interpret the result. Two copies meant two places to fix, and the
// browser silently lost its commit messages' Why paragraph for exactly that
// reason. There is one copy now; the frontends only decide what to say about
// the outcome.
package merge

import (
	"context"
	"fmt"
	"strings"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/demux/matcher"

	"github.com/satoricorp/hv/apply"
	"github.com/satoricorp/hv/journal"
	hvplan "github.com/satoricorp/hv/plan"
	"github.com/satoricorp/hv/recall"
	"github.com/satoricorp/hv/session"
)

// Request is one merge: these sessions' work, in this repository.
type Request struct {
	RepoRoot string
	Sessions []session.Session
	Events   []capture.SessionEvent
	// Branch commits everything to one branch instead of the branches demux
	// proposes. "HEAD" means the branch the worktree has checked out.
	Branch string
	// Staged plans the index rather than the working tree.
	Staged bool
	// Intent prefixes each proposed revision.
	Intent string
	// Repair applies demux's deterministic repair hints before committing.
	Repair bool
	// Resolver settles a conflict with the branch instead of failing.
	Resolver apply.Resolver
}

// Result is what happened: the plan, and the entry if it was committed.
type Result struct {
	Plan      hvplan.Result
	Entry     journal.Entry
	Committed bool
}

// Plan works out what the merge would do, without writing anything.
func Plan(ctx context.Context, request Request) (hvplan.Result, error) {
	return hvplan.Run(ctx, hvplan.Request{
		RepoRoot: request.RepoRoot,
		Staged:   request.Staged,
		Sessions: request.Sessions,
		Events:   request.Events,
		Intent:   request.Intent,
		Repair:   request.Repair,
	})
}

// Commit plans and then commits.
func Commit(ctx context.Context, request Request) (Result, error) {
	planned, err := Plan(ctx, request)
	if err != nil {
		return Result{}, err
	}
	return CommitPlanned(ctx, request, planned)
}

// CommitPlanned commits a plan the caller already has, which is what the
// browser has by the time someone presses enter on it.
func CommitPlanned(ctx context.Context, request Request, planned hvplan.Result) (Result, error) {
	branch := strings.TrimSpace(request.Branch)
	// HEAD means "the branch I am on", which is the common case for someone
	// who wants the work committed where they are standing rather than parked
	// on a name they will have to go find.
	if strings.EqualFold(branch, "HEAD") {
		current, err := hvplan.CurrentBranch(ctx, request.RepoRoot)
		// rev-parse spells a detached HEAD as the literal word, and a branch
		// by that name would be created in its honour.
		if err != nil || current == "HEAD" {
			return Result{Plan: planned}, fmt.Errorf("HEAD is not on a branch; name one with -branch")
		}
		branch = current
	}
	entry, err := apply.Apply(ctx, request.RepoRoot, planned.Packet, apply.Options{
		Sessions:         SessionInfo(request.Sessions),
		RevisionSessions: Attribution(planned),
		Branch:           branch,
		Resolver:         request.Resolver,
		Remainder:        planned.Remainder,
	})
	if err != nil {
		return Result{Plan: planned, Entry: entry}, err
	}
	return Result{Plan: planned, Entry: entry, Committed: true}, nil
}

// SessionInfo reads each session's transcript for the facts a commit message
// needs: what it was asked to do, and what it reported doing.
func SessionInfo(sessions []session.Session) []apply.SessionInfo {
	out := make([]apply.SessionInfo, 0, len(sessions))
	for _, current := range sessions {
		info := apply.SessionInfo{ID: current.ID, Title: current.Label()}
		if items, err := recall.Read(current, 0); err == nil {
			summary := recall.Summarize(current, items)
			info.Intent, info.Outcome = summary.Intent, summary.Outcome
		}
		out = append(out, info)
	}
	return out
}

// Attribution maps each revision to the sessions whose edits are in it, from
// hv's own hunk links.
//
// Not from the proposal's provenance field: demux fills that only from exact
// and fuzzy links and leaves it empty more often than not, and a commit
// missing its Why paragraph over a bookkeeping gap helps nobody.
func Attribution(planned hvplan.Result) map[string][]string {
	out := map[string][]string{}
	for _, revision := range planned.Packet.Proposal.Revisions {
		if ids := RevisionSessions(revision, planned.Attributed.Links); len(ids) > 0 {
			out[revision.ID] = ids
		}
	}
	return out
}

// RevisionSessions is who wrote one revision, in the order their hunks appear.
func RevisionSessions(revision demux.RevisionProposal, links []matcher.HunkLink) []string {
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

// ByRepo groups sessions by the repository their work lives in. A selection
// can span repositories; a plan cannot.
func ByRepo(ctx context.Context, sessions []session.Session) ([]string, map[string][]session.Session, []error) {
	return hvplan.GroupByRepo(ctx, sessions)
}
