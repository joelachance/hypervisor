package gitx

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func blob(t *testing.T, dir, body string) string {
	t.Helper()
	id, err := HashObject(context.Background(), dir, body)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The merged text is the file's bytes: no newline appended, no whitespace
// trimmed. A conflicted file used to come back altered even where nothing
// conflicted.
func TestMergeFileKeepsBytes(t *testing.T) {
	dir := repo(t)
	base := blob(t, dir, "\n\nhead\n\n\ntail")
	got, err := MergeFile(context.Background(), dir, base, base, base, [3]string{"base", "ours", "theirs"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "\n\nhead\n\n\ntail" {
		t.Fatalf("merged = %q, want the bytes unchanged", got)
	}
}

func TestMergeFileRefusesBinary(t *testing.T) {
	dir := repo(t)
	text := blob(t, dir, "text\n")
	binary := blob(t, dir, "bin\x00ary")
	_, err := MergeFile(context.Background(), dir, text, binary, text, [3]string{"base", "ours", "theirs"})
	if !errors.Is(err, ErrBinary) {
		t.Fatalf("MergeFile() error = %v, want ErrBinary", err)
	}
}

// A Markdown underline is content, not the separator; it used to vanish.
func TestSetextUnderlineSurvivesARegion(t *testing.T) {
	dir := repo(t)
	base := blob(t, dir, "x\n")
	ours := blob(t, dir, "Title\n=======\n")
	theirs := blob(t, dir, "Other\n-------\n")
	merged, err := MergeFile(context.Background(), dir, base, ours, theirs, [3]string{"base", "ours", "theirs"})
	if err != nil {
		t.Fatal(err)
	}
	regions := ParseRegions(merged)
	if len(regions) != 1 || strings.Join(regions[0].Ours, "|") != "Title|=======" {
		t.Fatalf("regions = %+v, want the underline kept on ours", regions)
	}
	if got := ConflictedLines(merged); got != 4 {
		t.Fatalf("ConflictedLines() = %d, want 4", got)
	}
	resolved, err := ReplaceRegions(merged, [][]string{{"Title", "======="}})
	if err != nil || resolved != "Title\n=======\n" {
		t.Fatalf("ReplaceRegions() = %q, %v", resolved, err)
	}
}

func TestConflictedLinesReadsAnyMarkerWidth(t *testing.T) {
	seven := "a\n<<<<<<< ours\nA\nA2\n||||||| base\nO\n=======\nB\n>>>>>>> theirs\nb\n"
	if got := ConflictedLines(seven); got != 3 {
		t.Fatalf("ConflictedLines() = %d, want 3", got)
	}
	if !HasMarkers(seven) || HasMarkers("a\n<<< not a marker\n") {
		t.Fatal("HasMarkers misread a marker")
	}
}
