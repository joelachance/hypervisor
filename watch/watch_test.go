package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satoricorp/demux/capture"
	"github.com/satoricorp/hv/parse"
)

// lineParser emits one edit event per line, with the line body as the file
// path, so a test can assert exactly which appended lines were consumed.
type lineParser struct{ root string }

func (l lineParser) Tool() string    { return capture.ToolClaude }
func (l lineParser) Roots() []string { return []string{l.root} }
func (l lineParser) Owns(p string) bool {
	return strings.HasPrefix(p, l.root) && filepath.Ext(p) == ".jsonl"
}
func (l lineParser) ParseLine(_ string, line []byte) ([]capture.SessionEvent, error) {
	return []capture.SessionEvent{{
		SessionID: "s1",
		Tool:      capture.ToolClaude,
		TS:        1,
		Kind:      capture.KindEdit,
		FilePath:  strings.TrimSpace(string(line)),
	}}, nil
}

func TestScanReadsOnlyAppendedLines(t *testing.T) {
	root := t.TempDir()
	log := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(log, []byte("a.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	watcher := New(parse.NewRegistry(lineParser{root: root}), 0)

	first, err := watcher.Scan()
	if err != nil {
		t.Fatalf("first Scan() error = %v", err)
	}
	if len(first) != 1 || first[0].FilePath != "a.go" {
		t.Fatalf("first sweep = %v, want the whole file", first)
	}

	if second, err := watcher.Scan(); err != nil || len(second) != 0 {
		t.Fatalf("unchanged sweep = %v, %v; want no events", second, err)
	}

	file, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("b.go\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()

	third, err := watcher.Scan()
	if err != nil {
		t.Fatalf("third Scan() error = %v", err)
	}
	if len(third) != 1 || third[0].FilePath != "b.go" {
		t.Fatalf("appended sweep = %v, want only the new line", third)
	}

	indexed, ok := watcher.Get("s1")
	if !ok || len(indexed.Files) != 2 {
		t.Fatalf("indexed session = %v, %v; want both files folded in", indexed, ok)
	}
}

func TestScanRereadsTruncatedLog(t *testing.T) {
	root := t.TempDir()
	log := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(log, []byte("a.go\nb.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	watcher := New(parse.NewRegistry(lineParser{root: root}), 0)
	if _, err := watcher.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte("c.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := watcher.Scan()
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(events) != 1 || events[0].FilePath != "c.go" {
		t.Fatalf("after truncation = %v, want the rewritten file re-read from zero", events)
	}
}

func TestScanToleratesMissingRoot(t *testing.T) {
	watcher := New(parse.NewRegistry(lineParser{root: filepath.Join(t.TempDir(), "absent")}), 0)
	if _, err := watcher.Scan(); err != nil {
		t.Fatalf("Scan() over a missing root = %v, want no error", err)
	}
}
