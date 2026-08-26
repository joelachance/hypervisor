package gitx

import (
	"context"
	"fmt"
	"os"
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

// MergeFile merges three versions of a file and returns the result with
// conflict markers, exactly as git wrote it.
//
// The blobs are written to a scratch directory rather than the worktree; an
// empty id means the side does not have the file.
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
			if body, err = Run(ctx, dir, "cat-file", "blob", blob); err != nil {
				return "", err
			}
			body += "\n"
		}
		if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	// --diff3 keeps the base, which is what lets a region show all three
	// versions rather than two and a guess.
	return Output(ctx, dir, "merge-file", "-p", "--diff3",
		"-L", labels[1], "-L", labels[0], "-L", labels[2],
		paths[1], paths[0], paths[2]), nil
}

// ParseRegions reads git's conflict markers back into structured regions.
func ParseRegions(merged string) []Region {
	regions := []Region{}
	var current *Region
	section := 0 // 1 ours, 2 base, 3 theirs
	for number, line := range strings.Split(merged, "\n") {
		switch {
		case strings.HasPrefix(line, "<<<<<<<"):
			current = &Region{Index: len(regions) + 1, StartLine: number + 1}
			section = 1
		case strings.HasPrefix(line, "|||||||") && current != nil:
			section = 2
		case strings.HasPrefix(line, "=======") && current != nil:
			section = 3
		case strings.HasPrefix(line, ">>>>>>>") && current != nil:
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

// UnmergedStages lists the paths git left conflicted in an index, with the
// base, ours, and theirs blob for each, in the order they appear.
func UnmergedStages(ctx context.Context, dir string, env []string) ([]string, map[string][3]string, error) {
	listing, err := RunIn(ctx, dir, env, "ls-files", "-u")
	if err != nil {
		return nil, nil, err
	}
	order := []string{}
	stages := map[string][3]string{}
	for _, line := range strings.Split(listing, "\n") {
		path, stage, blob, ok := parseStageLine(line)
		if !ok {
			continue
		}
		if _, seen := stages[path]; !seen {
			order = append(order, path)
		}
		entry := stages[path]
		entry[stage-1] = blob
		stages[path] = entry
	}
	return order, stages, nil
}

// parseStageLine reads "<mode> <oid> <stage>\t<path>".
func parseStageLine(line string) (path string, stage int, blob string, ok bool) {
	parts := strings.SplitN(line, "\t", 2)
	if len(parts) != 2 {
		return "", 0, "", false
	}
	meta := strings.Fields(parts[0])
	if len(meta) != 3 {
		return "", 0, "", false
	}
	switch meta[2] {
	case "1":
		stage = 1
	case "2":
		stage = 2
	case "3":
		stage = 3
	default:
		return "", 0, "", false
	}
	return parts[1], stage, meta[1], true
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
