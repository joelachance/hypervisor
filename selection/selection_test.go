package selection

import (
	"testing"

	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/hv/session"
)

func sessions() []session.Session {
	return []session.Session{
		{ID: "aaa111", Tool: capture.ToolClaude, LastActiveAt: 300, EditCount: 2, Files: []string{"a.go", "b.go"}, RepoRoot: "/repo"},
		{ID: "bbb222", Tool: capture.ToolCodex, LastActiveAt: 200, EditCount: 1, Files: []string{"b.go"}, RepoRoot: "/repo"},
		{ID: "ccc333", Tool: capture.ToolClaude, LastActiveAt: 100, EditCount: 0, RepoRoot: "/other"},
	}
}

func TestZeroFilterMatchesEverythingOrderedByRecency(t *testing.T) {
	got := Apply(sessions(), Filter{})
	if len(got) != 3 || got[0].ID != "aaa111" || got[2].ID != "ccc333" {
		t.Fatalf("Apply() = %v, want all three, most recent first", IDs(got))
	}
}

func TestFilterByIDPrefix(t *testing.T) {
	got := Apply(sessions(), Filter{IDs: []string{"bbb"}})
	if len(got) != 1 || got[0].ID != "bbb222" {
		t.Fatalf("Apply() = %v, want the bbb session by prefix", IDs(got))
	}
}

func TestFiltersAreANDed(t *testing.T) {
	got := Apply(sessions(), Filter{Tools: []string{capture.ToolClaude}, RepoRoot: "/repo", EditsOnly: true})
	if len(got) != 1 || got[0].ID != "aaa111" {
		t.Fatalf("Apply() = %v, want only the claude session in /repo with edits", IDs(got))
	}
}

func TestTouchedFilesRequiresEveryPath(t *testing.T) {
	if got := Apply(sessions(), Filter{TouchedFiles: []string{"a.go", "b.go"}}); len(got) != 1 || got[0].ID != "aaa111" {
		t.Fatalf("Apply() = %v, want the session touching both files", IDs(got))
	}
	if got := Apply(sessions(), Filter{TouchedFiles: []string{"a.go", "missing.go"}}); len(got) != 0 {
		t.Fatalf("Apply() = %v, want none - every listed path must match", IDs(got))
	}
}

func TestFilesUnionsSelection(t *testing.T) {
	got := Files(Apply(sessions(), Filter{RepoRoot: "/repo"}))
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Fatalf("Files() = %v, want the deduplicated union", got)
	}
}

func TestLimitAppliesAfterOrdering(t *testing.T) {
	got := Apply(sessions(), Filter{Limit: 2})
	if len(got) != 2 || got[0].ID != "aaa111" || got[1].ID != "bbb222" {
		t.Fatalf("Apply() = %v, want the two most recent", IDs(got))
	}
}
