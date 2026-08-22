package parse

import (
	"strings"

	"github.com/satoricorp/demux/capture"
)

// Cursor parses Cursor session logs.
//
// Cursor stores conversation state in a SQLite workspace database rather than
// JSONL, so this parser is the odd one out: Roots points at the workspace
// storage directory and the reader will need to be database-aware. It is
// registered now so the seam exists; it recognizes nothing yet.
type Cursor struct{ roots []string }

// NewCursor returns the Cursor parser.
func NewCursor() *Cursor {
	return &Cursor{roots: []string{home("Library", "Application Support", "Cursor", "User", "workspaceStorage")}}
}

// Tool implements Parser.
func (c *Cursor) Tool() string { return capture.ToolCursor }

// Roots implements Parser.
func (c *Cursor) Roots() []string { return append([]string(nil), c.roots...) }

// Owns implements Parser.
func (c *Cursor) Owns(path string) bool {
	// TODO(hv): Cursor's store is state.vscdb, a SQLite file. Owning it here
	// would promise a line-oriented ParseLine that cannot exist; claim it only
	// once the reader can open a database.
	_ = strings.TrimSpace(path)
	return false
}

// ParseLine implements Parser.
func (c *Cursor) ParseLine(string, []byte) ([]capture.SessionEvent, error) {
	return nil, nil
}
