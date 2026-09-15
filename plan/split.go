package plan

import (
	"fmt"
	"sort"
	"strings"
)

// Remainder is the part of a working diff demux cannot plan.
//
// demux's hunk parser keys everything on `+++ b/<path>` and `@@` lines, so a
// section without a hunk - a mode change, a binary file, a pure rename, an
// empty new file - is invisible to it, and a deletion is worse: its `+++
// /dev/null` leaves the parser holding a dead hunk that swallows every file
// after it. hv keeps those sections back before the handoff and commits them
// itself, as one commit after the revisions. Leaving them out would commit a
// fraction of the tree and report success, which is the one thing a tool that
// commits on your behalf must never do.
type Remainder struct {
	// Patch is the sections, verbatim, in diff order.
	Patch string
	// Changes names each file and what happened to it.
	Changes []Change
}

// Change is one file demux could not plan.
type Change struct {
	Path string
	// Kind is delete, mode, binary, rename, add (an empty file), submodule,
	// or other for a section hv could not classify but will not drop.
	Kind string
}

// Empty reports whether there is nothing to commit.
func (r Remainder) Empty() bool { return len(r.Changes) == 0 }

// Files is the paths, in diff order.
func (r Remainder) Files() []string {
	out := make([]string, 0, len(r.Changes))
	for _, change := range r.Changes {
		out = append(out, change.Path)
	}
	return out
}

// Summary reads as a commit subject: "remove b.txt, chmod run.sh".
func (r Remainder) Summary() string {
	parts := make([]string, 0, len(r.Changes))
	for _, change := range r.Changes {
		parts = append(parts, change.Describe())
	}
	return strings.Join(parts, ", ")
}

// Describe is one change as a verb phrase.
func (c Change) Describe() string {
	switch c.Kind {
	case "delete":
		return "remove " + c.Path
	case "mode":
		return "chmod " + c.Path
	case "binary":
		return "update binary " + c.Path
	case "rename":
		return "rename " + c.Path
	case "add":
		return "add empty " + c.Path
	case "submodule":
		return "bump submodule " + c.Path
	}
	return "update " + c.Path
}

// section is one file's part of a unified diff.
type section struct {
	text   string
	path   string
	kind   string // "" when demux can plan it
	hunked bool
}

// Split divides a diff into the part demux plans and the part hv commits
// itself. The planned part is what both demux passes must receive, byte for
// byte.
func Split(diff string) (planned string, remainder Remainder) {
	var builder strings.Builder
	for _, current := range sections(diff) {
		if current.kind == "" {
			builder.WriteString(current.text)
			continue
		}
		remainder.Patch += current.text
		remainder.Changes = append(remainder.Changes, Change{Path: current.path, Kind: current.kind})
	}
	return builder.String(), remainder
}

// ChangedFiles is every path a diff touches, in diff order. It is the yardstick
// a plan is checked against: a file here that no revision and no remainder
// covers has been lost.
func ChangedFiles(diff string) []string {
	out := []string{}
	for _, current := range sections(diff) {
		out = append(out, current.path)
	}
	return out
}

// Uncovered lists the changed files that neither the planned hunks nor the
// remainder account for, sorted.
func Uncovered(diff string, plannedFiles []string, remainder Remainder) []string {
	covered := map[string]struct{}{}
	for _, file := range plannedFiles {
		covered[file] = struct{}{}
	}
	for _, file := range remainder.Files() {
		covered[file] = struct{}{}
	}
	missing := []string{}
	for _, file := range ChangedFiles(diff) {
		if _, ok := covered[file]; !ok {
			missing = append(missing, file)
		}
	}
	sort.Strings(missing)
	return missing
}

// sections cuts a diff at every `diff --git` line and classifies each piece.
func sections(diff string) []section {
	out := []section{}
	lines := strings.SplitAfter(diff, "\n")
	start := -1
	for index, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			if start >= 0 {
				out = append(out, classify(strings.Join(lines[start:index], "")))
			}
			start = index
		}
	}
	if start >= 0 {
		out = append(out, classify(strings.Join(lines[start:], "")))
	}
	return out
}

// classify reads a section's header the way demux's parser would, and names
// the reason demux cannot have it when there is one.
func classify(text string) section {
	current := section{text: text}
	var oldPath, newPath, renameTo string
	var deleted, created, mode, binary, submodule bool
	inHeader := true
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "@@ "):
			inHeader = false
			current.hunked = true
		case !inHeader:
			if strings.HasPrefix(line, "-Subproject commit ") || strings.HasPrefix(line, "+Subproject commit ") {
				submodule = true
			}
		case strings.HasPrefix(line, "--- "):
			oldPath = HeaderPath(strings.TrimPrefix(line, "--- "), "a/")
		case strings.HasPrefix(line, "+++ "):
			newPath = HeaderPath(strings.TrimPrefix(line, "+++ "), "b/")
		case strings.HasPrefix(line, "deleted file mode"):
			deleted = true
		case strings.HasPrefix(line, "new file mode"):
			created = true
		case strings.HasPrefix(line, "old mode"), strings.HasPrefix(line, "new mode"):
			mode = true
		case strings.HasPrefix(line, "Binary files"), strings.HasPrefix(line, "GIT binary patch"):
			binary = true
		case strings.HasPrefix(line, "rename to "):
			renameTo = strings.TrimPrefix(line, "rename to ")
		}
	}
	current.path = SectionPath(oldPath, newPath, text)
	if renameTo != "" {
		current.path = renameTo
	}
	switch {
	case deleted || newPath == "/dev/null":
		current.kind = "delete"
	case binary:
		current.kind = "binary"
	case submodule:
		current.kind = "submodule"
	case renameTo != "":
		current.kind = "rename"
	case mode && !current.hunked:
		current.kind = "mode"
	case created && !current.hunked:
		current.kind = "add"
	case !current.hunked || newPath == "":
		current.kind = "other"
	}
	return current
}

// SectionPath is the file a diff section is about: the new path, the old one
// for a deletion, and the `diff --git` line when the section has neither -
// which is what mode-only and binary sections look like.
func SectionPath(oldPath, newPath, text string) string {
	if newPath != "" && newPath != "/dev/null" {
		return newPath
	}
	if oldPath != "" && oldPath != "/dev/null" {
		return oldPath
	}
	return GitLinePath(text)
}

// HeaderPath strips the prefix from a `---`/`+++` path, keeping /dev/null as
// the marker it is. git appends a tab before a trailing-space path; both are
// dropped.
func HeaderPath(value, prefix string) string {
	value = strings.TrimRight(value, "\r")
	if index := strings.IndexByte(value, '\t'); index >= 0 {
		value = value[:index]
	}
	if value == "/dev/null" {
		return value
	}
	return strings.TrimPrefix(value, prefix)
}

// GitLinePath recovers the path from `diff --git a/P b/P`. The two halves
// name the same file, which makes a path with spaces unambiguous: the rest of
// the line is exactly "a/P b/P".
func GitLinePath(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	rest := strings.TrimPrefix(line, "diff --git ")
	if len(rest) < 5 || (len(rest)-5)%2 != 0 {
		return strings.TrimSpace(rest)
	}
	half := (len(rest) - 5) / 2
	left, right := rest[:2+half], rest[3+half:]
	if strings.HasPrefix(left, "a/") && strings.HasPrefix(right, "b/") && left[2:] == right[2:] {
		return left[2:]
	}
	return strings.TrimSpace(rest)
}

// coverageError reports the files a plan lost.
func coverageError(missing []string) error {
	return fmt.Errorf("plan covers only part of the tree; not planned: %s", strings.Join(missing, ", "))
}
