package parse

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/satoricorp/demux/capture"
)

// Cursor parses Cursor agent transcripts.
//
// Cursor keeps two stores. Chats under ~/.cursor/chats are SQLite, which hv
// does not open; agent transcripts under
// ~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl are JSONL and are
// what this parser reads.
//
// Those transcripts are conversation only: {role, message} records with no
// timestamps, no tool calls, and no file paths. So a Cursor session registers -
// hv knows it exists, which repo it belongs to (from the project slug), and
// when it was last written (from the file) - but contributes no edit evidence.
// Selecting one scopes nothing, and demux plans its repo's tree unattributed.
type Cursor struct{ roots []string }

// NewCursor returns the Cursor parser.
func NewCursor() *Cursor {
	return &Cursor{roots: []string{home(".cursor", "projects")}}
}

// Tool implements Parser.
func (c *Cursor) Tool() string { return capture.ToolCursor }

// Roots implements Parser.
func (c *Cursor) Roots() []string { return append([]string(nil), c.roots...) }

// Owns implements Parser.
func (c *Cursor) Owns(path string) bool {
	return ownedByRoot(path, c.roots, ".jsonl") && strings.Contains(path, "agent-transcripts")
}

type cursorRecord struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Message json.RawMessage `json:"message"`
}

// Untimed implements the Untimed interface: Cursor transcripts carry no
// timestamps at all, so the log file's modification time is the only clock
// available for them.
func (c *Cursor) Untimed() bool { return true }

// ParseLine implements Parser.
func (c *Cursor) ParseLine(path string, line []byte) ([]capture.SessionEvent, error) {
	var record cursorRecord
	if err := json.Unmarshal(line, &record); err != nil {
		return nil, malformed(path, err)
	}
	if record.Role == "" && record.Type == "" {
		return nil, nil
	}
	event := capture.SessionEvent{
		SessionID: strings.TrimSuffix(pathBase(path), ".jsonl"),
		Tool:      capture.ToolCursor,
		Kind:      capture.KindMessage,
		Raw:       rawContext(CursorRepoRoot(path)),
	}
	// Transcripts carry no timestamp; the watcher stamps them from the log
	// file's modification time. Per-record timing needs the SQLite store.
	return []capture.SessionEvent{event}, nil
}

// CursorRepoRoot recovers the worktree from a Cursor project slug directory,
// which encodes an absolute path with separators replaced by dashes.
//
// The encoding is lossy - a real dash is indistinguishable from a separator -
// so candidates are verified against the filesystem and an unverifiable slug
// yields nothing rather than a guess.
func CursorRepoRoot(path string) string {
	slug := ""
	for dir := filepath.Dir(path); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if filepath.Base(filepath.Dir(dir)) == "projects" {
			slug = filepath.Base(dir)
			break
		}
	}
	if slug == "" {
		return ""
	}
	return verifiedPathFromSlug(slug)
}

func pathBase(path string) string { return filepath.Base(path) }
