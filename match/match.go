// Package match links working-tree hunks to the agent sessions that produced
// them, in three descending tiers of confidence.
//
// This is the engine gx had and demux did not keep: demux consumes
// matcher.HunkLink as provenance evidence but has no way to build one. The
// tiers are ordered so a hunk takes the strongest available explanation and
// stops:
//
//	exact     an edit event's new text appears verbatim in the hunk
//	fuzzy     it appears modulo whitespace and later edits
//	temporal  the session edited that file inside the hunk's time window
//
// Attribution is evidence, never authority. Demux uses links to nudge grouping
// and to stamp provenance; nothing here ever chooses a branch.
package match

import (
	"sort"
	"strings"
	"time"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/demux/matcher"
	"github.com/satoricorp/demux/provenance"
)

// DefaultTemporalWindow bounds how long after an edit event a hunk may still
// be attributed to it by time alone.
const DefaultTemporalWindow = 30 * time.Minute

// DefaultFuzzyThreshold is the minimum similarity for a tier-2 link.
const DefaultFuzzyThreshold = 0.8

// Confidence assigned per tier. Exact is not 1.0: the same line can be typed
// by an agent and then edited by a person, and a link claims authorship.
const (
	ConfidenceExact    = 0.95
	ConfidenceFuzzy    = 0.7
	ConfidenceTemporal = 0.4
)

// Matcher links hunks to sessions.
type Matcher struct {
	// TemporalWindow bounds tier-3 attribution; zero uses the default.
	TemporalWindow time.Duration
	// FuzzyThreshold is the tier-2 similarity floor; zero uses the default.
	FuzzyThreshold float64
}

// Result is a matching run, shaped for handoff to demux.
type Result struct {
	Links      []matcher.HunkLink
	Coverage   float64
	Tools      []string
	SessionIDs []string
	Warnings   []string
}

// Link attributes each hunk to at most one session, strongest tier first.
func (m Matcher) Link(hunks []demux.HunkRange, events []capture.SessionEvent) Result {
	window := m.TemporalWindow
	if window <= 0 {
		window = DefaultTemporalWindow
	}
	edits := editEvents(events)
	result := Result{Links: []matcher.HunkLink{}}
	linked := 0
	for _, hunk := range hunks {
		link, ok := m.linkHunk(hunk, edits, window)
		if !ok {
			// An unlinked hunk is a normal outcome, not a gap to paper over:
			// it usually means a person wrote those lines by hand.
			continue
		}
		result.Links = append(result.Links, link)
		linked++
	}
	if len(hunks) > 0 {
		result.Coverage = float64(linked) / float64(len(hunks))
	}
	result.Tools = toolsOf(result.Links)
	result.SessionIDs = sessionIDsOf(result.Links)
	return result
}

// Status reports the provenance status a result earns, for
// demux.ProposeOptions.ProvenanceStatus.
func (r Result) Status() string {
	if len(r.Links) == 0 {
		return provenance.StatusAbsent
	}
	return provenance.StatusMatched
}

// Capture converts a result into the evidence demux accepts.
func (r Result) Capture() demux.ComposeCaptureResult {
	return demux.ComposeCaptureResult{
		HunkLinks:    append([]matcher.HunkLink(nil), r.Links...),
		HunkCoverage: r.Coverage,
		Tools:        append([]string(nil), r.Tools...),
		SessionIDs:   append([]string(nil), r.SessionIDs...),
		Status:       r.Status(),
		Warnings:     append([]string(nil), r.Warnings...),
	}
}

func (m Matcher) linkHunk(hunk demux.HunkRange, edits []capture.SessionEvent, window time.Duration) (matcher.HunkLink, bool) {
	if event, ok := m.matchExact(hunk, edits); ok {
		return link(hunk, event, matcher.TierExact, ConfidenceExact), true
	}
	if event, ok := m.matchFuzzy(hunk, edits); ok {
		return link(hunk, event, matcher.TierFuzzy, ConfidenceFuzzy), true
	}
	if event, ok := m.matchTemporal(hunk, edits, window); ok {
		return link(hunk, event, matcher.TierTemporal, ConfidenceTemporal), true
	}
	return matcher.HunkLink{}, false
}

