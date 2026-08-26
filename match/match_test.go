package match

import (
	"testing"
	"time"

	"github.com/satoricorp/demux"
	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/demux/matcher"
	"github.com/satoricorp/demux/provenance"
)

func TestTemporalTierLinksByFile(t *testing.T) {
	hunks := []demux.HunkRange{{ID: "h1", File: "internal/billing/invoice.go"}}
	events := []capture.SessionEvent{
		{SessionID: "s1", Tool: capture.ToolClaude, Kind: capture.KindEdit, TS: 100, FilePath: "/repo/internal/billing/invoice.go"},
		{SessionID: "s2", Tool: capture.ToolCodex, Kind: capture.KindEdit, TS: 200, FilePath: "/repo/internal/billing/invoice.go"},
		{SessionID: "s3", Tool: capture.ToolClaude, Kind: capture.KindRead, TS: 300, FilePath: "/repo/internal/billing/invoice.go"},
	}

	result := (Matcher{}).Link(hunks, events)
	if len(result.Links) != 1 {
		t.Fatalf("links = %v, want one", result.Links)
	}
	got := result.Links[0]
	if got.SessionID != "s2" {
		t.Errorf("session = %q, want s2 - the most recent edit, not the later read", got.SessionID)
	}
	if got.Tier != matcher.TierTemporal || got.Confidence != ConfidenceTemporal {
		t.Errorf("tier/confidence = %d/%v, want temporal", got.Tier, got.Confidence)
	}
	if got.Authorship != matcher.AuthorshipAgent {
		t.Errorf("authorship = %q, want agent", got.Authorship)
	}
}

func TestUnmatchedHunksLowerCoverage(t *testing.T) {
	hunks := []demux.HunkRange{{ID: "h1", File: "a.go"}, {ID: "h2", File: "handwritten.go"}}
	events := []capture.SessionEvent{{SessionID: "s1", Kind: capture.KindEdit, TS: 1, FilePath: "a.go"}}

	result := (Matcher{}).Link(hunks, events)
	if len(result.Links) != 1 {
		t.Fatalf("links = %v, want only the agent-edited hunk", result.Links)
	}
	if result.Coverage != 0.5 {
		t.Errorf("coverage = %v, want 0.5", result.Coverage)
	}
}

func TestStatusReflectsEvidence(t *testing.T) {
	empty := Matcher{}.Link(nil, nil)
	if empty.Status() != provenance.StatusAbsent {
		t.Errorf("empty status = %q, want absent", empty.Status())
	}
	matched := Matcher{}.Link([]demux.HunkRange{{ID: "h1", File: "a.go"}},
		[]capture.SessionEvent{{SessionID: "s1", Kind: capture.KindEdit, TS: 1, FilePath: "a.go"}})
	if matched.Status() != provenance.StatusMatched {
		t.Errorf("matched status = %q, want matched", matched.Status())
	}
}

func TestCaptureCarriesEvidenceToDemux(t *testing.T) {
	result := Matcher{}.Link(
		[]demux.HunkRange{{ID: "h1", File: "a.go"}},
		[]capture.SessionEvent{{SessionID: "s1", Tool: capture.ToolClaude, Kind: capture.KindEdit, TS: 1, FilePath: "a.go"}},
	)
	got := result.Capture()
	if len(got.HunkLinks) != 1 || got.HunkCoverage != 1 {
		t.Fatalf("capture = %+v, want one link at full coverage", got)
	}
	if len(got.SessionIDs) != 1 || got.SessionIDs[0] != "s1" {
		t.Errorf("session ids = %v, want [s1]", got.SessionIDs)
	}
	if len(got.Tools) != 1 || got.Tools[0] != capture.ToolClaude {
		t.Errorf("tools = %v, want [claude]", got.Tools)
	}
}

func TestAddedLinesIgnoresFileHeader(t *testing.T) {
	hunk := demux.HunkRange{Patch: "+++ b/a.go\n@@\n line\n+added\n-removed\n"}
	got := AddedLines(hunk)
	if len(got) != 1 || got[0] != "added" {
		t.Fatalf("AddedLines() = %v, want [added]", got)
	}
}

