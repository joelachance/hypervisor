package parse

import (
	"strings"
	"testing"

	"github.com/satoricorp/demux/capture"
)

type fakeParser struct {
	tool   string
	events []capture.SessionEvent
	err    error
}

func (f fakeParser) Tool() string       { return f.tool }
func (f fakeParser) Roots() []string    { return []string{"/tmp/" + f.tool} }
func (f fakeParser) Owns(p string) bool { return strings.Contains(p, f.tool) }
func (f fakeParser) ParseLine(string, []byte) ([]capture.SessionEvent, error) {
	return f.events, f.err
}

func TestRegistryForDispatchesByOwnership(t *testing.T) {
	registry := NewRegistry(fakeParser{tool: "claude"}, fakeParser{tool: "codex"})
	parser, ok := registry.For("/tmp/codex/x.jsonl")
	if !ok || parser.Tool() != "codex" {
		t.Fatalf("For() = %v, %v; want the codex parser", parser, ok)
	}
	if _, ok := registry.For("/tmp/other/x.jsonl"); ok {
		t.Fatal("For() claimed an unowned path")
	}
}

func TestParseReaderSkipsBlankAndMalformedLines(t *testing.T) {
	parser := fakeParser{tool: "claude", events: []capture.SessionEvent{{SessionID: "s1"}}}
	events, soft, err := ParseReader(parser, "/tmp/claude/x.jsonl", strings.NewReader("{}\n\n{}\n"))
	if err != nil {
		t.Fatalf("ParseReader() error = %v", err)
	}
	if len(soft) != 0 {
		t.Fatalf("soft errors = %v, want none", soft)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (the blank line skipped)", len(events))
	}
}

func TestDefaultRegistryCoversEveryTool(t *testing.T) {
	tools := map[string]bool{}
	for _, parser := range Default().Parsers() {
		tools[parser.Tool()] = true
	}
	for _, want := range []string{capture.ToolClaude, capture.ToolCodex, capture.ToolCursor} {
		if !tools[want] {
			t.Errorf("default registry is missing a parser for %q", want)
		}
	}
}
