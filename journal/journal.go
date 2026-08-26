// Package journal records what hv committed, so it can be taken back.
//
// One file per repository, in the *common* git directory: worktrees share one
// object database and one set of refs, so a merge run in a worktree creates
// branches every checkout can see, and a journal written to the worktree's own
// git directory would leave them undoable from anywhere else.
package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/satoricorp/hv/internal/gitx"
)

// Commit is one commit an entry created.
type Commit struct {
	Revision string `json:"revision"`
	Branch   string `json:"branch"`
	SHA      string `json:"sha"`
	Subject  string `json:"subject"`
	Files    int    `json:"files"`
}

// BranchChange records a ref as it was before, so undo can put it back. Before
// is empty when the branch did not exist.
type BranchChange struct {
	Name   string `json:"name"`
	Before string `json:"before,omitempty"`
	After  string `json:"after"`
}

// Created reports whether this entry brought the branch into existence, in
// which case undo deletes it rather than resetting it.
func (b BranchChange) Created() bool { return strings.TrimSpace(b.Before) == "" }

// Rename records a branch hv could not use under its routed name.
type Rename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Entry is one applied plan.
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
	// Resolved names the files a resolver had to settle on the way in.
	Resolved []string `json:"resolved,omitempty"`
	// CheckedOut is set when the commits landed on the branch the worktree has
	// checked out, which means the index was refreshed too.
	CheckedOut string `json:"checked_out,omitempty"`
	UndoneAt   int64  `json:"undone_at,omitempty"`
}

// Undone reports whether this entry has already been undone.
func (e Entry) Undone() bool { return e.UndoneAt != 0 }

// NewID returns an id for a fresh entry.
func NewID() string { return fmt.Sprintf("hv-%d", time.Now().UnixNano()) }

// Record appends an entry. A later line supersedes an earlier one with the
// same id, which is how an undo is written without rewriting history.
func Record(ctx context.Context, repoRoot string, entry Entry) error {
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

// History returns the repository's entries, newest first.
func History(ctx context.Context, repoRoot string) ([]Entry, error) {
	path, err := journalPath(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	paths := []string{path}
	if legacy, err := legacyPath(ctx, repoRoot); err == nil && legacy != path {
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

// Undo puts the refs back: branches the entry created are deleted, branches it
// extended are reset to the commit they were at.
//
// The commits themselves are left in the object database, unreferenced, where
// the reflog keeps them recoverable. Nothing in the working tree is touched,
// because nothing in it ever was.
func Undo(ctx context.Context, entry Entry) (Entry, error) {
	if entry.Undone() {
		return entry, fmt.Errorf("%s was already undone", entry.ID)
	}
	for _, branch := range entry.Branches {
		ref := "refs/heads/" + branch.Name
		current, err := gitx.Run(ctx, entry.Repo, "rev-parse", "--verify", "--quiet", ref)
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
			if _, err := gitx.Run(ctx, entry.Repo, "update-ref", "-d", ref, branch.After); err != nil {
				return entry, err
			}
			continue
		}
		if _, err := gitx.Run(ctx, entry.Repo, "update-ref", ref, branch.Before, branch.After); err != nil {
			return entry, err
		}
		// If the commits landed on the checked-out branch, the index was
		// pointed at them; putting the ref back has to put the index back too,
		// or the work would look staged for deletion.
		if entry.CheckedOut == branch.Name {
			if _, err := gitx.Run(ctx, entry.Repo, "reset", "--mixed", "--quiet", branch.Before); err != nil {
				return entry, err
			}
		}
	}
	entry.UndoneAt = time.Now().UnixMilli()
	if err := Record(ctx, entry.Repo, entry); err != nil {
		return entry, err
	}
	return entry, nil
}

// journalPath is inside the common git directory, so one repository has one
// journal however many worktrees it has, and it never appears as an untracked
// file in the tree hv is planning.
func journalPath(ctx context.Context, repoRoot string) (string, error) {
	dir, err := gitx.Run(ctx, repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		// Older git without --path-format; outside a worktree the per-worktree
		// directory is the common one, which is the usual case.
		if dir, err = gitx.Run(ctx, repoRoot, "rev-parse", "--absolute-git-dir"); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, "hv-journal.jsonl"), nil
}

// legacyPath is where journals written before the common-directory fix live.
// It is read, never written.
func legacyPath(ctx context.Context, repoRoot string) (string, error) {
	dir, err := gitx.Run(ctx, repoRoot, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hv-journal.jsonl"), nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
