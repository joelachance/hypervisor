// Package parse normalizes per-tool agent session logs into the
// capture.SessionEvent stream the rest of hv works in.
//
// Each supported tool writes JSONL with its own shape; a Parser is responsible
// for one tool and for exactly one thing: turning a line into zero or more
// normalized events. Nothing downstream should ever branch on Tool again.
package parse

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/satoricorp/demux/capture"
)

// Parser reads one tool's session log format.
type Parser interface {
	// Tool is the capture.Tool* constant this parser handles.
	Tool() string
	// Roots are the directories this tool writes session logs under, absolute
	// and with ~ already expanded. Missing directories are not an error.
	Roots() []string
	// Owns reports whether the parser recognizes a log file by path.
	Owns(path string) bool
	// ParseLine converts one JSONL line into normalized events. A line the
	// parser has no event for yields nil, nil - that is the common case, not
	// a failure.
	ParseLine(path string, line []byte) ([]capture.SessionEvent, error)
}

// Registry dispatches log files to the parser that owns them.
type Registry struct {
	parsers []Parser
}

// NewRegistry returns a registry over the given parsers.
func NewRegistry(parsers ...Parser) *Registry {
	return &Registry{parsers: parsers}
}

// Default returns a registry with every parser hv ships.
func Default() *Registry {
	return NewRegistry(NewClaude(), NewCodex(), NewCursor())
}

// Parsers returns the registered parsers.
func (r *Registry) Parsers() []Parser {
	return append([]Parser(nil), r.parsers...)
}

// Roots returns every directory any registered parser watches, deduplicated.
func (r *Registry) Roots() []string {
	seen := map[string]struct{}{}
	roots := []string{}
	for _, parser := range r.parsers {
		for _, root := range parser.Roots() {
			root = strings.TrimSpace(root)
			if root == "" {
				continue
			}
			if _, ok := seen[root]; ok {
				continue
			}
			seen[root] = struct{}{}
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	return roots
}

// For returns the parser owning a log file path.
func (r *Registry) For(path string) (Parser, bool) {
	for _, parser := range r.parsers {
		if parser.Owns(path) {
			return parser, true
		}
	}
	return nil, false
}

// File parses a whole session log, in order. Lines the owning parser does not
// recognize are skipped; a malformed line is skipped with its error collected
// rather than abandoning the file, because a truncated tail is normal for a
// log that is still being written.
func (r *Registry) File(path string) ([]capture.SessionEvent, []error, error) {
	parser, ok := r.For(path)
	if !ok {
		return nil, nil, fmt.Errorf("no parser owns %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	return ParseReader(parser, path, file)
}

// ParseReader parses an open log stream with a specific parser.
func ParseReader(parser Parser, path string, reader io.Reader) ([]capture.SessionEvent, []error, error) {
	events := []capture.SessionEvent{}
	softErrors := []error{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		parsed, err := parser.ParseLine(path, line)
		if err != nil {
			softErrors = append(softErrors, err)
			continue
		}
		events = append(events, parsed...)
	}
	if err := scanner.Err(); err != nil {
		return events, softErrors, err
	}
	return events, softErrors, nil
}

// maxLogLineBytes caps one JSONL record. Agent logs embed whole file contents
// in edit events, so the default bufio limit is far too small.
const maxLogLineBytes = 16 * 1024 * 1024

// rawLine is the shared envelope decode every parser starts from.
func rawLine(line []byte) (map[string]json.RawMessage, error) {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("malformed session log line: %w", err)
	}
	return raw, nil
}

// home expands a path relative to the user's home directory. An unresolvable
// home yields an empty root, which the walker treats as absent.
func home(parts ...string) string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{dir}, parts...)...)
}
