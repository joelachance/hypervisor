package parse

import (
	"path/filepath"
	"strings"

	"github.com/satoricorp/demux/capture"
)

// Codex parses Codex CLI session logs.
//
// Codex writes rollout JSONL under ~/.codex/sessions, bucketed by date. Its
// records name the worktree directly, so no slug inversion is needed.
type Codex struct{ roots []string }

// NewCodex returns the Codex parser.
func NewCodex() *Codex {
	return &Codex{roots: []string{home(".codex", "sessions")}}
}

// Tool implements Parser.
func (c *Codex) Tool() string { return capture.ToolCodex }

// Roots implements Parser.
func (c *Codex) Roots() []string { return append([]string(nil), c.roots...) }

// Owns implements Parser.
func (c *Codex) Owns(path string) bool {
	if filepath.Ext(path) != ".jsonl" {
		return false
	}
	for _, root := range c.roots {
		if root != "" && strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ParseLine implements Parser.
func (c *Codex) ParseLine(path string, line []byte) ([]capture.SessionEvent, error) {
	raw, err := rawLine(line)
	if err != nil {
		return nil, err
	}
	_ = raw
	// TODO(hv): map Codex rollout records to capture.SessionEvent. Codex
	// applies edits as patch payloads rather than old/new text pairs, so
	// KindEdit needs the patch reconstructed into OldText/NewText - or the
	// matcher taught to accept a patch directly.
	return nil, nil
}
