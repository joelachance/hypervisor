// Package selection turns "which coding sessions?" into a concrete set.
//
// Selection is deliberately explicit. Everything downstream - the matcher, and
// through it demux's session-affinity grouping - treats a selected session as
// a statement that the person meant those edits to travel together, so hv
// never guesses the set on their behalf.
package selection

import (
	"sort"
	"strings"

	"github.com/satoricorp/hv/session"
)

// Filter narrows a session list. A zero Filter matches everything; each set
// field further constrains, and they are ANDed.
type Filter struct {
	// IDs restricts to these exact session IDs. A prefix is accepted, so a
	// person can type the first few characters of a uuid.
	IDs []string
	// Tools restricts to these capture.Tool* values.
	Tools []string
	// RepoRoot restricts to sessions resolved to this worktree.
	RepoRoot string
	// Since and Until bound last activity, in unix milliseconds. Zero means
	// unbounded on that end.
	Since int64
	Until int64
	// TouchedFiles restricts to sessions that edited every listed path.
	TouchedFiles []string
	// EditsOnly drops sessions that never edited a file - a session that only
	// read and answered questions produced no hunks to attribute.
	EditsOnly bool
	// Limit caps the result, applied after ordering. Zero means no cap.
	Limit int
}

// Match reports whether one session satisfies the filter.
func (f Filter) Match(candidate session.Session) bool {
	if len(f.IDs) > 0 && !matchesAnyID(candidate.ID, f.IDs) {
		return false
	}
	if len(f.Tools) > 0 && !containsFold(f.Tools, candidate.Tool) {
		return false
	}
	if root := strings.TrimSpace(f.RepoRoot); root != "" && candidate.RepoRoot != root {
		return false
	}
	if f.Since > 0 && candidate.LastActiveAt < f.Since {
		return false
	}
	if f.Until > 0 && candidate.LastActiveAt > f.Until {
		return false
	}
	if f.EditsOnly && candidate.EditCount == 0 {
		return false
	}
	for _, file := range f.TouchedFiles {
		if !candidate.Touched(file) {
			return false
		}
	}
	return true
}

// Apply filters and orders sessions, most recently active first.
func Apply(sessions []session.Session, filter Filter) []session.Session {
	out := make([]session.Session, 0, len(sessions))
	for _, candidate := range sessions {
		if filter.Match(candidate) {
			out = append(out, candidate)
		}
	}
	sort.Slice(out, func(left, right int) bool {
		if out[left].LastActiveAt != out[right].LastActiveAt {
			return out[left].LastActiveAt > out[right].LastActiveAt
		}
		return out[left].ID < out[right].ID
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out
}

// IDs returns the session IDs of a selection, in order.
func IDs(sessions []session.Session) []string {
	out := make([]string, 0, len(sessions))
	for _, candidate := range sessions {
		out = append(out, candidate.ID)
	}
	return out
}

// Files returns every file the selected sessions edited, deduplicated and
// sorted. This is what scopes the diff hv asks demux to plan.
func Files(sessions []session.Session) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, candidate := range sessions {
		for _, file := range candidate.Files {
			if _, ok := seen[file]; ok {
				continue
			}
			seen[file] = struct{}{}
			out = append(out, file)
		}
	}
	sort.Strings(out)
	return out
}

// Interactive presents sessions for a person to choose from and returns the
// chosen subset.
//
// TODO(hv): implement the picker. Until then it is the identity function, so
// callers can wire the flow and a non-interactive caller keeps working.
func Interactive(sessions []session.Session) ([]session.Session, error) {
	return sessions, nil
}

func matchesAnyID(id string, candidates []string) bool {
	id = strings.TrimSpace(id)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && strings.HasPrefix(id, candidate) {
			return true
		}
	}
	return false
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}
