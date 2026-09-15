package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Region is one stretch of a file that two sides changed differently, with
// every line of all three versions kept. Summarizing here would be summarizing
// the thing a person has to decide about.
type Region struct {
	Index     int      `json:"index"`
	StartLine int      `json:"start_line"`
	Base      []string `json:"base"`
	Ours      []string `json:"ours"`
	Theirs    []string `json:"theirs"`
	Identical bool     `json:"identical,omitempty"`
}

// ErrBinary is returned when a side of a merge is not text. git will happily
// wrap binary bytes in conflict markers; nothing downstream can read that.
var ErrBinary = errors.New("binary file")

// markerSize is the width of the conflict markers hv asks git for. git's
// default of seven collides with a Markdown setext underline (=======),
// which would be read as the separator and vanish from the resolution. A
// content line would have to be exactly this many marker characters to be
// mistaken for one.
const markerSize = 24

// markers is the set of conflict markers at one width. The width is read
// from the opening marker of whatever text is being parsed, so text git wrote
// at hv's width and text written by hand at git's default both read.
type markers struct{ ours, base, split, theirs string }

func markersOfWidth(width int) markers {
	return markers{
		ours:   strings.Repeat("<", width),
		base:   strings.Repeat("|", width),
		split:  strings.Repeat("=", width),
		theirs: strings.Repeat(">", width),
	}
}

// openingWidth is the width of the conflict marker a line opens with, or 0
// when the line is not an opening marker: at least seven '<', alone or
// followed by a space and a label.
func openingWidth(line string) int {
	width := 0
	for width < len(line) && line[width] == '<' {
		width++
	}
	if width < 7 || (width < len(line) && line[width] != ' ') {
		return 0
	}
	return width
}

// isMarker reports whether a line is the given conflict marker: the marker
// alone, or the marker followed by a space and a label.
func isMarker(line, marker string) bool {
	return line == marker || strings.HasPrefix(line, marker+" ")
}

