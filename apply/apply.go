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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/hv/conflict"
)

// Commit is one commit this run created.
type Commit struct {
	Revision string `json:"revision"`
	Branch   string `json:"branch"`
	SHA      string `json:"sha"`
	Subject  string `json:"subject"`
	Files    int    `json:"files"`
}

// BranchChange records a ref as it was before this run, so undo can put it
// back. Before is empty when the branch did not exist.
type BranchChange struct {
	Name   string `json:"name"`
	Before string `json:"before,omitempty"`
	After  string `json:"after"`
}

// Created reports whether this run brought the branch into existence, in which
// case undo deletes it rather than resetting it.
func (b BranchChange) Created() bool { return strings.TrimSpace(b.Before) == "" }

// Entry is one applied plan, as recorded in the repository's journal.
type Entry struct {
	ID         string         `json:"id"`
	AppliedAt  int64          `json:"applied_at"`
	Repo       string         `json:"repo"`
	ProposalID string         `json:"proposal_id,omitempty"`
	Base       string         `json:"base"`
	Commits    []Commit       `json:"commits"`
	Branches   []BranchChange `json:"branches"`
	SessionIDs []string       `json:"session_ids,omitempty"`
	Renamed    []Rename       `json:"renamed,omitempty"`
	// Resolved names the files a Resolver had to settle on the way in.
	Resolved []string `json:"resolved,omitempty"`
	UndoneAt int64    `json:"undone_at,omitempty"`
}

// Rename records a branch hv could not use under its routed name.
type Rename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Undone reports whether this entry has already been undone.
func (e Entry) Undone() bool { return e.UndoneAt != 0 }

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

// Options configure one apply run.
type Options struct {
	// Resolver, when set, is asked to settle conflicts instead of failing.
	Resolver Resolver
	// Label names the side the revision came from, for the resolver's prompt.
	Label string
	// SessionIDs are recorded on the entry and named in commit trailers.
	SessionIDs []string
	// Trailers adds "hv-session: <id>" style trailers to each message.
	Trailers map[string]string
}

