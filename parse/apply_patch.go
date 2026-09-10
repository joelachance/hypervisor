package parse

import "strings"

// Codex's structured edit tool is apply_patch. Its input is a patch of its
// own dialect rather than a unified diff:
//
//	*** Begin Patch
//	*** Update File: src/lib.rs
//	*** Move to: src/lib2.rs
//	@@ optional context
//	 unchanged
//	-removed
//	+added
//	*** Add File: src/new.rs
//	+every line
//	*** Delete File: src/old.rs
//	*** End Patch
//
// What hv needs from it is per file: the path, the lines the session wrote,
// and the lines it removed - the same evidence a Claude Edit call carries.

// PatchEdit is one file an apply_patch call touched.
type PatchEdit struct {
	Path    string
	OldText string
	NewText string
	// Deleted marks a *** Delete File, and the old side of a *** Move to.
	Deleted bool
}

// ApplyPatchEdits reads an apply_patch input into per-file edits, in patch
// order. Malformed input yields whatever files it could name; a patch that
// names none yields nothing.
func ApplyPatchEdits(input string) []PatchEdit {
	edits := []PatchEdit{}
	var current *PatchEdit
	var added, removed []string
	flush := func() {
		if current == nil {
			return
		}
		current.NewText = strings.Join(added, "\n")
		current.OldText = strings.Join(removed, "\n")
		edits = append(edits, *current)
		current, added, removed = nil, nil, nil
	}
	for _, line := range strings.Split(input, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "*** Add File: "):
			flush()
			current = &PatchEdit{Path: strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: "))}
		case strings.HasPrefix(line, "*** Update File: "):
			flush()
			current = &PatchEdit{Path: strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))}
		case strings.HasPrefix(line, "*** Delete File: "):
			flush()
			edits = append(edits, PatchEdit{Path: strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: ")), Deleted: true})
		case strings.HasPrefix(line, "*** Move to: "):
			// The old path stops existing; the new one takes the content.
			if current != nil {
				edits = append(edits, PatchEdit{Path: current.Path, Deleted: true})
				current.Path = strings.TrimSpace(strings.TrimPrefix(line, "*** Move to: "))
			}
		case strings.HasPrefix(line, "*** "):
			// Begin Patch, End Patch, End of File: structure, not content.
		case current == nil:
		case strings.HasPrefix(line, "+"):
			added = append(added, line[1:])
		case strings.HasPrefix(line, "-"):
			removed = append(removed, line[1:])
		}
	}
	flush()
	out := edits[:0]
	for _, edit := range edits {
		if strings.TrimSpace(edit.Path) != "" {
			out = append(out, edit)
		}
	}
	return out
}
