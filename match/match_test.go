package match

import (
	"testing"

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

	result := Matcher{}.Link(hunks, events)
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

	result := Matcher{}.Link(hunks, events)
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