// Apply commits every revision in a plan and returns what it did.
//
// Revisions routed to the same branch become successive commits on it, in plan
// order. A branch whose base is another branch created in the same run starts
// from that branch's new tip, so a stack stays a stack.
func Apply(ctx context.Context, repoRoot string, packet demux.DemuxPlanPacket, options Options) (Entry, error) {
	proposal := packet.Proposal
	if len(proposal.Revisions) == 0 {
		return Entry{}, fmt.Errorf("plan has no revisions")
	}
	base, err := git(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return Entry{}, err
	}
	patches := patchIndex(proposal)

	entry := Entry{
		ID:         newID(),
		AppliedAt:  time.Now().UnixMilli(),
		Repo:       repoRoot,
		ProposalID: proposal.ID,
		Base:       base,
		SessionIDs: append([]string(nil), options.SessionIDs...),
	}

	// Ref updates are collected and written only after every commit exists, so
	// a patch that fails to apply leaves no half-built branch behind.
	tips := map[string]string{}
	before := map[string]string{}
	// Branches that had to be renamed because the name was taken by unrelated
	// history; reported so nobody goes looking for commits under the old one.
	renamed := map[string]string{}

	for _, revision := range proposal.Revisions {
		branch := branchOf(revision)
		parent, ok := tips[branch]
		if !ok {
			parent = startingPoint(ctx, repoRoot, revision, tips, base)
			if existing, err := git(ctx, repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
				if descendsFrom(ctx, repoRoot, base, existing) {
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
					branch = freeBranch(ctx, repoRoot, branch, base)
					renamed[branchOf(revision)] = branch
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
		entry.Commits = append(entry.Commits, Commit{
			Revision: revision.ID,
			Branch:   branch,
			SHA:      sha,
			Subject:  subjectFor(revision),
			Files:    len(revision.Files),
		})
	}

	for from, to := range renamed {
		entry.Renamed = append(entry.Renamed, Rename{From: from, To: to})
	}
	for branch, tip := range tips {
		if _, err := git(ctx, repoRoot, "update-ref", "refs/heads/"+branch, tip); err != nil {
			return entry, fmt.Errorf("branch %s: %w", branch, err)
		}
		entry.Branches = append(entry.Branches, BranchChange{
			Name: branch, Before: before[branch], After: tip,
		})
	}
	if err := record(ctx, repoRoot, entry); err != nil {
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
		if existing, err := git(ctx, repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+parent); err == nil {
			return existing
		}
	}
	return base
}

// commitRevision builds one commit without touching the working tree: read the
// parent into a scratch index, apply the patch to it, write the tree, and
// commit that tree.
func commitRevision(ctx context.Context, repoRoot, parent string, revision demux.RevisionProposal, patch string, options Options) (string, []string, error) {
	indexFile, err := os.CreateTemp("", "hv-index-*")
	if err != nil {
		return "", nil, err
	}
	indexPath := indexFile.Name()
	indexFile.Close()
	os.Remove(indexPath) // git wants to create it itself
	defer os.Remove(indexPath)

	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if _, err := gitEnv(ctx, repoRoot, env, "read-tree", parent); err != nil {
		return "", nil, err
	}
	resolved := []string{}
	if err := applyPatch(ctx, repoRoot, env, patch); err != nil {
		if options.Resolver == nil {
			return "", nil, err
		}
		// The patch collides with what is already on the branch. Three-way
		// gets as far as git can on its own; whatever it leaves unmerged is
		// what the resolver is for.
		if err := applyPatchThreeWay(ctx, repoRoot, env, patch); err != nil {
			return "", nil, err
		}
		files, err := resolveUnmerged(ctx, repoRoot, env, revision, options)
		if err != nil {
			return "", nil, err
		}
		resolved = files
	}
	tree, err := gitEnv(ctx, repoRoot, env, "write-tree")
	if err != nil {
		return "", nil, err
	}
	message := messageFor(revision, options)
	if len(resolved) > 0 {
		message = strings.TrimRight(message, "\n") +
			fmt.Sprintf("\n\nhv-resolved-by: %s (%s)\n", options.Resolver.Name(), strings.Join(resolved, ", "))
	}
	sha, err := gitInput(ctx, repoRoot, env, message, "commit-tree", tree, "-p", parent, "-F", "-")
	if err != nil {
		return "", nil, err
	}
	return sha, resolved, nil
}

// applyPatchThreeWay falls back to git's own three-way merge, which needs the
// blobs the patch refers to; a patch hv produced from this repository has
// them.
func applyPatchThreeWay(ctx context.Context, repoRoot string, env []string, patch string) error {
	command := exec.CommandContext(ctx, "git", "apply", "--cached", "--3way", "--whitespace=nowarn", "-")
	command.Dir = repoRoot
	command.Env = env
	command.Stdin = strings.NewReader(patch)
	out, err := command.CombinedOutput()
	if err != nil {
		// --3way exits non-zero when it leaves conflicts, which is the case
		// the resolver handles; only a total failure has no unmerged paths.
		if !strings.Contains(string(out), "conflict") && !strings.Contains(string(out), "U ") {
			return fmt.Errorf("git apply --3way: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// resolveUnmerged settles every path git left conflicted, staging the answer.
func resolveUnmerged(ctx context.Context, repoRoot string, env []string, revision demux.RevisionProposal, options Options) ([]string, error) {
	listing, err := gitEnv(ctx, repoRoot, env, "ls-files", "-u")
	if err != nil {
		return nil, err
	}
	stages := map[string][3]string{}
	order := []string{}
	for _, line := range strings.Split(listing, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		meta := strings.Fields(parts[0])
		if len(meta) != 3 {
			continue
		}
		stage, err := strconv.Atoi(meta[2])
		if err != nil || stage < 1 || stage > 3 {
			continue
		}
		path := parts[1]
		if _, seen := stages[path]; !seen {
			order = append(order, path)
		}
		entry := stages[path]
		entry[stage-1] = meta[1]
		stages[path] = entry
	}
	if len(order) == 0 {
		return nil, nil
	}

	resolved := make([]string, 0, len(order))
	for _, path := range order {
		body, err := resolveFile(ctx, repoRoot, path, stages[path], revision, options)
		if err != nil {
			return nil, err
		}
		blob, err := hashObject(ctx, repoRoot, body)
		if err != nil {
			return nil, err
		}
		if _, err := gitEnv(ctx, repoRoot, env, "update-index", "--add", "--cacheinfo",
			fmt.Sprintf("100644,%s,%s", blob, path)); err != nil {
			return nil, err
		}
		resolved = append(resolved, path)
	}
	return resolved, nil
}

// resolveFile rebuilds one conflicted file, asking the resolver per region.
func resolveFile(ctx context.Context, repoRoot, path string, stage [3]string, revision demux.RevisionProposal, options Options) (string, error) {
	dir, err := os.MkdirTemp("", "hv-apply-conflict-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	names := [3]string{"base", "ours", "theirs"}
	paths := [3]string{}
	for index, blob := range stage {
		target := filepath.Join(dir, names[index])
		paths[index] = target
		body := ""
		if blob != "" {
			if body, err = git(ctx, repoRoot, "cat-file", "blob", blob); err != nil {
				return "", err
			}
			body += "\n"
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	merged := runIgnoringStatus(ctx, repoRoot, "merge-file", "-p", "--diff3",
		"-L", "on the branch", "-L", "base", "-L", "this revision", paths[1], paths[0], paths[2])

	regions := conflict.ParseRegions(merged)
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
	return replaceRegions(merged, replacements)
}

func labelFor(revision demux.RevisionProposal, options Options) string {
	if label := strings.TrimSpace(options.Label); label != "" {
		return label
	}
	if intent := strings.TrimSpace(revision.Intent); intent != "" {
		return intent
	}
	return revision.ID
}

// replaceRegions swaps every conflict block for its resolution in one pass.
func replaceRegions(merged string, replacements [][]string) (string, error) {
	lines := strings.Split(merged, "\n")
	out := make([]string, 0, len(lines))
	seen, inBlock := 0, false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "<<<<<<<"):
			if seen >= len(replacements) {
				return "", fmt.Errorf("more conflict blocks than resolutions")
			}
			out = append(out, replacements[seen]...)
			seen++
			inBlock = true
		case inBlock && strings.HasPrefix(line, ">>>>>>>"):
			inBlock = false
		case inBlock:
		default:
			out = append(out, line)
		}
	}
	if seen != len(replacements) {
		return "", fmt.Errorf("resolved %d regions but the file has %d", len(replacements), seen)
	}
	return strings.Join(out, "\n"), nil
}

func hashObject(ctx context.Context, repoRoot, body string) (string, error) {
	command := exec.CommandContext(ctx, "git", "hash-object", "-w", "--stdin")
	command.Dir = repoRoot
	command.Stdin = strings.NewReader(body)
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git hash-object: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// runIgnoringStatus returns stdout whatever the exit status: merge-file
// reports conflicts through the exit code rather than failing.
func runIgnoringStatus(ctx context.Context, dir string, args ...string) string {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	out, _ := command.Output()
	return string(out)
}

// applyPatch stages a patch into the scratch index. --cached keeps it out of
// the working tree; --index-info would not handle new files.
func applyPatch(ctx context.Context, repoRoot string, env []string, patch string) error {
	command := exec.CommandContext(ctx, "git", "apply", "--cached", "--whitespace=nowarn", "-")
	command.Dir = repoRoot
	command.Env = env
	command.Stdin = strings.NewReader(patch)
	out, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git apply: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Undo puts the refs back the way they were: branches this run created are
// deleted, branches it extended are reset to the commit they were at.
//
// The commits themselves are left in the object database, unreferenced, where
// git's reflog and gc grace period keep them recoverable for a while. Nothing
// in the working tree is touched, because nothing in it ever was.
func Undo(ctx context.Context, entry Entry) (Entry, error) {
	if entry.Undone() {
		return entry, fmt.Errorf("%s was already undone", entry.ID)
	}
	for _, branch := range entry.Branches {
		current, err := git(ctx, entry.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch.Name)
		if err != nil {
			// Someone deleted it first; that is the outcome undo wanted.
			continue
		}
		if current != branch.After {
			return entry, fmt.Errorf(
				"branch %s has moved since it was applied (%s, expected %s); leaving it alone",
				branch.Name, short(current), short(branch.After))
		}
		if branch.Created() {
			if _, err := git(ctx, entry.Repo, "update-ref", "-d", "refs/heads/"+branch.Name, branch.After); err != nil {
				return entry, err
			}
			continue
		}
		if _, err := git(ctx, entry.Repo, "update-ref", "refs/heads/"+branch.Name, branch.Before, branch.After); err != nil {
			return entry, err
		}
	}
	entry.UndoneAt = time.Now().UnixMilli()
	if err := rewrite(ctx, entry.Repo, entry); err != nil {
		return entry, err
	}
	return entry, nil
}

// descendsFrom reports whether tip has base in its history, which is what
// makes it safe to apply patches computed against base on top of tip.
func descendsFrom(ctx context.Context, repoRoot, base, tip string) bool {
	command := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", base, tip)
	command.Dir = repoRoot
	return command.Run() == nil
}

// freeBranch finds an unused name near the routed one, preferring a suffix
// that says where the work came from.
func freeBranch(ctx context.Context, repoRoot, branch, base string) string {
	candidate := branch + "-" + short(base)
	for suffix := 2; ; suffix++ {
		if _, err := git(ctx, repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+candidate); err != nil {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%s-%d", branch, short(base), suffix)
	}
}

func branchOf(revision demux.RevisionProposal) string {
	if branch := strings.TrimSpace(revision.TargetStack); branch != "" {
		return branch
	}
	return "hv/unrouted"
}

func subjectFor(revision demux.RevisionProposal) string {
	subject := strings.TrimSpace(revision.Intent)
	if subject == "" {
		subject = "update " + strings.Join(revision.Files, ", ")
	}
	if index := strings.IndexByte(subject, '\n'); index >= 0 {
		subject = subject[:index]
	}
	return subject
}

func messageFor(revision demux.RevisionProposal, options Options) string {
	var builder strings.Builder
	builder.WriteString(subjectFor(revision))
	builder.WriteString("\n")
	// The sessions that produced the change are recorded as trailers, so the
	// provenance survives in the commit rather than only in hv's journal.
	for _, id := range revision.SessionIDs {
		builder.WriteString("\nhv-session: " + id)
		if title := options.Trailers[id]; title != "" {
			builder.WriteString(" (" + title + ")")
		}
	}
	if len(revision.SessionIDs) > 0 {
		builder.WriteString("\n")
	}
	return builder.String()
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

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func newID() string {
	return fmt.Sprintf("hv-%d", time.Now().UnixNano())
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitEnv(ctx, dir, nil, args...)
}

func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = env
	out, err := command.Output()
	if err != nil {
		detail := ""
		var exitErr *exec.ExitError
		if ok := asExit(err, &exitErr); ok {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, detail)
	}
	return strings.TrimSpace(string(out)), nil
}

func gitInput(ctx context.Context, dir string, env []string, input string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = env
	command.Stdin = strings.NewReader(input)
	out, err := command.Output()
	if err != nil {
		detail := ""
		var exitErr *exec.ExitError
		if ok := asExit(err, &exitErr); ok {
			detail = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, detail)
	}
	return strings.TrimSpace(string(out)), nil
}

func asExit(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}

// journalPath is inside the *common* git directory, so the journal travels
// with the repository and never shows up as an untracked file in the tree hv
// is planning.
//
// The common directory matters: worktrees share one object database and one
// set of refs, so a merge run in a worktree creates branches every checkout
// can see. A journal written to the worktree's own git directory would leave
// those branches undoable from anywhere else, which is precisely where someone
// would look for them.
func journalPath(ctx context.Context, repoRoot string) (string, error) {
	dir, err := git(ctx, repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		// Older git without --path-format; the per-worktree directory is the
		// common one outside a worktree, which is the usual case.
		if dir, err = git(ctx, repoRoot, "rev-parse", "--absolute-git-dir"); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, "hv-journal.jsonl"), nil
}

// legacyJournalPath is where journals written before the common-directory fix
// live: inside the worktree's own git directory. It is read, never written.
func legacyJournalPath(ctx context.Context, repoRoot string) (string, error) {
	dir, err := git(ctx, repoRoot, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hv-journal.jsonl"), nil
}

func record(ctx context.Context, repoRoot string, entry Entry) error {
	path, err := journalPath(ctx, repoRoot)
	if err != nil {
		return err
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(body, '\n'))
	return err
}

// Journal records an entry written by something other than Apply - a resolved
// merge, for instance - so hv undo can take it back the same way.
func Journal(ctx context.Context, repoRoot string, entry Entry) error {
	return record(ctx, repoRoot, entry)
}

// NewID returns an entry id for callers building their own entries.
func NewID() string { return newID() }

// History returns the repository's applied plans, newest first.
func History(ctx context.Context, repoRoot string) ([]Entry, error) {
	path, err := journalPath(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	paths := []string{path}
	if legacy, err := legacyJournalPath(ctx, repoRoot); err == nil && legacy != path {
		paths = append(paths, legacy)
	}
	entries := []Entry{}
	for _, candidate := range paths {
		body, err := os.ReadFile(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, line := range strings.Split(string(body), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var entry Entry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				continue
			}
			entries = append(entries, entry)
		}
	}
	sort.SliceStable(entries, func(left, right int) bool {
		return entries[left].AppliedAt < entries[right].AppliedAt
	})
	// Later lines supersede earlier ones with the same id, which is how an
	// undo is recorded without rewriting history in place.
	latest := map[string]Entry{}
	order := []string{}
	for _, entry := range entries {
		if _, ok := latest[entry.ID]; !ok {
			order = append(order, entry.ID)
		}
		latest[entry.ID] = entry
	}
	out := make([]Entry, 0, len(order))
	for index := len(order) - 1; index >= 0; index-- {
		out = append(out, latest[order[index]])
	}
	return out, nil
}

// Last returns the most recent entry that has not been undone.
func Last(ctx context.Context, repoRoot string) (Entry, bool, error) {
	entries, err := History(ctx, repoRoot)
	if err != nil {
		return Entry{}, false, err
	}
	for _, entry := range entries {
		if !entry.Undone() {
			return entry, true, nil
		}
	}
	return Entry{}, false, nil
}

func rewrite(ctx context.Context, repoRoot string, entry Entry) error {
	return record(ctx, repoRoot, entry)
}