// MergeFile merges three versions of a file and returns the result with
// conflict markers, exactly as git wrote it - bytes included: a file without
// a trailing newline stays without one.
//
// The blobs are written to a scratch directory rather than the worktree; an
// empty id means the side does not have the file. A binary side is refused
// with ErrBinary rather than merged into something unreadable.
func MergeFile(ctx context.Context, dir string, base, ours, theirs string, labels [3]string) (string, error) {
	scratch, err := os.MkdirTemp("", "hv-merge-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)

	paths := [3]string{}
	for index, blob := range [3]string{base, ours, theirs} {
		target := filepath.Join(scratch, []string{"base", "ours", "theirs"}[index])
		paths[index] = target
		body := ""
		if blob != "" {
			if body, err = RunRaw(ctx, dir, "cat-file", "blob", blob); err != nil {
				return "", err
			}
			if bytes.IndexByte([]byte(body), 0) >= 0 {
				return "", fmt.Errorf("%s side: %w", []string{"base", "ours", "theirs"}[index], ErrBinary)
			}
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	// --diff3 keeps the base, which is what lets a region show all three
	// versions rather than two and a guess. merge-file exits with the number
	// of conflicts, which is a result; only 255 is a failure.
	command := exec.CommandContext(ctx, "git", "merge-file", "-p", "--diff3",
		fmt.Sprintf("--marker-size=%d", markerSize),
		"-L", labels[1], "-L", labels[0], "-L", labels[2],
		paths[1], paths[0], paths[2])
	command.Dir = dir
	out, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 && exitErr.ExitCode() < 128 {
			return string(out), nil
		}
		return "", fmt.Errorf("git merge-file: %v: %s", err, stderr(err))
	}
	return string(out), nil
}

// ParseRegions reads git's conflict markers back into structured regions.
func ParseRegions(merged string) []Region {
	regions := []Region{}
	var current *Region
	var m markers
	section := 0 // 1 ours, 2 base, 3 theirs
	for number, line := range strings.Split(merged, "\n") {
		switch {
		case current == nil && openingWidth(line) > 0:
			m = markersOfWidth(openingWidth(line))
			current = &Region{Index: len(regions) + 1, StartLine: number + 1}
			section = 1
		case current != nil && isMarker(line, m.base):
			section = 2
		case current != nil && line == m.split:
			section = 3
		case current != nil && isMarker(line, m.theirs):
			current.Identical = equalLines(current.Ours, current.Theirs)
			regions = append(regions, *current)
			current, section = nil, 0
		case current != nil:
			switch section {
			case 1:
				current.Ours = append(current.Ours, line)
			case 2:
				current.Base = append(current.Base, line)
			case 3:
				current.Theirs = append(current.Theirs, line)
			}
		}
	}
	return regions
}

// ConflictedLines counts the lines inside conflict markers, ours and theirs
// sides, straight from the marked-up text. It is deliberately not derived
// from ParseRegions: it is the figure the regions are checked against.
func ConflictedLines(merged string) int {
	count := 0
	section := 0
	var m markers
	for _, line := range strings.Split(merged, "\n") {
		switch {
		case section == 0 && openingWidth(line) > 0:
			m = markersOfWidth(openingWidth(line))
			section = 1
		case section != 0 && isMarker(line, m.base):
			section = 2
		case section != 0 && line == m.split:
			section = 3
		case section != 0 && isMarker(line, m.theirs):
			section = 0
		case section == 1 || section == 3:
			count++
		}
	}
	return count
}

// HasMarkers reports whether a text still carries conflict markers.
func HasMarkers(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if openingWidth(line) > 0 {
			return true
		}
	}
	return false
}

// ReplaceRegions swaps every conflict block for its resolution in one pass,
// leaving everything around them byte for byte as git produced it.
//
// One pass, not one block at a time: replacing them individually renumbers the
// blocks that follow, so the second resolution would be looked for in a file
// that no longer has a second conflict.
func ReplaceRegions(merged string, replacements [][]string) (string, error) {
	lines := strings.Split(merged, "\n")
	out := make([]string, 0, len(lines))
	seen, inBlock := 0, false
	var m markers
	for _, line := range lines {
		switch {
		case !inBlock && openingWidth(line) > 0:
			m = markersOfWidth(openingWidth(line))
			if seen >= len(replacements) {
				return "", fmt.Errorf("more conflict blocks than resolutions")
			}
			out = append(out, replacements[seen]...)
			seen++
			inBlock = true
		case inBlock && isMarker(line, m.theirs):
			inBlock = false
		case inBlock:
			// dropped: the resolution replaces the whole block
		default:
			out = append(out, line)
		}
	}
	if seen != len(replacements) {
		return "", fmt.Errorf("resolved %d regions but the file has %d", len(replacements), seen)
	}
	return strings.Join(out, "\n"), nil
}

// Stage is one conflicted path's three blobs and the mode each side had.
type Stage struct {
	Blobs [3]string
	Modes [3]string
}

// Mode is the mode the resolved file should carry: ours when ours has the
// file, then theirs, then base, then a plain file.
func (s Stage) Mode() string {
	for _, index := range []int{1, 2, 0} {
		if s.Modes[index] != "" {
			return s.Modes[index]
		}
	}
	return "100644"
}

// UnmergedStages lists the paths git left conflicted in an index, with the
// base, ours, and theirs blob and mode for each, in the order they appear.
func UnmergedStages(ctx context.Context, dir string, env []string) ([]string, map[string]Stage, error) {
	listing, err := RunIn(ctx, dir, env, "ls-files", "-u")
	if err != nil {
		return nil, nil, err
	}
	order := []string{}
	stages := map[string]Stage{}
	for _, line := range strings.Split(listing, "\n") {
		path, stage, mode, blob, ok := parseStageLine(line)
		if !ok {
			continue
		}
		if _, seen := stages[path]; !seen {
			order = append(order, path)
		}
		entry := stages[path]
		entry.Blobs[stage-1] = blob
		entry.Modes[stage-1] = mode
		stages[path] = entry
	}
	return order, stages, nil
}

// parseStageLine reads "<mode> <oid> <stage>\t<path>".
func parseStageLine(line string) (path string, stage int, mode, blob string, ok bool) {
	parts := strings.SplitN(line, "\t", 2)
	if len(parts) != 2 {
		return "", 0, "", "", false
	}
	meta := strings.Fields(parts[0])
	if len(meta) != 3 {
		return "", 0, "", "", false
	}
	switch meta[2] {
	case "1":
		stage = 1
	case "2":
		stage = 2
	case "3":
		stage = 3
	default:
		return "", 0, "", "", false
	}
	return parts[1], stage, meta[0], meta[1], true
}

func equalLines(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
