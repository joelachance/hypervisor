package selection

import (
	"io"
	"strings"
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

func TestParseSelectionInput(t *testing.T) {
	list := sessions()
	cases := []struct {
		input string
		want  []string
	}{
		{"1", []string{"aaa111"}},
		{"1,3", []string{"aaa111", "ccc333"}},
		{"1 2", []string{"aaa111", "bbb222"}},
		{"1-3", []string{"aaa111", "bbb222", "ccc333"}},
		{"all", []string{"aaa111", "bbb222", "ccc333"}},
		{"2,2", []string{"bbb222"}},
		{"", nil},
	}
	for _, testCase := range cases {
		got, err := Parse(list, testCase.input)
		if err != nil {
			t.Errorf("Parse(%q) error = %v", testCase.input, err)
			continue
		}
		if strings.Join(IDs(got), ",") != strings.Join(testCase.want, ",") {
			t.Errorf("Parse(%q) = %v, want %v", testCase.input, IDs(got), testCase.want)
		}
	}
}

func TestParseRejectsOutOfRange(t *testing.T) {
	for _, input := range []string{"0", "4", "x", "3-1"} {
		if _, err := Parse(sessions(), input); err == nil {
			t.Errorf("Parse(%q) accepted an invalid selection", input)
		}
	}
}

func TestInteractiveReadsAChoice(t *testing.T) {
	var out strings.Builder
	got, err := Interactive(strings.NewReader("2\n"), &out, sessions(), 0)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "bbb222" {
		t.Fatalf("Interactive() = %v, want the second listed session", IDs(got))
	}
	if !strings.Contains(out.String(), "select sessions to merge") {
		t.Error("Interactive() did not prompt")
	}
}

func TestInteractiveEmptyLineSelectsNothing(t *testing.T) {
	got, err := Interactive(strings.NewReader("\n"), io.Discard, sessions(), 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("Interactive() = %v, %v; want an empty selection and no error", got, err)
	}
}

func TestTouchedUnderMatchesFilesNotCWD(t *testing.T) {
	list := []session.Session{{
		ID: "s1", RepoRoot: "/elsewhere", EditCount: 1,
		Files: []string{"/repo/internal/a.go"},
	}}
	if got := Apply(list, Filter{TouchedUnder: "/repo"}); len(got) != 1 {
		t.Fatal("a session editing into /repo from another cwd must still match")
	}
	if got := Apply(list, Filter{TouchedUnder: "/other"}); len(got) != 0 {
		t.Fatal("TouchedUnder matched a directory the session never edited")
	}
}