const limiterHunk = `diff --git a/limiter.go b/limiter.go
--- a/limiter.go
+++ b/limiter.go
@@ -1,3 +1,6 @@
 package app
 
+func Limit(n int) int {
+	return n * 2
+}
`

// The case the temporal tier cannot get right: two sessions edited the same
// file in the same window, and only the text says whose work this is.
func TestExactTextBeatsRecency(t *testing.T) {
	hunks := []demux.HunkRange{{ID: "h1", File: "limiter.go", Patch: limiterHunk}}
	events := []capture.SessionEvent{
		{SessionID: "wrote-it", Tool: capture.ToolClaude, Kind: capture.KindEdit, TS: 100,
			FilePath: "limiter.go", NewText: "func Limit(n int) int {\n\treturn n * 2\n}\n"},
		// Later, but a different part of the file - recency alone would pick it.
		{SessionID: "touched-it-later", Tool: capture.ToolClaude, Kind: capture.KindEdit, TS: 900,
			FilePath: "limiter.go", NewText: "// unrelated comment somewhere else\n"},
	}
	result := (Matcher{}).Link(hunks, events)
	if len(result.Links) != 1 {
		t.Fatalf("links = %v, want one", result.Links)
	}
	got := result.Links[0]
	if got.SessionID != "wrote-it" {
		t.Errorf("session = %q, want the one whose text is in the hunk", got.SessionID)
	}
	if got.Tier != matcher.TierExact || got.Confidence != ConfidenceExact {
		t.Errorf("tier = %d, want exact", got.Tier)
	}
}

// Re-indentation must not cost a link: the line is still that agent's.
func TestExactIgnoresWhitespace(t *testing.T) {
	hunks := []demux.HunkRange{{ID: "h1", File: "limiter.go", Patch: limiterHunk}}
	events := []capture.SessionEvent{{SessionID: "s1", Kind: capture.KindEdit, TS: 1,
		FilePath: "limiter.go", NewText: "        func Limit(n int) int {\n                return n * 2\n        }"}}
	got := (Matcher{}).Link(hunks, events).Links
	if len(got) != 1 || got[0].Tier != matcher.TierExact {
		t.Fatalf("links = %+v, want an exact match despite the indentation", got)
	}
}

const bigHunk = `diff --git a/limiter.go b/limiter.go
--- a/limiter.go
+++ b/limiter.go
@@ -1,2 +1,8 @@
 package app
 
+// Limiter caps requests per window.
+type Limiter struct {
+	window time.Duration
+	burst  int
+}
+
+func (l *Limiter) Allow() bool { return l.burst > 0 }
`

// Text that survived a later tweak still matches: overlap is measured against
// the smaller side, so one changed line out of five is still that session's
// work rather than a coincidence.
func TestFuzzyMatchesTextThatWasLaterTweaked(t *testing.T) {
	hunks := []demux.HunkRange{{ID: "h1", File: "limiter.go", Patch: bigHunk}}
	events := []capture.SessionEvent{{SessionID: "s1", Kind: capture.KindEdit, TS: 1,
		FilePath: "limiter.go", NewText: "// Limiter caps requests per window.\n" +
			"type Limiter struct {\n\twindow time.Duration\n\tburst  int\n}\n" +
			"func (l *Limiter) Allow() bool { return l.burst > 1 }\n"}}
	got := (Matcher{}).Link(hunks, events).Links
	if len(got) != 1 {
		t.Fatalf("links = %+v, want one", got)
	}
	if got[0].Tier != matcher.TierFuzzy {
		t.Errorf("tier = %d, want fuzzy - two lines of three match", got[0].Tier)
	}
}

// Text from another file must never claim a hunk, however similar.
func TestTextFromAnotherFileDoesNotMatch(t *testing.T) {
	hunks := []demux.HunkRange{{ID: "h1", File: "limiter.go", Patch: limiterHunk}}
	events := []capture.SessionEvent{{SessionID: "s1", Kind: capture.KindEdit, TS: 1,
		FilePath: "elsewhere.go", NewText: "func Limit(n int) int {\n\treturn n * 2\n}\n"}}
	if got := (Matcher{}).Link(hunks, events).Links; len(got) != 0 {
		t.Fatalf("links = %+v, want none - the edit was to another file", got)
	}
}

