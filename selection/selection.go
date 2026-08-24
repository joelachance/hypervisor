// Package selection turns "which coding sessions?" into a concrete set.
//
// Selection is deliberately explicit. Everything downstream - the matcher, and
// through it demux's session-affinity grouping - treats a selected session as
// a statement that the person meant those edits to travel together, so hv
// never guesses the set on their behalf.
package selection

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

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
	// TouchedUnder restricts to sessions that edited at least one file under
	// this directory. It is the honest test of "did this session work on this
	// repo": an agent's cwd may be another checkout entirely while it edits
	// files here, and worktrees make that routine.
	TouchedUnder string
	// IncludeArchived keeps sessions the person archived in the app. They are
	// dropped by default: archiving is someone saying they are done with it.
	IncludeArchived bool
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
	if candidate.Archived && !f.IncludeArchived {
		return false
	}
	for _, file := range f.TouchedFiles {
		if !candidate.Touched(file) {
			return false
		}
	}
	if under := strings.TrimSpace(f.TouchedUnder); under != "" && !touchedUnder(candidate, under) {
		return false
	}
	return true
}

func touchedUnder(candidate session.Session, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	for _, file := range candidate.Files {
		if strings.HasPrefix(file, dir+"/") {
			return true
		}
	}
	return false
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

// Interactive prints a numbered list of sessions and reads a choice.
//
// Accepted input is a comma or space separated list of numbers, a range
// ("1-3"), or "all" - and "o<n>" to open session n where it is running rather
// than merging it, which is the difference between "I want this work" and "I
// want to go back to that conversation".
//
// An empty line selects nothing and is not an error: a person changing their
// mind is a normal outcome, not a failure.
//
// onOpen may be nil, which disables the open key. It usually does not return -
// attaching to tmux or resuming a CLI session replaces this process - so the
// loop continues only for an open that hands off elsewhere.
func Interactive(in io.Reader, out io.Writer, sessions []session.Session, now int64, onOpen func(session.Session) error) ([]session.Session, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	reader := bufio.NewReader(in)
	for {
		for index, current := range sessions {
			fmt.Fprintf(out, "  %2d  %-6s %-7s %-13s %3d edits  %s\n",
				index+1, current.State(now), current.Tool, shortID(current.ID),
				current.EditCount, current.Label())
		}
		if onOpen != nil {
			fmt.Fprintf(out, "\n  1-%d merge  ·  o<n> open session n  ·  all  ·  enter to cancel\n", len(sessions))
		}
		fmt.Fprintf(out, "\nselect sessions to merge [1-%d, comma separated, or all]: ", len(sessions))

		line, err := reader.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return nil, nil
		}
		if index, ok := openRequest(line); ok {
			if onOpen == nil {
				fmt.Fprintln(out, "opening is not available here")
				continue
			}
			if index < 1 || index > len(sessions) {
				fmt.Fprintf(out, "session %d is outside 1-%d\n\n", index, len(sessions))
				continue
			}
			if err := onOpen(sessions[index-1]); err != nil {
				fmt.Fprintf(out, "could not open that session: %v\n\n", err)
			}
			continue
		}
		return Parse(sessions, line)
	}
}

// openRequest recognizes the open key: "o3", "o 3", "open 3".
func openRequest(line string) (int, bool) {
	trimmed := strings.TrimSpace(strings.ToLower(line))
	for _, prefix := range []string{"open ", "o "} {
		if strings.HasPrefix(trimmed, prefix) {
			return atoiOrZero(strings.TrimSpace(trimmed[len(prefix):]))
		}
	}
	if strings.HasPrefix(trimmed, "o") {
		return atoiOrZero(strings.TrimSpace(trimmed[1:]))
	}
	return 0, false
}

func atoiOrZero(value string) (int, bool) {
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return number, true
}

// Parse resolves a selection string against a listing.
func Parse(sessions []session.Session, input string) ([]session.Session, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}
	if strings.EqualFold(input, "all") {
		return sessions, nil
	}
	chosen := []session.Session{}
	seen := map[int]struct{}{}
	for _, field := range strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == ' ' }) {
		low, high, err := parseRange(field, len(sessions))
		if err != nil {
			return nil, err
		}
		for index := low; index <= high; index++ {
			if _, ok := seen[index]; ok {
				continue
			}
			seen[index] = struct{}{}
			chosen = append(chosen, sessions[index-1])
		}
	}
	return chosen, nil
}

func parseRange(field string, count int) (int, int, error) {
	bounds := strings.SplitN(strings.TrimSpace(field), "-", 2)
	low, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("not a session number: %q", field)
	}
	high := low
	if len(bounds) == 2 {
		if high, err = strconv.Atoi(strings.TrimSpace(bounds[1])); err != nil {
			return 0, 0, fmt.Errorf("not a session range: %q", field)
		}
	}
	if low < 1 || high > count || low > high {
		return 0, 0, fmt.Errorf("session %s is outside 1-%d", field, count)
	}
	return low, high, nil
}

// Active restricts a filter to sessions written within session.ActiveWindow.
func Active(now time.Time) Filter {
	return Filter{Since: now.Add(-session.ActiveWindow * time.Millisecond).UnixMilli()}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func matchesAnyID(id string, candidates []string) bool {
	id = strings.TrimSpace(id)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if strings.HasPrefix(id, candidate) {
			return true
		}
		// Accept the elided label hv prints for near-identical ids
		// ("aaaaaaaa…a001"): what is displayed should be paste-able back in.
		// Three dots means the same thing, because that is what people type
		// when the ellipsis is awkward to reach.
		normalized := strings.ReplaceAll(candidate, "...", "\u2026")
		if head, tail, ok := strings.Cut(normalized, "\u2026"); ok {
			if strings.HasPrefix(id, head) && strings.HasSuffix(id, tail) {
				return true
			}
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
