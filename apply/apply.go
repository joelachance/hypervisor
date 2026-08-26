// Package apply turns a demux plan into real branches and commits.
//
// It never checks anything out. Each revision's patch is applied to a
// temporary index, written as a tree, and committed with git commit-tree; the
// branch ref is moved last. The working tree and the real index are not
// touched at any point, which has three consequences worth relying on:
//
//   - the changes stay in the working tree afterwards, exactly as they were,
//     so nothing is lost if the commits turn out wrong;
//   - a dirty tree is not an obstacle, and it always is one here - the whole
//     point is committing part of a mixed working tree;
//   - undo is only ref surgery, because refs are the only thing that changed.
package apply

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/hv/internal/gitx"
	"github.com/satoricorp/hv/journal"
)

// Resolver settles a conflict that arises while committing. It exists so
// merging can finish rather than stopping at "does not apply": a patch that
// collides with a branch's existing work is exactly the case a coding agent
// can decide, and hv already knows which sessions wrote both sides.
type Resolver interface {
	ResolveConflict(ctx context.Context, request ConflictRequest) ([]string, error)
	Name() string
}

// ConflictRequest is one region a Resolver has to settle.
type ConflictRequest struct {
	Path       string
	Region     int
	Base       []string
	Ours       []string
	Theirs     []string
	OursFrom   string
	TheirsFrom string
}

// SessionInfo is everything a commit message needs about one session.
type SessionInfo struct {
	ID    string
	Title string
	// Intent is what the person asked for - the reason a diff can never
	// supply. Outcome is what the session reported doing.
	Intent  string
	Outcome string
}

// Options configure one apply run.
type Options struct {
	// Resolver, when set, is asked to settle conflicts instead of failing.
	Resolver Resolver
	// Label names the side the revision came from, for the resolver's prompt.
	Label string
	// RevisionSessions maps revision id to the sessions whose edits are in it.
	// It overrides the proposal's own provenance, which demux fills only from
	// exact and fuzzy links and leaves empty more often than not - hv's
	// matcher knows, and a commit with no Why paragraph because of a
	// bookkeeping gap helps nobody.
	RevisionSessions map[string][]string
	// Branch, when set, is the branch every revision is committed to, in plan
	// order, instead of the branches demux proposed. It is how someone says
	// "put this work where I actually want it" rather than accepting a name
	// invented from a file path.
	Branch string
	// Sessions are the sessions behind this plan: who they are, what they were
	// asked for, and what they reported. One list rather than three maps keyed
	// by id, which could be half-populated and silently drop a paragraph.
	Sessions []SessionInfo
}

