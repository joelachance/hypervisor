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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/hv/internal/gitx"
	"github.com/satoricorp/hv/journal"
	"github.com/satoricorp/hv/plan"
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
	// Remainder is what demux could not plan - deletions, mode changes,
	// binaries, renames - committed after the revisions on the branch the
	// last of them landed on, or on Branch when that is set.
	Remainder plan.Remainder
}

// Apply commits every revision in a plan and returns what it did.
//
// Revisions routed to the same branch become successive commits on it, in plan
// order. A branch whose base is another branch created in the same run starts
// from that branch's new tip, so a stack stays a stack. What demux could not
// plan - the remainder - goes last, on the branch the last revision landed on.
func Apply(ctx context.Context, repoRoot string, packet demux.DemuxPlanPacket, options Options) (journal.Entry, error) {
	proposal := packet.Proposal
	if len(proposal.Revisions) == 0 && options.Remainder.Empty() {
		return journal.Entry{}, fmt.Errorf("plan has no revisions")
	}
	base, err := gitx.Run(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return journal.Entry{}, err
	}
	run := &run{
		ctx: ctx, repoRoot: repoRoot, base: base, options: options,
		patches: patchIndex(proposal),
		tips:    map[string]string{}, before: map[string]string{}, renamed: map[string]string{},
	}
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
	last := ""
	for _, revision := range proposal.Revisions {
		branch, parent, err := run.place(branchOf(revision, options), startingPoint(ctx, repoRoot, revision, run.tips, base))
		if err != nil {
			return entry, err
		}
		patch := patchFor(revision, run.patches)
		if strings.TrimSpace(patch) == "" {
			return entry, fmt.Errorf("revision %s has no patch text", revision.ID)
		}
		sha, resolved, err := commitRevision(ctx, repoRoot, parent, revision, patch, options)
		entry.Resolved = append(entry.Resolved, resolved...)
		if err != nil {
			return entry, fmt.Errorf("revision %s: %w", revision.ID, err)
		}
		run.tips[branch] = sha
		last = branch
		entry.Commits = append(entry.Commits, journal.Commit{
			Revision: revision.ID,
			Branch:   branch,
			SHA:      sha,
			Subject:  subjectFor(revision),
			Files:    len(revision.Files),
		})
	}
	if !options.Remainder.Empty() {
		commit, err := run.commitRemainder(last)
		if err != nil {
			return entry, err
		}
		entry.Commits = append(entry.Commits, commit)
	}

	for from, to := range run.renamed {
		entry.Renamed = append(entry.Renamed, journal.Rename{From: from, To: to})
	}
	// A branch another worktree has checked out cannot be moved from here:
	// that worktree's index and HEAD would disagree, and git would show its
	// owner phantom changes. Refused before any ref moves, so nothing needs
	// undoing.
	if err := refuseCheckedOutElsewhere(ctx, repoRoot, run.tips); err != nil {
		return entry, err
	}
	// A detached HEAD or an unborn branch has no name, and the error is the
	// answer: no branch matches, so no index is refreshed. rev-parse spells
	// a detached HEAD as the literal word, which is not a branch either.
	current, _ := gitx.Run(ctx, repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if current == "HEAD" {
		current = ""
	}
	for _, branch := range sortedKeys(run.tips) {
		tip := run.tips[branch]
		if _, err := gitx.Run(ctx, repoRoot, "update-ref", "refs/heads/"+branch, tip); err != nil {
			// Refs that already moved are journalled before the error goes
			// out, so undo can take them back rather than losing track of
			// commits that exist on real branches.
			if len(entry.Branches) > 0 {
				if recordErr := journal.Record(ctx, repoRoot, entry); recordErr != nil {
					return entry, fmt.Errorf("branch %s: %w; and the %d branch(es) already moved could not be journalled: %v",
						branch, err, len(entry.Branches), recordErr)
				}
				return entry, fmt.Errorf("branch %s: %w; the %d branch(es) already moved are journalled - hv undo takes them back",
					branch, err, len(entry.Branches))
			}
			return entry, fmt.Errorf("branch %s: %w", branch, err)
		}
		entry.Branches = append(entry.Branches, journal.BranchChange{
			Name: branch, Before: run.before[branch], After: tip,
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

// run is the state one Apply accumulates: the branch tips built so far, what
// each extended branch was at before, and the names that had to change.
type run struct {
	ctx      context.Context
	repoRoot string
	base     string
	options  Options
	patches  map[string]string
	tips     map[string]string
	before   map[string]string
	renamed  map[string]string
}

// place decides where a commit for branch goes: the tip built so far in this
// run, the branch's own tip when it exists and descends from the base, or a
// fresh name when the branch belongs to unrelated history. It returns the
// branch actually used and the commit to build on.
func (r *run) place(branch, fallback string) (string, string, error) {
	if tip, ok := r.tips[branch]; ok {
		return branch, tip, nil
	}
	existing, err := gitx.Run(r.ctx, r.repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return branch, fallback, nil
	}
	if gitx.Succeeded(r.ctx, r.repoRoot, "merge-base", "--is-ancestor", r.base, existing) {
		// An existing branch is extended from its own tip rather than
		// rewritten from HEAD: hv adds commits, it does not discard anyone
		// else's.
		r.before[branch] = existing
		return branch, existing, nil
	}
	// The branch exists but grew from somewhere else - most often another
	// worktree of this repository that routed to the same name from a
	// different base. Its patches were computed against this base and will
	// not apply to that tip, so the work goes on a branch of its own rather
	// than failing or landing on top of unrelated commits.
	if r.options.Branch != "" {
		// An explicit target is not a suggestion. If its tip is unrelated to
		// this base, moving it would discard whatever is there.
		return "", "", fmt.Errorf(
			"branch %s is not descended from %s; commit from a worktree on that branch, or pick another",
			branch, shortSHA(r.base))
	}
	free := freeBranch(r.ctx, r.repoRoot, branch, r.base)
	r.renamed[branch] = free
	if tip, ok := r.tips[free]; ok {
		return free, tip, nil
	}
	return free, fallback, nil
}

// commitRemainder commits what demux could not plan, after the revisions and
// on the branch the last of them landed on - or the requested branch, or the
// unrouted one when there were no revisions at all.
func (r *run) commitRemainder(last string) (journal.Commit, error) {
	branch := strings.TrimSpace(r.options.Branch)
	if branch == "" {
		branch = last
	}
	if branch == "" {
		branch = branchOf(demux.RevisionProposal{}, r.options)
	}
	branch, parent, err := r.place(branch, r.base)
	if err != nil {
		return journal.Commit{}, err
	}
	revision := demux.RevisionProposal{
		ID:     "hv",
		Intent: r.options.Remainder.Summary(),
		Files:  r.options.Remainder.Files(),
	}
	// No hunk links exist for a deletion or a mode change, so the remainder
	// is credited the way an unlinked revision is: to the one session the
	// plan was scoped to, and to nobody when there were several.
	if len(r.options.Sessions) == 1 {
		revision.SessionIDs = []string{r.options.Sessions[0].ID}
	}
	sha, _, err := commitRevision(r.ctx, r.repoRoot, parent, revision, r.options.Remainder.Patch, r.options)
	if err != nil {
		return journal.Commit{}, fmt.Errorf("remainder (%s): %w", revision.Intent, err)
	}
	r.tips[branch] = sha
	return journal.Commit{
		Revision: revision.ID,
		Branch:   branch,
		SHA:      sha,
		Subject:  subjectFor(revision),
		Files:    len(revision.Files),
	}, nil
}

// refuseCheckedOutElsewhere fails when any branch about to move is checked
// out in a worktree other than this one.
func refuseCheckedOutElsewhere(ctx context.Context, repoRoot string, tips map[string]string) error {
	elsewhere, err := branchesCheckedOutElsewhere(ctx, repoRoot)
	if err != nil {
		return err
	}
	for _, branch := range sortedKeys(tips) {
		if path, ok := elsewhere[branch]; ok {
			return fmt.Errorf("branch %s is checked out in %s; run hv there, or pick another branch with -branch", branch, path)
		}
	}
	return nil
}

// branchesCheckedOutElsewhere maps each branch some other worktree of this
// repository has checked out to that worktree's path.
func branchesCheckedOutElsewhere(ctx context.Context, repoRoot string) (map[string]string, error) {
	listing, err := gitx.RunRaw(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	here := samePath(repoRoot)
	out := map[string]string{}
	path := ""
	for _, line := range strings.Split(listing, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			if samePath(path) != here {
				out[strings.TrimPrefix(line, "branch refs/heads/")] = path
			}
		}
	}
	return out, nil
}

// samePath is a path in the form two spellings of one directory share.
func samePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