// Blank lines and lone braces appear in every file; matching on them would
// attribute anything to anyone.
func TestTrivialLinesDoNotCarryAMatch(t *testing.T) {
	hunks := []demux.HunkRange{{ID: "h1", File: "limiter.go", Patch: limiterHunk}}
	events := []capture.SessionEvent{{SessionID: "s1", Kind: capture.KindEdit, TS: 1,
		FilePath: "limiter.go", NewText: "}\n\n"}}
	got := (Matcher{}).Link(hunks, events).Links
	if len(got) == 1 && got[0].Tier != matcher.TierTemporal {
		t.Fatalf("tier = %d, want the temporal fallback, not a text match on a brace", got[0].Tier)
	}
}

// Same inputs, same links, whatever order the logs were read in.
func TestLinkingIsDeterministic(t *testing.T) {
	hunks := []demux.HunkRange{{ID: "h1", File: "limiter.go", Patch: limiterHunk}}
	text := "func Limit(n int) int {\n\treturn n * 2\n}\n"
	forward := []capture.SessionEvent{
		{SessionID: "aaa", Kind: capture.KindEdit, TS: 5, FilePath: "limiter.go", NewText: text},
		{SessionID: "bbb", Kind: capture.KindEdit, TS: 5, FilePath: "limiter.go", NewText: text},
	}
	backward := []capture.SessionEvent{forward[1], forward[0]}
	first := (Matcher{}).Link(hunks, forward).Links[0].SessionID
	second := (Matcher{}).Link(hunks, backward).Links[0].SessionID
	if first != second {
		t.Fatalf("the same events in a different order produced %q and %q", first, second)
	}
}

// The temporal window has to mean something: an edit made days before the last
// one is a different piece of work that has since been overwritten, not a
// plausible author of what survived.
func TestTemporalWindowExcludesStaleEdits(t *testing.T) {
	hunks := []demux.HunkRange{{ID: "h1", File: "limiter.go"}}
	newest := int64(10_000_000)
	events := []capture.SessionEvent{
		{SessionID: "long-ago", Kind: capture.KindEdit, FilePath: "limiter.go",
			TS: newest - int64(72*time.Hour/time.Millisecond)},
		{SessionID: "just-before", Kind: capture.KindEdit, FilePath: "limiter.go",
			TS: newest - int64(5*time.Minute/time.Millisecond)},
		{SessionID: "newest", Kind: capture.KindEdit, FilePath: "limiter.go", TS: newest},
	}
	// The newest still wins; the point is that the stale one is not a
	// candidate at all.
	got := (Matcher{}).Link(hunks, events).Links
	if len(got) != 1 || got[0].SessionID != "newest" {
		t.Fatalf("links = %+v, want the newest edit", got)
	}

	// With the newest two removed, a short window leaves nothing plausible.
	stale := []capture.SessionEvent{events[0], {
		SessionID: "also-long-ago", Kind: capture.KindEdit, FilePath: "limiter.go",
		TS: events[0].TS - int64(48*time.Hour/time.Millisecond),
	}}
	linked := Matcher{TemporalWindow: time.Hour}.Link(hunks, stale).Links
	if len(linked) != 1 || linked[0].SessionID != "long-ago" {
		t.Fatalf("links = %+v, want only the edit inside the window", linked)
	}
}

// A window wide enough keeps everything, which is the old behaviour and the
// right default when sessions overlap.
func TestTemporalWindowKeepsRecentEdits(t *testing.T) {
	hunks := []demux.HunkRange{{ID: "h1", File: "limiter.go"}}
	events := []capture.SessionEvent{
		{SessionID: "older", Kind: capture.KindEdit, FilePath: "limiter.go", TS: 1_000},
		{SessionID: "newer", Kind: capture.KindEdit, FilePath: "limiter.go", TS: 2_000},
	}
	if got := (Matcher{}).Link(hunks, events).Links; len(got) != 1 || got[0].SessionID != "newer" {
		t.Fatalf("links = %+v, want the newer edit", got)
	}
}