// Apply commits every revision in a plan and returns what it did.
//
// Revisions routed to the same branch become successive commits on it, in plan
// order. A branch whose base is another branch created in the same run starts
// from that branch's new tip, so a stack stays a stack.
func Apply(ctx context.Context, repoRoot string, packet demux.DemuxPlanPacket, options Options) (journal.Entry, error) {
	proposal := packet.Proposal
	if len(proposal.Revisions) == 0 {
		return journal.Entry{}, fmt.Errorf("plan has no revisions")
	}
	base, err := gitx.Run(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return journal.Entry{}, err
	}
	patches := patchIndex(proposal)

	entry := journal.Entry{
		ID:         journal.NewID(),
		AppliedAt:  time.Now().UnixMilli(),
		Repo:       repoRoot,
		ProposalID: proposal.ID,
		Base:       base,
		SessionIDs: sessionIDs(options.Sessions),
	}

	// Ref updates are collected and written only after every commit exists, so
	// a patch that fails to apply leaves no half-built branch behind.
	tips := map[string]string{}
	before := map[string]string{}
	// Branches that had to be renamed because the name was taken by unrelated
	// history; reported so nobody goes looking for commits under the old one.
	renamed := map[string]string{}

	for _, revision := range proposal.Revisions {
		branch := branchOf(revision, options)
		parent, ok := tips[branch]
		if !ok {
			parent = startingPoint(ctx, repoRoot, revision, tips, base)
			if existing, err := gitx.Run(ctx, repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
				if gitx.Succeeded(ctx, repoRoot, "merge-base", "--is-ancestor", base, existing) {
					// An existing branch is extended from its own tip rather
					// than rewritten from HEAD: hv adds commits, it does not
					// discard anyone else's.
					before[branch] = existing
					parent = existing
				} else {
					// The branch exists but grew from somewhere else - most
					// often another worktree of this repository that routed to
					// the same name from a different base. Its patches were
					// computed against this base and will not apply to that
					// tip, so the work goes on a branch of its own rather than
					// failing or landing on top of unrelated commits.
					if options.Branch != "" {
						// An explicit target is not a suggestion. If its tip
						// is unrelated to this base, moving it would discard
						// whatever is there.
						return entry, fmt.Errorf(
							"branch %s is not descended from %s; commit from a worktree on that branch, or pick another",
							branch, shortSHA(base))
					}
					branch = freeBranch(ctx, repoRoot, branch, base)
					renamed[branchOf(revision, options)] = branch
					if tip, ok := tips[branch]; ok {
						parent = tip
					}
				}
			}
		}
		patch := patchFor(revision, patches)
		if strings.TrimSpace(patch) == "" {
			return entry, fmt.Errorf("revision %s has no patch text", revision.ID)
		}
		sha, resolved, err := commitRevision(ctx, repoRoot, parent, revision, patch, options)
		entry.Resolved = append(entry.Resolved, resolved...)
		if err != nil {
			return entry, fmt.Errorf("revision %s: %w", revision.ID, err)
		}
		tips[branch] = sha
		entry.Commits = append(entry.Commits, journal.Commit{
			Revision: revision.ID,
			Branch:   branch,
			SHA:      sha,
			Subject:  subjectFor(revision),
			Files:    len(revision.Files),
		})
	}

	for from, to := range renamed {
		entry.Renamed = append(entry.Renamed, journal.Rename{From: from, To: to})
	}
	// A detached HEAD or an unborn branch has no name, and the error is the
	// answer: no branch matches, so no index is refreshed.
	current, _ := gitx.Run(ctx, repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	for branch, tip := range tips {
		if _, err := gitx.Run(ctx, repoRoot, "update-ref", "refs/heads/"+branch, tip); err != nil {
			return entry, fmt.Errorf("branch %s: %w", branch, err)
		}
		entry.Branches = append(entry.Branches, journal.BranchChange{
			Name: branch, Before: before[branch], After: tip,
		})
		// Committing to the branch you are standing on has to finish the job:
		// the ref moved, so the index now disagrees with HEAD and git would
		// report the work as changes that undo it. A mixed reset points the
		// index at the new commit and leaves every file alone.
		if branch == current {
			if _, err := gitx.Run(ctx, repoRoot, "reset", "--mixed", "--quiet", tip); err != nil {
				return entry, fmt.Errorf("refreshing the index on %s: %w", branch, err)
			}
			entry.CheckedOut = branch
		}
	}
	if err := journal.Record(ctx, repoRoot, entry); err != nil {
		return entry, err
	}
	return entry, nil
}

// startingPoint is the commit a new branch begins from: the tip of the branch
// it declares as its base when that branch was built in this run, otherwise
// the repository's HEAD.
func startingPoint(ctx context.Context, repoRoot string, revision demux.RevisionProposal, tips map[string]string, base string) string {
	if parent := strings.TrimSpace(revision.BaseStack); parent != "" {
		if tip, ok := tips[parent]; ok {
			return tip
		}
		if existing, err := gitx.Run(ctx, repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+parent); err == nil {
			return existing
		}
	}
	return base
}

// commitRevision builds one commit without touching the working tree: read the
// parent into a scratch index, apply the patch to it, write the tree, and
// commit that tree.
func commitRevision(ctx context.Context, repoRoot, parent string, revision demux.RevisionProposal, patch string, options Options) (sha string, resolved []string, err error) {
	err = gitx.ScratchIndex(func(env []string) error {
		sha, resolved, err = buildCommit(ctx, repoRoot, parent, revision, patch, options, env)
		return err
	})
	return sha, resolved, err
}

// buildCommit does the work inside a scratch index: read the parent, stage the
// patch, write the tree, commit it.
func buildCommit(ctx context.Context, repoRoot, parent string, revision demux.RevisionProposal, patch string, options Options, env []string) (string, []string, error) {
	if _, err := gitx.RunIn(ctx, repoRoot, env, "read-tree", parent); err != nil {
		return "", nil, err
	}
	resolved := []string{}
	if err := applyPatch(ctx, repoRoot, env, patch); err != nil {
		if options.Resolver == nil {
			return "", nil, err
		}
		// The patch collides with what is already on the branch. Three-way
		// gets as far as git can on its own; whatever it leaves unmerged is
		// what the resolver is for. When three-way cannot start either - it
		// needs the blobs the patch names, which a patch for a file that did
		// not exist does not have - the first error is the useful one.
		direct := err
		if err := applyPatchThreeWay(ctx, repoRoot, env, patch); err != nil {
			return "", nil, fmt.Errorf("%w (three-way could not help either: %v)", direct, err)
		}
		files, err := resolveUnmerged(ctx, repoRoot, env, revision, options)
		if err != nil {
			return "", nil, err
		}
		resolved = files
	}
	tree, err := gitx.RunIn(ctx, repoRoot, env, "write-tree")
	if err != nil {
		return "", nil, err
	}
	message := messageFor(revision, patch, options)
	if len(resolved) > 0 {
		message = strings.TrimRight(message, "\n") +
			fmt.Sprintf("\n\nhv-resolved-by: %s (%s)\n", options.Resolver.Name(), strings.Join(resolved, ", "))
	}
	sha, err := gitx.CommitTree(ctx, repoRoot, tree, []string{parent}, message, env)
	if err != nil {
		return "", nil, err
	}
	return sha, resolved, nil
}

// applyPatch stages a patch into the scratch index. --cached keeps it out of
// the working tree.
func applyPatch(ctx context.Context, repoRoot string, env []string, patch string) error {
	_, err := gitx.RunInput(ctx, repoRoot, env, patch,
		"apply", "--cached", "--whitespace=nowarn", "-")
	return err
}

// applyPatchThreeWay falls back to git's own three-way merge, which needs the
// blobs the patch refers to; a patch hv produced from this repository has
// them.
func applyPatchThreeWay(ctx context.Context, repoRoot string, env []string, patch string) error {
	// --3way exits non-zero when it leaves conflicts, which is the case the
	// resolver handles, so the status alone cannot say whether it worked: a
	// run that left unmerged paths did, and one that left none did not.
	_, err := gitx.RunInput(ctx, repoRoot, env, patch,
		"apply", "--cached", "--3way", "--whitespace=nowarn", "-")
	if err == nil {
		return nil
	}
	order, _, listErr := gitx.UnmergedStages(ctx, repoRoot, env)
	if listErr == nil && len(order) > 0 {
		return nil
	}
	return err
}

// resolveUnmerged settles every path git left conflicted, staging the answer.
func resolveUnmerged(ctx context.Context, repoRoot string, env []string, revision demux.RevisionProposal, options Options) ([]string, error) {
	order, stages, err := gitx.UnmergedStages(ctx, repoRoot, env)
	if err != nil || len(order) == 0 {
		return nil, err
	}
	resolved := make([]string, 0, len(order))
	for _, path := range order {
		body, err := resolveFile(ctx, repoRoot, path, stages[path], revision, options)
		if err != nil {
			return nil, err
		}
		blob, err := gitx.HashObject(ctx, repoRoot, body)
		if err != nil {
			return nil, err
		}
		if _, err := gitx.RunIn(ctx, repoRoot, env, "update-index", "--add", "--cacheinfo",
			fmt.Sprintf("100644,%s,%s", blob, path)); err != nil {
			return nil, err
		}
		resolved = append(resolved, path)
	}
	return resolved, nil
}

// resolveFile rebuilds one conflicted file, asking the resolver per region.
func resolveFile(ctx context.Context, repoRoot, path string, stage [3]string, revision demux.RevisionProposal, options Options) (string, error) {
	merged, err := gitx.MergeFile(ctx, repoRoot, stage[0], stage[1], stage[2],
		[3]string{"base", "on the branch", "this revision"})
	if err != nil {
		return "", err
	}
	regions := gitx.ParseRegions(merged)
	if len(regions) == 0 {
		return merged, nil
	}
	replacements := make([][]string, 0, len(regions))
	for _, region := range regions {
		lines, err := options.Resolver.ResolveConflict(ctx, ConflictRequest{
			Path: path, Region: region.Index,
			Base: region.Base, Ours: region.Ours, Theirs: region.Theirs,
			OursFrom:   "the work already on the branch",
			TheirsFrom: labelFor(revision, options),
		})
		if err != nil {
			return "", fmt.Errorf("%s region %d: %w", path, region.Index, err)
		}
		replacements = append(replacements, lines)
	}
	return gitx.ReplaceRegions(merged, replacements)
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func patchIndex(proposal demux.DemuxProposal) map[string]string {
	patches := make(map[string]string, len(proposal.Hunks))
	for _, hunk := range proposal.Hunks {
		patches[hunk.ID] = hunk.Patch
	}
	return patches
}

func patchFor(revision demux.RevisionProposal, patches map[string]string) string {
	ids := revision.HunkIDs
	if len(ids) == 0 {
		for _, hunk := range revision.Hunks {
			ids = append(ids, hunk.ID)
		}
	}
	var builder strings.Builder
	for _, id := range ids {
		patch, ok := patches[id]
		if !ok {
			continue
		}
		builder.WriteString(patch)
		if !strings.HasSuffix(patch, "\n") {
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

func branchOf(revision demux.RevisionProposal, options Options) string {
	if branch := strings.TrimSpace(options.Branch); branch != "" {
		return branch
	}
	if branch := strings.TrimSpace(revision.TargetStack); branch != "" {
		return branch
	}
	return "hv/unrouted"
}

// freeBranch finds an unused name near the routed one, preferring a suffix
// that says where the work came from.
func freeBranch(ctx context.Context, repoRoot, branch, base string) string {
	candidate := branch + "-" + shortSHA(base)
	for suffix := 2; ; suffix++ {
		if _, err := gitx.Run(ctx, repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+candidate); err != nil {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%s-%d", branch, shortSHA(base), suffix)
	}
}

// sessionIDs is the ids of the sessions behind a plan, for the journal.
func sessionIDs(sessions []SessionInfo) []string {
	out := make([]string, 0, len(sessions))
	for _, current := range sessions {
		out = append(out, current.ID)
	}
	return out
}

// infoFor looks up one session, so the message code never indexes a map that
// might not have the key.
func (o Options) infoFor(id string) (SessionInfo, bool) {
	for _, current := range o.Sessions {
		if current.ID == id {
			return current, true
		}
	}
	return SessionInfo{}, false
}