// matchExact finds an edit whose new text appears verbatim among the hunk's
// added lines.
func (m Matcher) matchExact(hunk demux.HunkRange, edits []capture.SessionEvent) (capture.SessionEvent, bool) {
	// TODO(hv): extract added lines from hunk.Patch and index them, then test
	// each edit's NewText against that index. Compare on trimmed lines so
	// re-indentation does not drop a link to the fuzzy tier.
	_ = AddedLines(hunk)
	_ = edits
	return capture.SessionEvent{}, false
}

// matchFuzzy finds an edit whose new text survives in the hunk modulo
// whitespace and subsequent edits.
func (m Matcher) matchFuzzy(hunk demux.HunkRange, edits []capture.SessionEvent) (capture.SessionEvent, bool) {
	// TODO(hv): token-level similarity over added lines against NewText,
	// accepting at or above FuzzyThreshold. gx used a line-shingle overlap;
	// whatever replaces it must stay deterministic - the whole planner
	// downstream is.
	_ = edits
	return capture.SessionEvent{}, false
}

// matchTemporal attributes a hunk to the most recent session that edited the
// same file within the window. It is the weakest tier and the one that fires
// when a file was rewritten past recognition.
func (m Matcher) matchTemporal(hunk demux.HunkRange, edits []capture.SessionEvent, window time.Duration) (capture.SessionEvent, bool) {
	file := strings.TrimSpace(hunk.File)
	if file == "" {
		return capture.SessionEvent{}, false
	}
	// TODO(hv): the hunk has no timestamp of its own, so the window is
	// currently open-ended backwards from the newest matching edit. Once the
	// caller can supply the working-tree mtime, bound it on both sides.
	_ = window
	var best capture.SessionEvent
	found := false
	for _, event := range edits {
		if !sameFile(event.FilePath, file) {
			continue
		}
		if !found || event.TS > best.TS {
			best = event
			found = true
		}
	}
	return best, found
}

// AddedLines returns the lines a hunk adds, without their leading marker.
func AddedLines(hunk demux.HunkRange) []string {
	out := []string{}
	for _, line := range strings.Split(hunk.Patch, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			out = append(out, strings.TrimPrefix(line, "+"))
		}
	}
	return out
}

func link(hunk demux.HunkRange, event capture.SessionEvent, tier int, confidence float64) matcher.HunkLink {
	return matcher.HunkLink{
		HunkID:     hunk.ID,
		SessionID:  event.SessionID,
		Tier:       tier,
		Confidence: confidence,
		Authorship: matcher.AuthorshipAgent,
		Tool:       event.Tool,
		Model:      event.Model,
	}
}

func editEvents(events []capture.SessionEvent) []capture.SessionEvent {
	out := make([]capture.SessionEvent, 0, len(events))
	for _, event := range events {
		if event.IsEditEvent() {
			out = append(out, event)
		}
	}
	return out
}

// sameFile compares paths tolerantly: session logs record absolute paths while
// a diff records repo-relative ones.
func sameFile(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	return left == right || strings.HasSuffix(left, "/"+right) || strings.HasSuffix(right, "/"+left)
}

func toolsOf(links []matcher.HunkLink) []string {
	return uniqueSorted(links, func(l matcher.HunkLink) string { return l.Tool })
}

func sessionIDsOf(links []matcher.HunkLink) []string {
	return uniqueSorted(links, func(l matcher.HunkLink) string { return l.SessionID })
}

func uniqueSorted(links []matcher.HunkLink, key func(matcher.HunkLink) string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, current := range links {
		value := strings.TrimSpace(key(current))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
